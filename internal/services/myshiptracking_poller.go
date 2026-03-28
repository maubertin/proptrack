package services

// myshiptracking_poller.go — vessel discovery via the MyShipTracking AIS API v2.
//
// Endpoint used: GET /vessel/zone (zone-based bounding box query)
// Auth:          Authorization: Bearer <MYSHIPTRACKING_API_KEY>
// Free trial:    2 000 coins / 10 days — https://api.myshiptracking.com/
//
// Strategy: same 9 port zones as the MarineTraffic PortWatcher.
//   • Chinese zones → create monitoring/active shipments for slow vessels (speed ≤ 14 kn)
//   • Iranian zones  → confirm arrival on existing monitoring shipments
// Note: route pruning is handled by the shared osintPruneNonIranRoutes helper so that
// whichever poller runs last in a cycle performs the cleanup exactly once.

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
)

// mstVesselEntry is the JSON structure returned by MyShipTracking /vessel/zone
// with response=extended (3 credits per vessel).
type mstVesselEntry struct {
	VesselName   string  `json:"vessel_name"`
	MMSI         string  `json:"mmsi"`
	IMO          string  `json:"imo"`
	Latitude     float64 `json:"latitude"`
	Longitude    float64 `json:"longitude"`
	Course       float64 `json:"course"`
	Speed        float64 `json:"speed"`
	NavStatus    int     `json:"nav_status"`
	Flag         string  `json:"flag"`
	VesselType   string  `json:"vessel_type"`
	Draught      float64 `json:"draught"`
	GrossTonnage float64 `json:"gross_tonnage"`
	Deadweight   float64 `json:"deadweight"`
	Destination  string  `json:"destination"`
	ETA          string  `json:"eta"`
	CurrentPort  string  `json:"current_port"`
	LastPort     string  `json:"last_port"`
	NextPort     string  `json:"next_port"`
	Timestamp    string  `json:"timestamp"`
	Callsign     string  `json:"callsign"`
}

// MSTPoller uses the MyShipTracking API to discover vessels in watched port zones.
type MSTPoller struct {
	cfg *config.Config
}

// NewMSTPoller creates a new MSTPoller.
func NewMSTPoller(cfg *config.Config) *MSTPoller {
	return &MSTPoller{cfg: cfg}
}

// Start launches the polling loop. Blocks until ctx is cancelled.
func (p *MSTPoller) Start(ctx context.Context) {
	if !p.cfg.MSTPollerEnabled {
		slog.Warn("MST poller disabled (MYSHIPTRACKING_API_KEY not set)")
		return
	}
	slog.Info("MST poller started", "interval", p.cfg.MSTPollerInterval)

	p.runCycle(ctx)

	ticker := time.NewTicker(p.cfg.MSTPollerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.runCycle(ctx)
		case <-ctx.Done():
			slog.Info("MST poller stopped")
			return
		}
	}
}

func (p *MSTPoller) runCycle(ctx context.Context) {
	slog.Info("MST poller: starting cycle", "zones", len(watchedPortZones))
	discovered := 0
	for _, zone := range watchedPortZones {
		vessels, err := p.fetchZoneVessels(zone)
		if err != nil {
			slog.Warn("MST poller: zone query failed", "zone", zone.UNLOCODE, "err", err)
			continue
		}
		slog.Debug("MST poller: zone polled", "zone", zone.UNLOCODE, "vessels", len(vessels))
		for _, v := range vessels {
			if v.IMO == "" || v.IMO == "0" {
				continue
			}
			switch zone.Country {
			case "CN":
				if p.processCNZone(ctx, v, zone) {
					discovered++
				}
			case "IR":
				p.processIRZone(ctx, v, zone)
			}
		}
	}
	slog.Info("MST poller: cycle complete", "new_shipments", discovered)

	pruned := osintPruneNonIranRoutes(ctx, p.cfg.RoutePruneDays)
	if pruned > 0 {
		slog.Info("MST poller: routes pruned", "count", pruned)
	}
}

func (p *MSTPoller) processCNZone(ctx context.Context, v mstVesselEntry, zone watchedPortZone) bool {
	if v.Speed > 14 {
		return false
	}

	vesselUID, err := osintUpsertVessel(ctx,
		v.IMO, v.MMSI, v.VesselName, v.Flag,
		mstMapVesselType(v.VesselType), "myshiptracking",
		v.Latitude, v.Longitude, v.Draught, v.GrossTonnage, v.Deadweight,
	)
	if err != nil || vesselUID == "" {
		slog.Warn("MST poller: vessel upsert failed", "imo", v.IMO, "err", err)
		return false
	}

	if osintHasRecentShipmentFromPort(ctx, v.IMO, zone.UNLOCODE, 60) {
		return false
	}

	// Use NextPort as a secondary destination hint when AIS Destination is absent.
	destUNLOCODE := parseIranianDestinationPW(v.Destination)
	if destUNLOCODE == "" && v.NextPort != "" {
		destUNLOCODE = parseIranianDestinationPW(v.NextPort)
	}
	status := "monitoring"
	if destUNLOCODE != "" {
		status = "active"
	}

	originUID, err := pwResolvePort(ctx, zone.UNLOCODE)
	if err != nil || originUID == "" {
		slog.Warn("MST poller: origin port not found", "unlocode", zone.UNLOCODE)
		return false
	}

	destUID := ""
	if destUNLOCODE != "" {
		destUID, _ = pwResolvePort(ctx, destUNLOCODE)
	}

	shipmentID := "MST-" + uuid.New().String()[:8]
	sourceRef := fmt.Sprintf(
		"Auto-discovered via MyShipTracking API; AIS dest: %q; next_port: %q; speed: %.1f kn",
		v.Destination, v.NextPort, v.Speed,
	)
	if err := osintCreateShipment(ctx, shipmentID, vesselUID, originUID, destUID, status, sourceRef); err != nil {
		slog.Warn("MST poller: shipment creation failed", "imo", v.IMO, "err", err)
		return false
	}
	slog.Info("MST poller: shipment auto-created",
		"id", shipmentID, "imo", v.IMO, "vessel", v.VesselName,
		"origin", zone.UNLOCODE, "dest", destUNLOCODE, "status", status,
	)
	return true
}

func (p *MSTPoller) processIRZone(ctx context.Context, v mstVesselEntry, zone watchedPortZone) {
	osintUpsertIranPresence(ctx,
		v.IMO, v.MMSI, v.VesselName, v.Flag, v.VesselType, "myshiptracking",
		v.Latitude, v.Longitude, v.Draught, v.GrossTonnage, v.Deadweight,
		zone.UNLOCODE, "MST",
	)
}

// fetchZoneVessels calls GET /vessel/zone on the MST API v2.
// MST uses a center lat/lon + radius (km); we derive these from the bounding box.
// API reference: https://api.myshiptracking.com/docs/vessel-zone
func (p *MSTPoller) fetchZoneVessels(zone watchedPortZone) ([]mstVesselEntry, error) {
	centerLat := (zone.MinLat + zone.MaxLat) / 2
	centerLon := (zone.MinLon + zone.MaxLon) / 2

	// Approximate radius: half the larger diagonal span in km (111 km/degree).
	latKm := (zone.MaxLat - zone.MinLat) * 111
	lonKm := (zone.MaxLon - zone.MinLon) * 111
	radius := latKm
	if lonKm > radius {
		radius = lonKm
	}
	if radius < 50 {
		radius = 50
	}

	url := fmt.Sprintf(
		"https://api.myshiptracking.com/api/v2/vessel/zone?lat=%g&lon=%g&radius=%g&response=extended",
		centerLat, centerLon, radius,
	)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.MyShipTrackingAPIKey)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("MST API request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("MST API %d: %.200s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var result struct {
		Data []mstVesselEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return result.Data, nil
}

// mstMapVesselType normalises MST vessel_type strings to internal identifiers.
func mstMapVesselType(t string) string {
	lower := strings.ToLower(t)
	switch {
	case strings.Contains(lower, "bulk"):
		return "bulk_carrier"
	case strings.Contains(lower, "chemical") || strings.Contains(lower, "tanker"):
		return "chemical_tanker"
	case strings.Contains(lower, "container"):
		return "container_ship"
	case strings.Contains(lower, "ro-ro") || strings.Contains(lower, "roro"):
		return "ro_ro"
	default:
		return "general_cargo"
	}
}
