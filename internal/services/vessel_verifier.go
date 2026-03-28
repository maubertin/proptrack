package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/proptrack/proptrack/internal/config"
	"github.com/proptrack/proptrack/internal/db"
)

// ── Verification status constants ─────────────────────────────────────────────

const (
	VerifyStatusVerified    = "verified"     // all sources agree
	VerifyStatusDiscrepancy = "discrepancy"  // mismatch detected between sources
	VerifyStatusNotFound    = "not_found"    // vessel absent from external source (dark/spoofed IMO)
	VerifyStatusError       = "error"        // API call failed
	VerifyStatusPending     = "pending"      // not yet checked
)

// Discrepancy codes used as flags in the scoring engine.
const (
	DiscNameMismatch   = "name_mismatch"    // vessel name differs between sources
	DiscFlagMismatch   = "flag_mismatch"    // flag/registry changed (sanctions evasion)
	DiscPosSpoofing    = "position_spoofing" // GPS positions diverge > posGapSpoofingThresholdKm
	DiscMMSIConflict   = "mmsi_conflict"    // MMSI does not match IMO registry records
	DiscNotFound       = "source_not_found" // vessel absent from secondary source
)

// posGapSpoofingThresholdKm: position gap above which we consider potential AIS spoofing.
// 50 km is conservative — legitimate AIS data latency should not exceed ~10 km.
const posGapSpoofingThresholdKm = 50.0

// ── External source data ──────────────────────────────────────────────────────

// ExternalVesselData is the normalized result from any external source.
type ExternalVesselData struct {
	Source      string
	IMO         string
	MMSI        string
	Name        string
	Flag        string
	Lat         float64
	Lon         float64
	Speed       float64
	Destination string
	Timestamp   time.Time
	Found       bool   // false if vessel not found in source
}

// VerificationResult summarizes the cross-source check for one vessel.
type VerificationResult struct {
	IMO           string
	VerifiedAt    time.Time
	Status        string
	Sources       []string
	Discrepancies []string
	ExternalName  string
	ExternalFlag  string
	PosGapKm      float64
	Notes         string
}

// ── VesselVerifier ────────────────────────────────────────────────────────────

// VesselVerifier queries external AIS sources and cross-checks vessel identity
// and position against the data stored in Dgraph. It persists discrepancies
// back to the vessel node and adjusts the suspicion score criteria.
type VesselVerifier struct {
	cfg        *config.Config
	httpClient *http.Client
}

func NewVesselVerifier(cfg *config.Config) *VesselVerifier {
	return &VesselVerifier{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Start runs verification passes on a configurable interval.
// Each pass verifies all vessels in the DB that have an IMO number.
func (v *VesselVerifier) Start(ctx context.Context) {
	if !v.cfg.VesselVerifyEnabled {
		slog.Info("vessel verifier disabled (no API keys configured)")
		return
	}
	slog.Info("vessel verifier started",
		"interval", v.cfg.VesselVerifyInterval,
		"vf_enabled", v.cfg.VesselFinderEnabled,
		"mst_enabled", v.cfg.MyShipTrackingEnabled,
	)

	// Run once immediately, then on interval
	v.runPass(ctx)

	ticker := time.NewTicker(v.cfg.VesselVerifyInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("vessel verifier stopped")
			return
		case <-ticker.C:
			v.runPass(ctx)
		}
	}
}

// runPass fetches all tracked vessels from Dgraph and verifies each one.
func (v *VesselVerifier) runPass(ctx context.Context) {
	slog.Info("vessel verification pass started")

	imoList, err := v.fetchTrackedIMOs(ctx)
	if err != nil {
		slog.Error("failed to fetch tracked vessels for verification", "err", err)
		return
	}

	verified, discrepancies, notFound := 0, 0, 0
	for _, imo := range imoList {
		result, err := v.VerifyVessel(ctx, imo)
		if err != nil {
			slog.Warn("verification failed", "imo", imo, "err", err)
			continue
		}
		switch result.Status {
		case VerifyStatusVerified:
			verified++
		case VerifyStatusDiscrepancy:
			discrepancies++
			slog.Warn("vessel identity discrepancy detected",
				"imo", imo,
				"discrepancies", strings.Join(result.Discrepancies, ","),
				"pos_gap_km", result.PosGapKm,
				"ext_name", result.ExternalName,
				"ext_flag", result.ExternalFlag,
			)
		case VerifyStatusNotFound:
			notFound++
		}
	}
	slog.Info("vessel verification pass complete",
		"total", len(imoList),
		"verified", verified,
		"discrepancies", discrepancies,
		"not_found", notFound,
	)
}

// VerifyVessel runs a cross-source check for a single vessel by IMO.
// Exposed publicly so the HTTP handler can trigger on-demand verification.
func (v *VesselVerifier) VerifyVessel(ctx context.Context, imo string) (*VerificationResult, error) {
	// Load current vessel data from DB
	local, err := v.fetchLocalVessel(ctx, imo)
	if err != nil {
		return nil, fmt.Errorf("local fetch: %w", err)
	}

	result := &VerificationResult{
		IMO:        imo,
		VerifiedAt: time.Now().UTC(),
		Status:     VerifyStatusPending,
	}

	var externals []ExternalVesselData

	// ── Source 1: VesselFinder ────────────────────────────────────────────
	if v.cfg.VesselFinderEnabled {
		vf, err := v.queryVesselFinder(ctx, imo)
		if err != nil {
			slog.Warn("VesselFinder query failed", "imo", imo, "err", err)
		} else {
			externals = append(externals, vf)
			result.Sources = append(result.Sources, "VesselFinder")
		}
	}

	// ── Source 2: MyShipTracking ──────────────────────────────────────────
	if v.cfg.MyShipTrackingEnabled {
		mst, err := v.queryMyShipTracking(ctx, imo)
		if err != nil {
			slog.Warn("MyShipTracking query failed", "imo", imo, "err", err)
		} else {
			externals = append(externals, mst)
			result.Sources = append(result.Sources, "MyShipTracking")
		}
	}

	if len(externals) == 0 {
		result.Status = VerifyStatusError
		result.Notes = "all external sources unavailable"
		return result, nil
	}

	// ── Cross-check each external source against local data ───────────────
	discSet := map[string]bool{}
	var notes []string

	for _, ext := range externals {
		if !ext.Found {
			discSet[DiscNotFound] = true
			notes = append(notes, fmt.Sprintf("%s: vessel not found (dark or spoofed IMO)", ext.Source))
			continue
		}

		// Capture first found external data for storage
		if result.ExternalName == "" {
			result.ExternalName = ext.Name
			result.ExternalFlag = ext.Flag
		}

		// Name mismatch (normalise: uppercase, trim spaces)
		localName := strings.ToUpper(strings.TrimSpace(local.Name))
		extName := strings.ToUpper(strings.TrimSpace(ext.Name))
		if localName != "" && extName != "" && localName != extName {
			// Allow minor differences (e.g. "MV SHABDIZ" vs "SHABDIZ")
			if !strings.Contains(extName, localName) && !strings.Contains(localName, extName) {
				discSet[DiscNameMismatch] = true
				notes = append(notes, fmt.Sprintf("%s: name mismatch — local=%q ext=%q", ext.Source, local.Name, ext.Name))
			}
		}

		// Flag mismatch
		localFlag := strings.ToUpper(strings.TrimSpace(local.Flag))
		extFlag := strings.ToUpper(strings.TrimSpace(ext.Flag))
		if localFlag != "" && extFlag != "" && localFlag != extFlag {
			discSet[DiscFlagMismatch] = true
			notes = append(notes, fmt.Sprintf("%s: flag mismatch — local=%s ext=%s", ext.Source, local.Flag, ext.Flag))
		}

		// MMSI conflict
		if local.MMSI != "" && ext.MMSI != "" && local.MMSI != ext.MMSI {
			discSet[DiscMMSIConflict] = true
			notes = append(notes, fmt.Sprintf("%s: MMSI conflict — local=%s ext=%s", ext.Source, local.MMSI, ext.MMSI))
		}

		// Position gap (only meaningful if local position is known)
		if local.LastLat != 0 && local.LastLon != 0 && ext.Lat != 0 && ext.Lon != 0 {
			gap := haversineKmVV(local.LastLat, local.LastLon, ext.Lat, ext.Lon)
			if gap > result.PosGapKm {
				result.PosGapKm = gap
			}
			if gap > posGapSpoofingThresholdKm {
				discSet[DiscPosSpoofing] = true
				notes = append(notes, fmt.Sprintf("%s: position gap %.0f km > %.0f km threshold (possible AIS spoofing)", ext.Source, gap, posGapSpoofingThresholdKm))
			}
		}
	}

	// Build discrepancy list
	for d := range discSet {
		result.Discrepancies = append(result.Discrepancies, d)
	}
	result.Notes = strings.Join(notes, "; ")

	switch {
	case len(discSet) > 0:
		result.Status = VerifyStatusDiscrepancy
	case discSet[DiscNotFound]:
		result.Status = VerifyStatusNotFound
	default:
		result.Status = VerifyStatusVerified
	}

	// Persist results to Dgraph
	if err := v.persistVerification(ctx, local.UID, result); err != nil {
		slog.Warn("failed to persist verification result", "imo", imo, "err", err)
	}

	return result, nil
}

// ── VesselFinder REST API v2 ──────────────────────────────────────────────────
// Docs: https://api.vesselfinder.com/docs
// Endpoint: GET /vessels?userkey=KEY&imo=IMO
// Response: {"vessels":[{"AIS":{...},"VSL":{...}}]}

func (v *VesselVerifier) queryVesselFinder(ctx context.Context, imo string) (ExternalVesselData, error) {
	url := fmt.Sprintf("https://api.vesselfinder.com/vessels?userkey=%s&imo=%s",
		v.cfg.VesselFinderAPIKey, imo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ExternalVesselData{}, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return ExternalVesselData{}, fmt.Errorf("VesselFinder HTTP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return ExternalVesselData{}, fmt.Errorf("VesselFinder: invalid API key")
	}
	if resp.StatusCode != http.StatusOK {
		return ExternalVesselData{}, fmt.Errorf("VesselFinder: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return ExternalVesselData{}, err
	}

	var vfResp struct {
		Vessels []struct {
			AIS struct {
				MMSI      int64   `json:"MMSI"`
				Lat       float64 `json:"LAT"`
				Lon       float64 `json:"LON"`
				Speed     float64 `json:"SPEED"`
				Timestamp string  `json:"TIMESTAMP"`
			} `json:"AIS"`
			VSL struct {
				Name     string `json:"NAME"`
				IMO      int64  `json:"IMO"`
				Flag     string `json:"FLAG"`
				MMSI     int64  `json:"MMSI"`
				Callsign string `json:"CALLSIGN"`
			} `json:"VSL"`
		} `json:"vessels"`
	}

	if err := json.Unmarshal(body, &vfResp); err != nil {
		return ExternalVesselData{}, fmt.Errorf("VesselFinder unmarshal: %w", err)
	}

	if len(vfResp.Vessels) == 0 {
		return ExternalVesselData{Source: "VesselFinder", IMO: imo, Found: false}, nil
	}

	v0 := vfResp.Vessels[0]
	ts, _ := time.Parse("2006-01-02T15:04:05", v0.AIS.Timestamp)

	return ExternalVesselData{
		Source:    "VesselFinder",
		IMO:       imo,
		MMSI:      fmt.Sprintf("%d", v0.AIS.MMSI),
		Name:      v0.VSL.Name,
		Flag:      v0.VSL.Flag,
		Lat:       v0.AIS.Lat,
		Lon:       v0.AIS.Lon,
		Speed:     v0.AIS.Speed,
		Timestamp: ts,
		Found:     true,
	}, nil
}

// ── MyShipTracking public API ─────────────────────────────────────────────────
// MyShipTracking offers a free-tier vessel search endpoint (limited calls/day).
// Endpoint: GET https://www.myshiptracking.com/requests/vesseldetails.php?imo=IMO&type=json
// This endpoint returns basic vessel info without authentication.

func (v *VesselVerifier) queryMyShipTracking(ctx context.Context, imo string) (ExternalVesselData, error) {
	url := fmt.Sprintf("https://www.myshiptracking.com/requests/vesseldetails.php?imo=%s&type=json", imo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ExternalVesselData{}, err
	}
	// Mimic browser to avoid bot detection on the public endpoint
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; OSINT-tracker/1.0)")
	req.Header.Set("Accept", "application/json")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return ExternalVesselData{}, fmt.Errorf("MyShipTracking HTTP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ExternalVesselData{}, fmt.Errorf("MyShipTracking: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return ExternalVesselData{}, err
	}

	// MyShipTracking returns either a vessel object or an error/empty response
	var mstResp struct {
		IMO      string  `json:"IMO"`
		MMSI     string  `json:"MMSI"`
		Name     string  `json:"SHIPNAME"`
		Flag     string  `json:"FLAG"`
		Lat      float64 `json:"LAT"`
		Lon      float64 `json:"LON"`
		Speed    float64 `json:"SPEED"`
		Error    string  `json:"error"`
	}

	if len(body) == 0 {
		return ExternalVesselData{Source: "MyShipTracking", IMO: imo, Found: false}, nil
	}
	if err := json.Unmarshal(body, &mstResp); err != nil {
		return ExternalVesselData{}, fmt.Errorf("MyShipTracking unmarshal: %w", err)
	}

	if mstResp.Error != "" || mstResp.Name == "" {
		return ExternalVesselData{Source: "MyShipTracking", IMO: imo, Found: false}, nil
	}

	return ExternalVesselData{
		Source:    "MyShipTracking",
		IMO:       imo,
		MMSI:      mstResp.MMSI,
		Name:      mstResp.Name,
		Flag:      mstResp.Flag,
		Lat:       mstResp.Lat,
		Lon:       mstResp.Lon,
		Speed:     mstResp.Speed,
		Timestamp: time.Now().UTC(),
		Found:     true,
	}, nil
}

// ── Dgraph helpers ────────────────────────────────────────────────────────────

type localVesselMinimal struct {
	UID     string
	IMO     string
	MMSI    string
	Name    string
	Flag    string
	LastLat float64
	LastLon float64
}

func (v *VesselVerifier) fetchLocalVessel(ctx context.Context, imo string) (localVesselMinimal, error) {
	q := `query q($imo: string) {
  vessel(func: eq(vessel.imo, $imo)) {
    uid
    vessel.imo
    vessel.mmsi
    vessel.name
    vessel.flag
    vessel.last_lat
    vessel.last_lon
  }
}`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$imo": imo})
	if err != nil {
		return localVesselMinimal{}, err
	}
	var res struct {
		Vessel []struct {
			UID     string  `json:"uid"`
			IMO     string  `json:"vessel.imo"`
			MMSI    string  `json:"vessel.mmsi"`
			Name    string  `json:"vessel.name"`
			Flag    string  `json:"vessel.flag"`
			LastLat float64 `json:"vessel.last_lat"`
			LastLon float64 `json:"vessel.last_lon"`
		} `json:"vessel"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return localVesselMinimal{}, err
	}
	if len(res.Vessel) == 0 {
		return localVesselMinimal{}, fmt.Errorf("vessel IMO %s not found in DB", imo)
	}
	r := res.Vessel[0]
	return localVesselMinimal{
		UID: r.UID, IMO: r.IMO, MMSI: r.MMSI,
		Name: r.Name, Flag: r.Flag,
		LastLat: r.LastLat, LastLon: r.LastLon,
	}, nil
}

func (v *VesselVerifier) fetchTrackedIMOs(ctx context.Context) ([]string, error) {
	q := `{ vessels(func: has(vessel.imo)) { vessel.imo } }`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	var res struct {
		Vessels []struct {
			IMO string `json:"vessel.imo"`
		} `json:"vessels"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return nil, err
	}
	imos := make([]string, 0, len(res.Vessels))
	for _, vessel := range res.Vessels {
		if vessel.IMO != "" {
			imos = append(imos, vessel.IMO)
		}
	}
	return imos, nil
}

func (v *VesselVerifier) persistVerification(ctx context.Context, vesselUID string, r *VerificationResult) error {
	patch := map[string]interface{}{
		"uid":                       vesselUID,
		"vessel.verify_status":      r.Status,
		"vessel.verify_sources":     strings.Join(r.Sources, ","),
		"vessel.verify_last_at":     r.VerifiedAt,
		"vessel.verify_pos_gap_km":  r.PosGapKm,
		"vessel.verify_discrepancies": strings.Join(r.Discrepancies, " "),
	}
	if r.ExternalName != "" {
		patch["vessel.verify_name_ext"] = r.ExternalName
	}
	if r.ExternalFlag != "" {
		patch["vessel.verify_flag_ext"] = r.ExternalFlag
	}

	b, _ := json.Marshal(patch)
	txn := db.NewTxn()
	defer txn.Discard(ctx)
	_, err := txn.Mutate(ctx, &api.Mutation{SetJson: b, CommitNow: true})
	return err
}

// haversineKmVV is a package-level helper (renamed to avoid collision with mapview).
func haversineKmVV(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371.0
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
