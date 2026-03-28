package services

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/proptrack/proptrack/internal/config"
	"github.com/proptrack/proptrack/internal/db"
)

// SanctionsUpdater periodically fetches public sanctions lists and cross-checks
// them against the vessel and company nodes in Dgraph.
type SanctionsUpdater struct {
	cfg    *config.Config
	client *http.Client
}

// NewSanctionsUpdater creates a SanctionsUpdater with a 60-second HTTP timeout.
func NewSanctionsUpdater(cfg *config.Config) *SanctionsUpdater {
	return &SanctionsUpdater{
		cfg:    cfg,
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// Start launches all enabled sanctions pollers. Blocks until ctx is cancelled.
func (u *SanctionsUpdater) Start(ctx context.Context) {
	slog.Info("sanctions updater starting",
		"ofac", u.cfg.OFACEnabled,
		"un", u.cfg.UNSanctionsEnabled,
		"eu", u.cfg.EUSanctionsEnabled,
	)

	// Run immediately on startup, then on interval
	if u.cfg.OFACEnabled {
		go u.runLoop(ctx, "OFAC_SDN", u.cfg.OFACUpdateInterval, u.runOFAC)
	}
	if u.cfg.UNSanctionsEnabled {
		go u.runLoop(ctx, "UN_SC", u.cfg.UNUpdateInterval, u.runUN)
	}
	if u.cfg.EUSanctionsEnabled {
		go u.runLoop(ctx, "EU_RELEX", u.cfg.EUUpdateInterval, u.runEU)
	}

	<-ctx.Done()
	slog.Info("sanctions updater stopped")
}

func (u *SanctionsUpdater) runLoop(ctx context.Context, name string, interval time.Duration, fn func(context.Context) error) {
	run := func() {
		slog.Info("sanctions update starting", "source", name)
		start := time.Now()
		if err := fn(ctx); err != nil {
			slog.Error("sanctions update failed", "source", name, "err", err)
		} else {
			slog.Info("sanctions update complete", "source", name, "elapsed", time.Since(start).Round(time.Second))
		}
	}

	run() // immediate first run
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			run()
		case <-ctx.Done():
			return
		}
	}
}

// ══════════════════════════════════════════════════════════════════════════════
// OFAC SDN XML Parser
// Source: https://www.treasury.gov/ofac/downloads/sdn.xml (public, no auth)
// Updated: variable, typically multiple times per week
// ══════════════════════════════════════════════════════════════════════════════

// ofacSDNList is the root XML element.
type ofacSDNList struct {
	XMLName xml.Name       `xml:"sdnList"`
	Entries []ofacSDNEntry `xml:"sdnEntry"`
}

type ofacSDNEntry struct {
	UID      string         `xml:"uid"`
	LastName string         `xml:"lastName"`
	SDNType  string         `xml:"sdnType"`       // "Individual", "Entity", "Vessel", "Aircraft"
	Programs []string       `xml:"programList>program"`
	AKAs     []ofacAKA      `xml:"akaList>aka"`
	IDs      []ofacID       `xml:"idList>id"`
	Vessel   *ofacVesselInfo `xml:"vesselInfo"`
}

type ofacAKA struct {
	Type     string `xml:"type"`     // "a.k.a.", "f.k.a.", "n.k.a."
	LastName string `xml:"lastName"`
}

type ofacID struct {
	IDType   string `xml:"idType"`   // "IMO number", "MMSI", "Vessel Flag", etc.
	IDNumber string `xml:"idNumber"`
	Country  string `xml:"idCountry"`
}

type ofacVesselInfo struct {
	CallSign   string `xml:"callSign"`
	VesselType string `xml:"vesselType"`
	VesselFlag string `xml:"vesselFlag"`
	GrossReg   string `xml:"grossRegisteredTonnage"`
}

func (u *SanctionsUpdater) runOFAC(ctx context.Context) error {
	body, err := u.fetch(u.cfg.OFACSDNURL, "")
	if err != nil {
		return fmt.Errorf("OFAC fetch: %w", err)
	}
	defer body.Close()

	var sdnList ofacSDNList
	if err := xml.NewDecoder(body).Decode(&sdnList); err != nil {
		return fmt.Errorf("OFAC XML decode: %w", err)
	}

	processed, matched := 0, 0
	for _, entry := range sdnList.Entries {
		// Only process vessel entries or entities with IMO numbers
		imoNumbers := extractIMOs(entry.IDs)
		if entry.SDNType != "Vessel" && entry.SDNType != "Entity" {
			continue
		}
		if entry.SDNType == "Entity" && len(imoNumbers) == 0 {
			continue // skip non-vessel entities (individuals, etc.)
		}

		programs := strings.Join(entry.Programs, " ")
		aliases := buildAliasString(entry.AKAs)

		for _, imo := range imoNumbers {
			if err := u.upsertSanctionedVessel(ctx, imo, entry.LastName, "OFAC_SDN", programs, aliases, entry.UID); err != nil {
				slog.Warn("OFAC vessel upsert failed", "imo", imo, "err", err)
				continue
			}
			matched++
		}

		// Also check if the entity name matches any company in our DB
		if len(imoNumbers) == 0 {
			if err := u.upsertSanctionedCompany(ctx, entry.LastName, aliases, "OFAC_SDN", programs, entry.UID); err != nil {
				slog.Debug("OFAC company upsert skipped", "name", entry.LastName, "err", err)
			}
		}
		processed++
	}

	slog.Info("OFAC SDN processed", "entries", processed, "vessels_matched", matched)
	return nil
}

// extractIMOs scans an OFAC idList for entries with idType "IMO number" or "MMSI".
func extractIMOs(ids []ofacID) []string {
	var imos []string
	for _, id := range ids {
		t := strings.ToLower(id.IDType)
		if strings.Contains(t, "imo") {
			imos = append(imos, strings.TrimSpace(id.IDNumber))
		}
	}
	return imos
}

func buildAliasString(akas []ofacAKA) string {
	parts := make([]string, 0, len(akas))
	for _, a := range akas {
		if a.LastName != "" {
			parts = append(parts, a.LastName)
		}
	}
	return strings.Join(parts, ", ")
}

// ══════════════════════════════════════════════════════════════════════════════
// UN Security Council Consolidated List
// Source: https://scsanctions.un.org/resources/xml/en/consolidated.xml (public)
// Updated: 2-3 times per week
// ══════════════════════════════════════════════════════════════════════════════

// unConsolidatedList is the root element of the UN SC XML list.
type unConsolidatedList struct {
	XMLName    xml.Name      `xml:"CONSOLIDATED_LIST"`
	Entities   []unEntity    `xml:"ENTITIES>ENTITY"`
	Individuals []unIndividual `xml:"INDIVIDUALS>INDIVIDUAL"`
}

type unEntity struct {
	DataID       string        `xml:"DATAID,attr"`
	NameOriginal string        `xml:"FIRST_NAME"`
	NameAlias    []unNameAlias `xml:"ENTITY_ALIAS"`
	Comments     string        `xml:"COMMENTS1"`
	Documents    []unDocument  `xml:"INDIVIDUAL_DOCUMENT"` // reused structure
}

// unNameAlias covers both entity aliases and individual aliases.
type unNameAlias struct {
	Quality   string `xml:"QUALITY,attr"` // "Good", "Low"
	AliasName string `xml:"ALIAS_NAME"`
}

type unIndividual struct {
	DataID     string        `xml:"DATAID,attr"`
	FirstName  string        `xml:"FIRST_NAME"`
	LastName   string        `xml:"SECOND_NAME"`
	Aliases    []unNameAlias `xml:"INDIVIDUAL_ALIAS"`
	Documents  []unDocument  `xml:"INDIVIDUAL_DOCUMENT"`
	Comments   string        `xml:"COMMENTS1"`
}

type unDocument struct {
	TypeID   string `xml:"TYPE_ID,attr"`
	TypeDesc string `xml:"TYPE_DESCRIPTION"`
	Number   string `xml:"NUMBER"`
	Country  string `xml:"ISSUING_COUNTRY"`
}

func (u *SanctionsUpdater) runUN(ctx context.Context) error {
	body, err := u.fetch(u.cfg.UNSanctionsURL, "")
	if err != nil {
		return fmt.Errorf("UN fetch: %w", err)
	}
	defer body.Close()

	var list unConsolidatedList
	if err := xml.NewDecoder(body).Decode(&list); err != nil {
		return fmt.Errorf("UN XML decode: %w", err)
	}

	matched := 0
	for _, entity := range list.Entities {
		// Look for IMO numbers in document list
		for _, doc := range entity.Documents {
			if !isIMODocType(doc.TypeDesc, doc.TypeID) {
				continue
			}
			imo := strings.TrimSpace(doc.Number)
			if imo == "" {
				continue
			}
			name := entityPrimaryName(entity.NameAlias, entity.NameOriginal, "")
			if err := u.upsertSanctionedVessel(ctx, imo, name, "UN_1929", "UN", "", entity.DataID); err != nil {
				slog.Warn("UN vessel upsert failed", "imo", imo, "err", err)
				continue
			}
			matched++
		}
	}

	slog.Info("UN SC list processed",
		"entities", len(list.Entities),
		"individuals", len(list.Individuals),
		"vessels_matched", matched,
	)
	return nil
}

func isIMODocType(desc, id string) bool {
	d := strings.ToLower(desc)
	return strings.Contains(d, "imo") || id == "IMO"
}

func entityPrimaryName(aliases []unNameAlias, fallback1, fallback2 string) string {
	for _, a := range aliases {
		if a.AliasName != "" {
			return a.AliasName
		}
	}
	if fallback1 != "" {
		return fallback1
	}
	return fallback2
}

// ══════════════════════════════════════════════════════════════════════════════
// EU RELEX Financial Sanctions (optional — requires registration token)
// Source: https://webgate.ec.europa.eu/.../xmlFullSanctionsList_1_1/content
// Updated: daily
// ══════════════════════════════════════════════════════════════════════════════

type euSanctionsList struct {
	XMLName xml.Name    `xml:"export"`
	Entries []euSubject `xml:"sanctionEntity"`
}

type euSubject struct {
	LogicalID   string          `xml:"logicalId,attr"`
	SubjectType string          `xml:"subjectType,attr"` // "person", "entity", "vessel"
	NameAlias   []euNameAlias   `xml:"nameAlias"`
	Identification []euID       `xml:"identification"`
	Regulation  []euRegulation  `xml:"regulation"`
}

type euNameAlias struct {
	FirstName  string `xml:"firstName,attr"`
	LastName   string `xml:"lastName,attr"`
	WholeName  string `xml:"wholeName,attr"`
}

type euID struct {
	Type   string `xml:"identificationTypeCode,attr"` // "IMO", "MMSI", etc.
	Number string `xml:"number,attr"`
}

type euRegulation struct {
	Programme string `xml:"programme,attr"`
}

func (u *SanctionsUpdater) runEU(ctx context.Context) error {
	url := u.cfg.EUSanctionsURL
	if u.cfg.EUSanctionsToken != "" {
		url = url + "?token=" + u.cfg.EUSanctionsToken
	}

	body, err := u.fetch(url, "")
	if err != nil {
		return fmt.Errorf("EU fetch: %w", err)
	}
	defer body.Close()

	var list euSanctionsList
	if err := xml.NewDecoder(body).Decode(&list); err != nil {
		return fmt.Errorf("EU XML decode: %w", err)
	}

	matched := 0
	for _, subject := range list.Entries {
		if subject.SubjectType != "vessel" && subject.SubjectType != "entity" {
			continue
		}
		programs := euPrograms(subject.Regulation)
		name := euPrimaryName(subject.NameAlias)

		for _, id := range subject.Identification {
			if strings.ToUpper(id.Type) != "IMO" {
				continue
			}
			imo := strings.TrimSpace(id.Number)
			if imo == "" {
				continue
			}
			if err := u.upsertSanctionedVessel(ctx, imo, name, "EU", programs, "", subject.LogicalID); err != nil {
				slog.Warn("EU vessel upsert failed", "imo", imo, "err", err)
				continue
			}
			matched++
		}
	}

	slog.Info("EU RELEX list processed",
		"entries", len(list.Entries),
		"vessels_matched", matched,
	)
	return nil
}

func euPrograms(regs []euRegulation) string {
	parts := make([]string, 0, len(regs))
	for _, r := range regs {
		if r.Programme != "" {
			parts = append(parts, r.Programme)
		}
	}
	return strings.Join(parts, " ")
}

func euPrimaryName(aliases []euNameAlias) string {
	for _, a := range aliases {
		if a.WholeName != "" {
			return a.WholeName
		}
		if a.LastName != "" {
			return a.LastName
		}
	}
	return ""
}

// ══════════════════════════════════════════════════════════════════════════════
// Dgraph upsert helpers
// ══════════════════════════════════════════════════════════════════════════════

// upsertSanctionedVessel finds a vessel by IMO and marks it as sanctioned.
// If the vessel doesn't exist yet, it creates a minimal placeholder node
// so the sanction flag is recorded for when the vessel is later added via AIS.
func (u *SanctionsUpdater) upsertSanctionedVessel(
	ctx context.Context,
	imo, name, list, programs, aliases, sourceID string,
) error {
	q := `query {
  vessel as var(func: eq(vessel.imo, "` + imo + `"))
}`

	type vesselUpdate struct {
		UID              string   `json:"uid"`
		DgraphType       []string `json:"dgraph.type"`
		IMO              string   `json:"vessel.imo"`
		Name             string   `json:"vessel.name,omitempty"`
		Sanctioned       bool     `json:"vessel.sanctioned"`
		SanctionPrograms string   `json:"vessel.sanction_programs,omitempty"`
	}

	updateNode := vesselUpdate{
		UID:              "uid(vessel)",
		DgraphType:       []string{"Vessel"},
		IMO:              imo,
		Sanctioned:       true,
		SanctionPrograms: programs,
	}
	// Only set name if vessel doesn't exist yet (insert path)
	insertNode := updateNode
	insertNode.UID = "_:vessel"
	if name != "" {
		insertNode.Name = name
		updateNode.Name = name
	}

	bUpdate, _ := json.Marshal(updateNode)
	bInsert, _ := json.Marshal(insertNode)

	req := &api.Request{
		Query: q,
		Mutations: []*api.Mutation{
			{SetJson: bUpdate, Cond: `@if(gt(len(vessel), 0))`},
			{SetJson: bInsert, Cond: `@if(eq(len(vessel), 0))`},
		},
		CommitNow: true,
	}

	txn := db.NewTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Do(ctx, req)
	if err != nil {
		return err
	}

	// Determine UID (existing or newly created)
	vesselUID := ""
	for _, uid := range resp.Uids {
		vesselUID = uid
		break
	}
	if vesselUID == "" {
		// Vessel existed — resolve its UID for the sanction entry
		vesselUID, err = resolveVesselIMOUID(ctx, imo)
		if err != nil || vesselUID == "" {
			return nil // not critical
		}
	}

	// Write SanctionEntry node linked to this vessel
	type sanctionNode struct {
		UID        string    `json:"uid"`
		DgraphType []string  `json:"dgraph.type"`
		List       string    `json:"sanction.list"`
		EntityType string    `json:"sanction.entity_type"`
		EntityRef  string    `json:"sanction.entity_ref"`
		DateListed time.Time `json:"sanction.date_listed"`
		Program    string    `json:"sanction.program,omitempty"`
		Notes      string    `json:"sanction.notes,omitempty"`
	}
	sn := sanctionNode{
		UID:        "_:sanction",
		DgraphType: []string{"SanctionEntry"},
		List:       list,
		EntityType: "vessel",
		EntityRef:  vesselUID,
		DateListed: time.Now().UTC(),
		Program:    programs,
		Notes:      fmt.Sprintf("source_id=%s aliases=%s", sourceID, aliases),
	}
	sb, _ := json.Marshal(sn)

	txn2 := db.NewTxn()
	defer txn2.Discard(ctx)
	_, err = txn2.Mutate(ctx, &api.Mutation{SetJson: sb, CommitNow: true})
	return err
}

// upsertSanctionedCompany marks a company as sanctioned if it exists in Dgraph.
func (u *SanctionsUpdater) upsertSanctionedCompany(
	ctx context.Context,
	name, aliases, list, programs, sourceID string,
) error {
	// Fuzzy name match using Dgraph term index
	q := `query q($name: string) {
  company(func: anyofterms(company.name, $name)) {
    uid
    company.name
  }
}`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$name": name})
	if err != nil {
		return err
	}
	var res struct {
		Company []struct {
			UID  string `json:"uid"`
			Name string `json:"company.name"`
		} `json:"company"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil || len(res.Company) == 0 {
		return nil // company not in our DB, skip
	}

	for _, co := range res.Company {
		patch := map[string]interface{}{
			"uid":               co.UID,
			"company.sanctioned": true,
			"company.ofac_id":   sourceID,
		}
		if aliases != "" {
			patch["company.known_aliases"] = aliases
		}
		b, _ := json.Marshal(patch)
		txn2 := db.NewTxn()
		if _, err := txn2.Mutate(ctx, &api.Mutation{SetJson: b, CommitNow: true}); err != nil {
			txn2.Discard(ctx)
			slog.Warn("company sanction patch failed", "uid", co.UID, "err", err)
		}
		slog.Info("company marked sanctioned from list",
			"source", list,
			"company", co.Name,
			"uid", co.UID,
		)
	}
	return nil
}

// resolveVesselIMOUID is a local helper (duplicated from seed to avoid import cycle).
func resolveVesselIMOUID(ctx context.Context, imo string) (string, error) {
	q := `query q($imo: string) { vessel(func: eq(vessel.imo, $imo)) { uid } }`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)
	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$imo": imo})
	if err != nil {
		return "", err
	}
	var res struct {
		Vessel []struct{ UID string `json:"uid"` } `json:"vessel"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil || len(res.Vessel) == 0 {
		return "", nil
	}
	return res.Vessel[0].UID, nil
}

// fetch performs an HTTP GET and returns the response body.
// The caller must close the returned ReadCloser.
func (u *SanctionsUpdater) fetch(url, token string) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "PropTrack-OSINT/1.0 (counter-proliferation research)")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return resp.Body, nil
}
