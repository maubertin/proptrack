package services

// datalastic_enricher.go — vessel discovery and enrichment via the Datalastic AIS API.
//
// Endpoints used:
//   GET /vessel_find  — discover vessels by flag country (high-risk flags)
//   GET /vessel_pro   — enrich known active vessels with current destination/ETA/tonnage
//
// Auth:    ?api-key=<DATALASTIC_API_KEY>  (query parameter)
// Pricing: credit-based; vessel_find ~1 cr/req, vessel_pro ~1 cr/req.
//          14-day trial starts at 9 EUR — https://datalastic.com/pricing/
//
// Two complementary discovery strategies:
//   1. discoverByFlag  — scan for vessels flying Iran shadow-fleet FOC flags
//      that are currently slow-steaming inside a watched Chinese port zone.
//   2. enrichKnownVessels — pull fresh destination/ETA/tonnage for vessels
//      linked to active or monitoring shipments in the graph.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/google/uuid"
	"github.com/proptrack/proptrack/internal/config"
	"github.com/proptrack/proptrack/internal/db"
)

// dlVesselEntry is returned by Datalastic /vessel_find.
type dlVesselEntry struct {
	UUID         string  `json:"uuid"`
	MMSI         string  `json:"mmsi"`
	IMO          string  `json:"imo"`
	Name         string  `json:"name"`
	CountryISO   string  `json:"country_iso"`    // flag country (ISO-2)
	Type         string  `json:"type"`           // vessel class (e.g. "Cargo")
	TypeSpecific string  `json:"type_specific"`  // e.g. "Bulk Carrier"
	Lat          float64 `json:"lat"`
	Lon          float64 `json:"lon"`
	Speed        float64 `json:"speed"`
	Course       float64 `json:"course"`
	Heading      float64 `json:"heading"`
	NavStatus    int     `json:"navigational_status"`
	Destination  string  `json:"destination"`
	Timestamp    string  `json:"timestamp"`
}

// dlVesselProEntry is returned by Datalastic /vessel_pro.
// Extends dlVesselEntry with ETA, actual departure time, standardised destination,
// draught, and tonnage — these cost 1 extra credit vs /vessel.
type dlVesselProEntry struct {
	dlVesselEntry
	ETA                 string  `json:"eta"`
	ATD                 string  `json:"atd"`                  // actual time of departure
	Draught             float64 `json:"draught"`
	DestinationPort     string  `json:"destination_port"`     // standardised name
	DestinationPortUNLO string  `json:"destination_port_unlo"` // UNLOCODE when available
	GrossTonnage        float64 `json:"gross_tonnage"`
	Deadweight          float64 `json:"deadweight"`
}

// dlHighRiskFlags lists flag states frequently used by Iran's shadow fleet.
// Primary: Iran (IR). FOC flags documented in UN/OFAC sanctions reports:
// Comoros (KM), Mongolia (MN), Tanzania (TZ), Tuvalu (TV), Palau (PW),
// Cameroon (CM), Bolivia (BO), Guinea-Bissau (GW), Gabon (GA).
var dlHighRiskFlags = []string{
	"IR",           // Iran — direct flag
	"KM",           // Comoros — primary IRISL shadow fleet flag (UN Panel reports)
	"MN",           // Mongolia — documented for tanker fleet evasion
	"TZ",           // Tanzania — used post-2020 sanctions wave
	"TV",           // Tuvalu — small registry, frequent shadow fleet abuse
	"PW",           // Palau — documented in OFAC designations
	"CM",           // Cameroon — used by IRISL proxies
	"BO",           // Bolivia — landlocked but has registry, used for tankers
	"GW",           // Guinea-Bissau
	"GA",           // Gabon
	"TG",           // Tonga — documented in scorer flagsOfConvenience; IRISL-linked vessels
	"SL",           // Sierra Leone — frequently used for Iranian shadow tankers
	"MH",           // Marshall Islands — large open registry; subset tied to Iran
	"BZ",           // Belize — used for Iran-linked general cargo
}

// DatalasticEnricher discovers and enriches vessel data using the Datalastic API.
type DatalasticEnricher struct {
	cfg *config.Config
}

// NewDatalasticEnricher creates a new DatalasticEnricher.
func NewDatalasticEnricher(cfg *config.Config) *DatalasticEnricher {
	return &DatalasticEnricher{cfg: cfg}
}

// Start launches the enrichment loop. Blocks until ctx is cancelled.
func (e *DatalasticEnricher) Start(ctx context.Context) {
	if !e.cfg.DatalasticEnabled {
		slog.Warn("Datalastic enricher disabled (DATALASTIC_API_KEY not set)")
		return
	}
	slog.Info("Datalastic enricher started", "interval", e.cfg.DatalasticInterval)

	e.runCycle(ctx)

	ticker := time.NewTicker(e.cfg.DatalasticInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.runCycle(ctx)
		case <-ctx.Done():
			slog.Info("Datalastic enricher stopped")
			return
		}
	}
}

func (e *DatalasticEnricher) runCycle(ctx context.Context) {
	slog.Info("Datalastic enricher: starting cycle")
	discovered := e.discoverByFlag(ctx)
	slog.Info("Datalastic enricher: flag discovery complete", "new_shipments", discovered)
	enriched := e.enrichKnownVessels(ctx)
	slog.Info("Datalastic enricher: enrichment complete", "vessels_updated", enriched)
}

// discoverByFlag queries /vessel_find for each high-risk flag and creates monitoring
// shipments for vessels that are slow-steaming inside a watched Chinese port zone.
func (e *DatalasticEnricher) discoverByFlag(ctx context.Context) int {
	created := 0
	for _, flag := range dlHighRiskFlags {
		vessels, err := e.fetchVesselsByFlag(flag)
		if err != nil {
			slog.Warn("Datalastic enricher: flag discovery failed", "flag", flag, "err", err)
			continue
		}
		slog.Debug("Datalastic enricher: flag query done", "flag", flag, "count", len(vessels))

		for _, v := range vessels {
			if v.IMO == "" || v.IMO == "0" {
				continue
			}
			zone := findZoneForPosition(v.Lat, v.Lon)
			if zone == nil || zone.Country != "CN" {
				continue
			}
			if v.Speed > 14 {
				continue
			}

			vesselUID, err := osintUpsertVessel(ctx,
				v.IMO, v.MMSI, v.Name, v.CountryISO,
				pwMapAISType(v.TypeSpecific), "datalastic",
				v.Lat, v.Lon, 0, 0, 0,
			)
			if err != nil || vesselUID == "" {
				continue
			}

			if osintHasRecentShipmentFromPort(ctx, v.IMO, zone.UNLOCODE, 60) {
				continue
			}

			destUNLOCODE := parseIranianDestinationPW(v.Destination)
			status := "monitoring"
			if destUNLOCODE != "" {
				status = "active"
			}

			originUID, err := pwResolvePort(ctx, zone.UNLOCODE)
			if err != nil || originUID == "" {
				continue
			}

			destUID := ""
			if destUNLOCODE != "" {
				destUID, _ = pwResolvePort(ctx, destUNLOCODE)
			}

			shipmentID := "DL-" + uuid.New().String()[:8]
			sourceRef := fmt.Sprintf(
				"Auto-discovered via Datalastic API (flag: %s); AIS dest: %q; speed: %.1f kn",
				flag, v.Destination, v.Speed,
			)
			if err := osintCreateShipment(ctx, shipmentID, vesselUID, originUID, destUID, status, sourceRef); err != nil {
				slog.Warn("Datalastic enricher: shipment creation failed", "imo", v.IMO, "err", err)
				continue
			}
			slog.Info("Datalastic enricher: shipment created",
				"id", shipmentID, "imo", v.IMO, "vessel", v.Name,
				"flag", flag, "zone", zone.UNLOCODE, "status", status,
			)
			created++
		}
	}
	return created
}

// enrichKnownVessels calls /vessel_pro for vessels linked to active/monitoring
// shipments, then updates position, tonnage, and confirms Iranian arrivals.
// Capped at DatalasticEnrichBatch vessels per cycle to control credit spend.
func (e *DatalasticEnricher) enrichKnownVessels(ctx context.Context) int {
	q := `{
  vessels(func: has(vessel.imo)) @cascade {
    uid
    vessel.imo
    ~shipment.vessel @filter(anyofterms(shipment.status, "active monitoring")) { uid }
  }
}`
	txn := db.NewReadOnlyTxn()
	resp, err := txn.Query(ctx, q)
	txn.Discard(ctx)
	if err != nil {
		slog.Warn("Datalastic enricher: failed to fetch active vessels", "err", err)
		return 0
	}

	var res struct {
		Vessels []struct {
			UID string `json:"uid"`
			IMO string `json:"vessel.imo"`
		} `json:"vessels"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return 0
	}

	batch := e.cfg.DatalasticEnrichBatch
	enriched := 0
	for _, vessel := range res.Vessels {
		if enriched >= batch {
			slog.Debug("Datalastic enricher: batch cap reached", "cap", batch)
			break
		}

		pro, err := e.fetchVesselPro(vessel.IMO)
		if err != nil {
			slog.Debug("Datalastic enricher: vessel_pro failed", "imo", vessel.IMO, "err", err)
			continue
		}

		// Patch position and tonnage fields.
		patch := map[string]interface{}{
			"uid":             vessel.UID,
			"vessel.last_lat": pro.Lat,
			"vessel.last_lon": pro.Lon,
		}
		if pro.Deadweight > 0 {
			patch["vessel.dwt"] = pro.Deadweight
		}
		if pro.GrossTonnage > 0 {
			patch["vessel.grt"] = pro.GrossTonnage
		}
		if pro.Draught > 0 {
			patch["vessel.draft_loaded"] = pro.Draught
		}

		b, _ := json.Marshal(patch)
		txn2 := db.NewTxn()
		if _, mutErr := txn2.Mutate(ctx, &api.Mutation{SetJson: b, CommitNow: true}); mutErr != nil {
			txn2.Discard(ctx)
			continue
		}

		// If destination became Iranian, confirm arrival on any monitoring shipment.
		destCode := strings.ToUpper(pro.DestinationPortUNLO)
		if destCode == "" {
			destCode = parseIranianDestinationPW(pro.Destination)
		}
		if destCode != "" {
			shipUID, _ := osintFindMonitoringShipment(ctx, vessel.IMO)
			if shipUID != "" {
				portUID, _ := pwResolvePort(ctx, destCode)
				if portUID != "" {
					osintConfirmIranianArrival(ctx, shipUID, portUID, vessel.IMO, pro.Name, destCode)
				}
			}
		}
		enriched++
	}
	return enriched
}

// findZoneForPosition returns the first watched port zone whose bounding box
// contains (lat, lon), or nil if none match.
func findZoneForPosition(lat, lon float64) *watchedPortZone {
	for i, zone := range watchedPortZones {
		if lat >= zone.MinLat && lat <= zone.MaxLat && lon >= zone.MinLon && lon <= zone.MaxLon {
			return &watchedPortZones[i]
		}
	}
	return nil
}

// fetchVesselsByFlag calls Datalastic /vessel_find filtered by flag (ISO-2 lowercase).
// Returns up to 100 vessels per page (page 0 only; pagination can be added later).
func (e *DatalasticEnricher) fetchVesselsByFlag(flag string) ([]dlVesselEntry, error) {
	url := fmt.Sprintf(
		"https://api.datalastic.com/api/v0/vessel_find?api-key=%s&flag=%s&page=0&size=100",
		e.cfg.DatalasticAPIKey, strings.ToLower(flag),
	)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("Datalastic request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Datalastic %d: %.200s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var result struct {
		Data []dlVesselEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return result.Data, nil
}

// fetchVesselPro calls Datalastic /vessel_pro for a single vessel by IMO.
func (e *DatalasticEnricher) fetchVesselPro(imo string) (*dlVesselProEntry, error) {
	url := fmt.Sprintf(
		"https://api.datalastic.com/api/v0/vessel_pro?api-key=%s&imo=%s",
		e.cfg.DatalasticAPIKey, imo,
	)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("Datalastic pro request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Datalastic pro %d: %.200s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var result struct {
		Data *dlVesselProEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if result.Data == nil {
		return nil, fmt.Errorf("no data returned for IMO %s", imo)
	}
	return result.Data, nil
}
