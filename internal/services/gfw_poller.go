package services

// gfw_poller.go — vessel enrichment via Global Fishing Watch API v3 (free tier)
//
// GFW's free-tier vessel search API is designed for known-vessel lookup (by MMSI/IMO)
// not for bulk flag-based discovery.  Attempts to use WHERE clause flag filtering
// returned 0 results in testing, consistent with free-tier restrictions on the
// vessel-identity dataset.
//
// Strategy (two phases per cycle):
//
//   Phase 1 — MMSI enrichment
//     For each vessel already in the graph, search GFW by MMSI to obtain the
//     GFW internal vessel ID.  This succeeds reliably on the free tier.
//
//   Phase 2 — Iranian port-visit detection
//     Batch-query port-visit events for the GFW vessel IDs collected in Phase 1.
//     Any confirmed port visit inside Iranian waters (confidence ≥ 2) in the last
//     180 days triggers osintUpsertIranPresence.  This catches historical Iran calls
//     that pre-date the start of AIS stream monitoring.
//
// Free tier: register at https://globalfishingwatch.org/our-apis/
// Set env:   GFW_API_KEY=<token>
// Rate limits: free tier allows ~1 000 req/day; Phase 1 is limited to 50 vessels
// per cycle to stay well within this budget.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/proptrack/proptrack/internal/config"
	"github.com/proptrack/proptrack/internal/db"
)

const gfwBaseURL = "https://gateway.api.globalfishingwatch.org"

// GFWPoller enriches known vessels and detects Iranian port visits via GFW.
type GFWPoller struct {
	cfg    *config.Config
	client *http.Client
}

// NewGFWPoller creates a new GFWPoller.
func NewGFWPoller(cfg *config.Config) *GFWPoller {
	return &GFWPoller{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Start launches the polling loop. Blocks until ctx is cancelled.
func (g *GFWPoller) Start(ctx context.Context) {
	if !g.cfg.GFWEnabled {
		slog.Warn("GFW poller disabled (GFW_API_KEY not set)")
		return
	}
	slog.Info("GFW poller started", "interval", g.cfg.GFWInterval)

	g.runCycle(ctx)

	ticker := time.NewTicker(g.cfg.GFWInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			g.runCycle(ctx)
		case <-ctx.Done():
			slog.Info("GFW poller stopped")
			return
		}
	}
}

func (g *GFWPoller) runCycle(ctx context.Context) {
	slog.Info("GFW poller: starting cycle")

	// Phase 1: collect GFW vessel IDs for vessels already in the graph
	gfwIDs := g.collectGFWIDs(ctx)
	slog.Info("GFW poller: MMSI lookup complete", "gfw_ids_found", len(gfwIDs))

	if len(gfwIDs) == 0 {
		slog.Info("GFW poller: no GFW IDs resolved — skipping event detection")
		return
	}

	// Phase 2: query port-visit events and detect Iranian port calls
	detected := g.detectIranVisits(ctx, gfwIDs)
	slog.Info("GFW poller: Iran visit detection complete", "iran_arrivals", detected)
}

// ── Phase 1: MMSI → GFW vessel ID lookup ─────────────────────────────────────

// collectGFWIDs queries the graph for known vessel MMSIs and searches GFW for each
// one to obtain the GFW internal vessel ID.  Capped at 50 vessels per cycle.
func (g *GFWPoller) collectGFWIDs(ctx context.Context) []string {
	mmsis, err := g.queryKnownMMSIs(ctx)
	if err != nil {
		slog.Warn("GFW: failed to query known MMSIs", "err", err)
		return nil
	}
	if len(mmsis) == 0 {
		slog.Debug("GFW: no vessels with MMSI in graph yet")
		return nil
	}

	// Cap at 50 to stay within free-tier request budget
	if len(mmsis) > 50 {
		mmsis = mmsis[:50]
	}

	var gfwIDs []string
	for _, mmsi := range mmsis {
		id, err := g.searchByMMSI(ctx, mmsi)
		if err != nil {
			slog.Debug("GFW: MMSI lookup failed", "mmsi", mmsi, "err", err)
			continue
		}
		if id != "" {
			gfwIDs = append(gfwIDs, id)
		}
		// 200 ms between requests — ~5 req/s, well within free-tier rate limits
		select {
		case <-ctx.Done():
			return gfwIDs
		case <-time.After(200 * time.Millisecond):
		}
	}
	return gfwIDs
}

// queryKnownMMSIs returns all vessel MMSIs currently stored in the graph.
func (g *GFWPoller) queryKnownMMSIs(ctx context.Context) ([]string, error) {
	q := `{ vessels(func: has(vessel.mmsi)) { vessel.mmsi } }`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	var res struct {
		Vessels []struct {
			MMSI string `json:"vessel.mmsi"`
		} `json:"vessels"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return nil, err
	}
	mmsis := make([]string, 0, len(res.Vessels))
	for _, v := range res.Vessels {
		if v.MMSI != "" {
			mmsis = append(mmsis, v.MMSI)
		}
	}
	return mmsis, nil
}

// gfwVesselSearchResponse is the top-level response from /v3/vessels/search.
type gfwVesselSearchResponse struct {
	Entries []gfwVesselEntry `json:"entries"`
	Total   int              `json:"total"`
}

type gfwVesselEntry struct {
	GFWID        string            `json:"id"`
	RegistryInfo []gfwRegistryInfo `json:"registryInfo"`
	SelfReported []gfwSelfReported `json:"selfReportedInfo"`
}

type gfwRegistryInfo struct {
	SSVID    string `json:"ssvid"`
	Flag     string `json:"flag"`
	ShipName string `json:"shipname"`
	IMO      string `json:"imo"`
}

type gfwSelfReported struct {
	ShipType string `json:"shiptype"`
}

// searchByMMSI searches GFW for the vessel matching mmsi and returns its GFW internal ID.
// Returns "" if not found.
func (g *GFWPoller) searchByMMSI(ctx context.Context, mmsi string) (string, error) {
	apiURL := fmt.Sprintf(
		"%s/v3/vessels/search?query=%s&datasets[0]=public-global-vessel-identity:latest&limit=5",
		gfwBaseURL, mmsi,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.GFWAPIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var result gfwVesselSearchResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("unmarshal: %w", err)
	}

	// Return the first entry whose MMSI (ssvid) matches exactly
	for _, entry := range result.Entries {
		for _, reg := range entry.RegistryInfo {
			if reg.SSVID == mmsi {
				return entry.GFWID, nil
			}
		}
	}
	// Fallback: first result if only one came back
	if len(result.Entries) == 1 {
		return result.Entries[0].GFWID, nil
	}
	return "", nil
}

// ── Phase 2: Iranian port-visit detection ─────────────────────────────────────

type gfwEventsResponse struct {
	Entries []gfwEventEntry `json:"entries"`
	Total   int             `json:"total"`
}

type gfwEventEntry struct {
	Type      string         `json:"type"`
	Start     string         `json:"start"`
	Position  gfwPosition    `json:"position"`
	Vessel    gfwEventVessel `json:"vessel"`
	PortVisit *gfwPortVisit  `json:"portVisit"`
}

type gfwPosition struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type gfwEventVessel struct {
	SSVID string `json:"ssvid"`
	Name  string `json:"name"`
	Flag  string `json:"flag"` // ISO-3
	IMO   string `json:"imo"`
}

type gfwPortVisit struct {
	Confidence     int          `json:"confidence"`
	StartAnchorage gfwAnchorage `json:"startAnchorage"`
}

type gfwAnchorage struct {
	Name    string  `json:"name"`
	Country string  `json:"country"` // ISO-3
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

// gfwISO3ToISO2 converts ISO-3 flag codes to ISO-2 for storage in the graph.
var gfwISO3ToISO2 = map[string]string{
	"IRN": "IR", "COM": "KM", "MNG": "MN", "TZA": "TZ", "TUV": "TV",
	"PLW": "PW", "CMR": "CM", "BOL": "BO", "GNB": "GW", "GAB": "GA",
	"TGO": "TG", "SLE": "SL", "MHL": "MH", "BLZ": "BZ",
	"CHN": "CN", "SGP": "SG", "MYS": "MY", "ARE": "AE",
}

// detectIranVisits queries port-visit events for the given GFW vessel IDs and
// calls osintUpsertIranPresence for each confirmed Iranian port visit.
func (g *GFWPoller) detectIranVisits(ctx context.Context, gfwIDs []string) int {
	const batchSize = 50
	startDate := time.Now().UTC().AddDate(0, 0, -180).Format("2006-01-02")
	endDate := time.Now().UTC().Format("2006-01-02")

	detected := 0
	for i := 0; i < len(gfwIDs); i += batchSize {
		end := i + batchSize
		if end > len(gfwIDs) {
			end = len(gfwIDs)
		}

		events, err := g.fetchPortVisitEvents(ctx, gfwIDs[i:end], startDate, endDate)
		if err != nil {
			slog.Warn("GFW: port visit query failed", "err", err)
			continue
		}

		for _, ev := range events {
			if ev.PortVisit == nil || ev.PortVisit.Confidence < 2 {
				continue
			}
			if ev.PortVisit.StartAnchorage.Country != "IRN" {
				continue
			}
			imo := ev.Vessel.IMO
			if imo == "" || imo == "0" {
				continue
			}

			lat := ev.PortVisit.StartAnchorage.Lat
			lon := ev.PortVisit.StartAnchorage.Lon
			portCode := gfwResolveIranPort(lat, lon)

			flag := gfwISO3ToISO2[ev.Vessel.Flag]
			if flag == "" && len(ev.Vessel.Flag) >= 2 {
				flag = ev.Vessel.Flag[:2]
			}

			slog.Info("GFW: Iranian port visit detected",
				"imo", imo, "vessel", ev.Vessel.Name,
				"port", ev.PortVisit.StartAnchorage.Name,
				"date", ev.Start, "confidence", ev.PortVisit.Confidence,
			)
			osintUpsertIranPresence(ctx,
				imo, ev.Vessel.SSVID, ev.Vessel.Name,
				flag, "", "gfw",
				lat, lon, 0, 0, 0,
				portCode, "GFW",
			)
			detected++
		}

		select {
		case <-ctx.Done():
			return detected
		case <-time.After(300 * time.Millisecond):
		}
	}
	return detected
}

func (g *GFWPoller) fetchPortVisitEvents(ctx context.Context, gfwIDs []string, startDate, endDate string) ([]gfwEventEntry, error) {
	var sb strings.Builder
	sb.WriteString(gfwBaseURL)
	sb.WriteString("/v3/events?datasets[0]=public-global-vessel-events:latest&types=port_visit&limit=200")
	fmt.Fprintf(&sb, "&startDate=%s&endDate=%s", startDate, endDate)
	for i, id := range gfwIDs {
		fmt.Fprintf(&sb, "&vessels[%d]=%s", i, id)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sb.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.GFWAPIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var result gfwEventsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return result.Entries, nil
}

// gfwResolveIranPort returns the best matching Iranian UNLOCODE for a lat/lon.
func gfwResolveIranPort(lat, lon float64) string {
	zone := findZoneForPosition(lat, lon)
	if zone != nil && zone.Country == "IR" {
		return zone.UNLOCODE
	}
	iranPorts := []struct {
		code     string
		lat, lon float64
	}{
		{"IRBND", 27.15, 56.19},
		{"IRCHB", 25.29, 60.64},
		{"IRBUZ", 28.98, 50.83},
		{"IRKHO", 30.43, 48.17},
	}
	best, bestDist := "IRBND", 1e9
	for _, p := range iranPorts {
		d := (lat-p.lat)*(lat-p.lat) + (lon-p.lon)*(lon-p.lon)
		if d < bestDist {
			bestDist = d
			best = p.code
		}
	}
	return best
}
