package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/proptrack/proptrack/internal/config"
	"github.com/proptrack/proptrack/internal/db"
)

// watchedPortZone defines a geographic bounding box for port-area surveillance.
type watchedPortZone struct {
	UNLOCODE string
	Country  string // "CN" → departure discovery; "IR" → arrival confirmation
	Name     string
	MinLat, MaxLat float64
	MinLon, MaxLon float64
}

// watchedPortZones covers the key Chinese export ports and Iranian receiving ports.
var watchedPortZones = []watchedPortZone{
	// ── Chinese high-risk export ports ────────────────────────────────────────
	{UNLOCODE: "CNZHL", Country: "CN", Name: "Gaolan/Zhuhai",
		MinLat: 21.6, MaxLat: 22.4, MinLon: 113.0, MaxLon: 113.7},
	{UNLOCODE: "CNTAI", Country: "CN", Name: "Taicang",
		MinLat: 31.2, MaxLat: 32.0, MinLon: 120.6, MaxLon: 121.5},
	{UNLOCODE: "CNTJN", Country: "CN", Name: "Tianjin",
		MinLat: 38.5, MaxLat: 39.5, MinLon: 117.2, MaxLon: 118.2},
	{UNLOCODE: "CNTAO", Country: "CN", Name: "Qingdao",
		MinLat: 35.6, MaxLat: 36.6, MinLon: 119.8, MaxLon: 121.0},
	{UNLOCODE: "CNSHA", Country: "CN", Name: "Shanghai",
		MinLat: 30.5, MaxLat: 32.0, MinLon: 121.0, MaxLon: 122.5},
	// ── Iranian receiving ports ───────────────────────────────────────────────
	{UNLOCODE: "IRCHB", Country: "IR", Name: "Chabahar",
		MinLat: 24.8, MaxLat: 25.8, MinLon: 60.2, MaxLon: 61.1},
	{UNLOCODE: "IRBND", Country: "IR", Name: "Bandar Abbas",
		MinLat: 26.8, MaxLat: 27.6, MinLon: 55.8, MaxLon: 56.8},
	{UNLOCODE: "IRBUZ", Country: "IR", Name: "Bushehr",
		MinLat: 28.5, MaxLat: 29.5, MinLon: 50.3, MaxLon: 51.3},
	{UNLOCODE: "IRKHO", Country: "IR", Name: "Khorramshahr",
		MinLat: 29.9, MaxLat: 30.9, MinLon: 47.5, MaxLon: 48.9},
}

// mtVesselEntry is the JSON structure returned by MarineTraffic exportvessel API v8
// with msgtype:extended. Fields are a subset of the full MT extended vessel record.
type mtVesselEntry struct {
	MMSI            string  `json:"MMSI"`
	IMO             string  `json:"IMO"`
	ShipName        string  `json:"SHIPNAME"`
	Flag            string  `json:"FLAG"`
	TypeSpecific    string  `json:"TYPE_SPECIFIC"`
	AISTypeSummary  string  `json:"AIS_TYPE_SUMMARY"`
	Lat             float64 `json:"LAT"`
	Lon             float64 `json:"LON"`
	Speed           float64 `json:"SPEED"`
	Draught         float64 `json:"DRAUGHT"`
	GRT             float64 `json:"GRT"`
	DWT             float64 `json:"DWT"`
	Length          float64 `json:"LENGTH"`
	Width           float64 `json:"WIDTH"`
	Destination     string  `json:"DESTINATION"`
	ETA             string  `json:"ETA"`
	CurrentPortUNLO string  `json:"CURRENT_PORT_UNLO"`
	LastPortUNLO    string  `json:"LAST_PORT_UNLO"`
	LastPortTime    string  `json:"LAST_PORT_TIME"`
	Timestamp       string  `json:"TIMESTAMP"`
	Status          int     `json:"STATUS"`
}

// PortWatcher periodically queries MarineTraffic for vessels in watched port zones.
// For Chinese port zones: auto-creates vessel + shipment records on discovery.
// For Iranian port zones: confirms arrivals on monitoring shipments.
// Routes with no Iranian connection after RoutePruneDays are marked "pruned".
type PortWatcher struct {
	cfg *config.Config
}

// NewPortWatcher creates a new PortWatcher.
func NewPortWatcher(cfg *config.Config) *PortWatcher {
	return &PortWatcher{cfg: cfg}
}

// Start launches the watch loop. Blocks until ctx is cancelled.
func (w *PortWatcher) Start(ctx context.Context) {
	if !w.cfg.MarineTrafficEnabled {
		slog.Warn("port watcher disabled (MARINETRAFFIC_API_KEY not set)")
		return
	}
	slog.Info("port watcher started",
		"interval", w.cfg.PortWatchInterval,
		"prune_days", w.cfg.RoutePruneDays,
	)

	// Run immediately then on interval
	w.runCycle(ctx)

	ticker := time.NewTicker(w.cfg.PortWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.runCycle(ctx)
		case <-ctx.Done():
			slog.Info("port watcher stopped")
			return
		}
	}
}

// runCycle performs one full discovery + pruning cycle.
func (w *PortWatcher) runCycle(ctx context.Context) {
	slog.Info("port watcher: starting cycle", "zones", len(watchedPortZones))
	discovered := 0
	for _, zone := range watchedPortZones {
		vessels, err := w.fetchMTVessels(zone)
		if err != nil {
			slog.Warn("port watcher: MT query failed", "zone", zone.UNLOCODE, "err", err)
			continue
		}
		slog.Debug("port watcher: zone polled", "zone", zone.UNLOCODE, "vessels", len(vessels))
		for _, v := range vessels {
			if v.IMO == "" || v.IMO == "0" {
				continue
			}
			switch zone.Country {
			case "CN":
				if w.processCNZoneVessel(ctx, v, zone) {
					discovered++
				}
			case "IR":
				w.processIRZoneVessel(ctx, v, zone)
			}
		}
	}
	slog.Info("port watcher: cycle complete", "new_shipments_created", discovered)
	pruned := osintPruneNonIranRoutes(ctx, w.cfg.RoutePruneDays)
	if pruned > 0 {
		slog.Info("port watcher: routes pruned", "count", pruned)
	}
}

// processCNZoneVessel handles a vessel found in a Chinese port zone.
// Returns true if a new tracking shipment was created.
func (w *PortWatcher) processCNZoneVessel(ctx context.Context, v mtVesselEntry, zone watchedPortZone) bool {
	if v.Speed > 14 {
		return false
	}

	vesselUID, err := osintUpsertVessel(ctx,
		v.IMO, v.MMSI, v.ShipName, v.Flag,
		pwMapAISType(v.TypeSpecific), "marinetraffic",
		v.Lat, v.Lon, v.Draught, v.GRT, v.DWT,
	)
	if err != nil {
		slog.Warn("port watcher: vessel upsert failed", "imo", v.IMO, "err", err)
		return false
	}

	if osintHasRecentShipmentFromPort(ctx, v.IMO, zone.UNLOCODE, 60) {
		return false
	}

	destUNLOCODE := parseIranianDestinationPW(v.Destination)
	status := "monitoring"
	if destUNLOCODE != "" {
		status = "active"
	}

	originUID, err := pwResolvePort(ctx, zone.UNLOCODE)
	if err != nil || originUID == "" {
		slog.Warn("port watcher: origin port not found", "unlocode", zone.UNLOCODE)
		return false
	}

	destUID := ""
	if destUNLOCODE != "" {
		destUID, _ = pwResolvePort(ctx, destUNLOCODE)
	}

	shipmentID := "AUTO-" + uuid.New().String()[:8]
	sourceRef := fmt.Sprintf("Auto-discovered via MarineTraffic API; AIS dest: %q; speed: %.1f kn", v.Destination, v.Speed)
	if err := osintCreateShipment(ctx, shipmentID, vesselUID, originUID, destUID, status, sourceRef); err != nil {
		slog.Warn("port watcher: shipment creation failed", "imo", v.IMO, "err", err)
		return false
	}

	slog.Info("port watcher: shipment auto-created",
		"id", shipmentID,
		"imo", v.IMO,
		"vessel", v.ShipName,
		"origin", zone.UNLOCODE,
		"dest_parsed", destUNLOCODE,
		"dest_raw", v.Destination,
		"status", status,
	)
	return true
}

// processIRZoneVessel handles a vessel found near an Iranian port.
func (w *PortWatcher) processIRZoneVessel(ctx context.Context, v mtVesselEntry, zone watchedPortZone) {
	osintUpsertIranPresence(ctx,
		v.IMO, v.MMSI, v.ShipName, v.Flag, v.TypeSpecific, "marinetraffic",
		v.Lat, v.Lon, v.Draught, v.GRT, v.DWT,
		zone.UNLOCODE, "MT",
	)
}

// fetchMTVessels calls the MarineTraffic exportvessel v8 API for a zone.
// Endpoint: https://services.marinetraffic.com/api/exportvessel/v:8/{KEY}/minlat:{}/maxlat:{}/minlon:{}/maxlon:{}/msgtype:extended/protocol:jsono
func (w *PortWatcher) fetchMTVessels(zone watchedPortZone) ([]mtVesselEntry, error) {
	url := fmt.Sprintf(
		"https://services.marinetraffic.com/api/exportvessel/v:8/%s/minlat:%g/maxlat:%g/minlon:%g/maxlon:%g/msgtype:extended/protocol:jsono",
		w.cfg.MarineTrafficAPIKey,
		zone.MinLat, zone.MaxLat, zone.MinLon, zone.MaxLon,
	)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("MT API request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("MT API %d: %.200s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// jsono protocol returns {"data": [...]}
	var result struct {
		Data []mtVesselEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return result.Data, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// iranianPortMapPW maps AIS destination strings to known Iranian UNLOCODE.
// Kept separate from ais_poller.go to avoid conflicts.
var iranianPortMapPW = map[string]string{
	"BANDAR ABBAS":  "IRBND",
	"BANDARABBAS":   "IRBND",
	"BANDAR_ABBAS":  "IRBND",
	"IRBND":        "IRBND",
	"CHABAHAR":     "IRCHB",
	"CHAHBAHAR":    "IRCHB",
	"IRCHB":        "IRCHB",
	"BUSHEHR":      "IRBUZ",
	"BUSHIRE":      "IRBUZ",
	"IRBUZ":        "IRBUZ",
	"KHORRAMSHAHR": "IRKHO",
	"IRKHO":        "IRKHO",
	"TEHRAN":       "IRTHR",
	"IRTHR":        "IRTHR",
}

func parseIranianDestinationPW(dest string) string {
	d := strings.ToUpper(strings.TrimSpace(dest))
	for kw, code := range iranianPortMapPW {
		if strings.Contains(d, kw) {
			return code
		}
	}
	return ""
}

func pwMapAISType(aisType string) string {
	t := strings.ToLower(aisType)
	switch {
	case strings.Contains(t, "bulk"):
		return "bulk_carrier"
	case strings.Contains(t, "chemical") || strings.Contains(t, "tanker"):
		return "chemical_tanker"
	case strings.Contains(t, "ro-ro") || strings.Contains(t, "roro"):
		return "ro_ro"
	case strings.Contains(t, "container"):
		return "container_ship"
	default:
		return "general_cargo"
	}
}

func pwResolveVessel(ctx context.Context, imo string) (string, error) {
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
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return "", err
	}
	if len(res.Vessel) == 0 {
		return "", nil
	}
	return res.Vessel[0].UID, nil
}

func pwResolvePort(ctx context.Context, code string) (string, error) {
	q := `query q($code: string) { port(func: eq(port.unlocode, $code)) { uid } }`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)
	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$code": code})
	if err != nil {
		return "", err
	}
	var res struct {
		Port []struct{ UID string `json:"uid"` } `json:"port"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return "", err
	}
	if len(res.Port) == 0 {
		return "", nil
	}
	return res.Port[0].UID, nil
}
