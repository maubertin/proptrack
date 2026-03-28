package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/proptrack/proptrack/internal/config"
	"github.com/proptrack/proptrack/internal/db"
)

// ── aisstream.io message types ────────────────────────────────────────────────

// aisSubscription is the initial message sent to aisstream.io after connecting.
// Docs: https://aisstream.io/documentation
type aisSubscription struct {
	APIKey             string          `json:"APIKey"`
	BoundingBoxes      [][][2]float64  `json:"BoundingBoxes"`
	FiltersShipMMSI    []string        `json:"FiltersShipMMSI,omitempty"`
	FilterMessageTypes []string        `json:"FilterMessageTypes"`
}

// aisEnvelope is the top-level wrapper for every message from aisstream.io.
type aisEnvelope struct {
	MessageType string              `json:"MessageType"`
	Message     aisMessagePayload   `json:"Message"`
	MetaData    aisMetaData         `json:"MetaData"`
}

type aisMessagePayload struct {
	PositionReport    *aisPositionReport    `json:"PositionReport,omitempty"`
	ShipStaticData    *aisShipStaticData    `json:"ShipStaticData,omitempty"`
}

// aisPositionReport corresponds to AIS message types 1, 2, 3.
type aisPositionReport struct {
	UserID              int     `json:"UserID"` // MMSI
	Latitude            float64 `json:"Latitude"`
	Longitude           float64 `json:"Longitude"`
	Sog                 float64 `json:"Sog"`           // speed over ground (knots)
	Cog                 float64 `json:"Cog"`           // course over ground
	NavigationalStatus  int     `json:"NavigationalStatus"`
	// NavigationalStatus values: 0=underway, 1=at anchor, 5=moored, 8=under way sailing
	Valid               bool    `json:"Valid"`
}

// aisShipStaticData corresponds to AIS message type 5 (ship & voyage related data).
type aisShipStaticData struct {
	UserID      int    `json:"UserID"` // MMSI
	Name        string `json:"Name"`
	CallSign    string `json:"CallSign"`
	ImoNumber   int    `json:"ImoNumber"`
	Draught     float64 `json:"Draught"` // static draught (metres × 10 in raw AIS, normalised here)
	Destination string `json:"Destination"`
}

type aisMetaData struct {
	MMSI       int    `json:"MMSI"`
	MMSIString string `json:"MMSI_String"`
	ShipName   string `json:"ShipName"`
	Latitude   float64 `json:"latitude"`
	Longitude  float64 `json:"longitude"`
	TimeUTC    string `json:"time_utc"`
}

// ── NavigationalStatus → AISStatus mapping ────────────────────────────────────
// Status 15 means "not defined" which AIS receivers report when transponder is
// transmitting but source is unreliable — used as a proxy for spoofing detection.
func navStatusToAISStatus(status int) string {
	switch status {
	case 0, 8: // underway using engine / under way sailing
		return "active"
	case 1, 5: // at anchor, moored
		return "active"
	case 15: // undefined — possible spoofing
		return "spoofing"
	default:
		return "active"
	}
}

// ── Watchlist cache ───────────────────────────────────────────────────────────

// vesselWatchlist maintains an in-memory MMSI→IMO index refreshed from Dgraph.
type vesselWatchlist struct {
	mu      sync.RWMutex
	mmsiMap map[string]string // MMSI string → IMO string
}

func newWatchlist() *vesselWatchlist {
	return &vesselWatchlist{mmsiMap: make(map[string]string)}
}

func (w *vesselWatchlist) get(mmsi string) (string, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	imo, ok := w.mmsiMap[mmsi]
	return imo, ok
}

func (w *vesselWatchlist) set(mmsi, imo string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.mmsiMap[mmsi] = imo
}

func (w *vesselWatchlist) mmsis() []string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]string, 0, len(w.mmsiMap))
	for mmsi := range w.mmsiMap {
		out = append(out, mmsi)
	}
	return out
}

// refresh queries Dgraph for all vessel MMSIs and rebuilds the cache.
func (w *vesselWatchlist) refresh(ctx context.Context) {
	q := `{
  vessels(func: has(vessel.mmsi)) {
    vessel.mmsi
    vessel.imo
  }
}`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Query(ctx, q)
	if err != nil {
		slog.Warn("watchlist refresh failed", "err", err)
		return
	}
	var res struct {
		Vessels []struct {
			MMSI string `json:"vessel.mmsi"`
			IMO  string `json:"vessel.imo"`
		} `json:"vessels"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		slog.Warn("watchlist unmarshal failed", "err", err)
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.mmsiMap = make(map[string]string, len(res.Vessels))
	for _, v := range res.Vessels {
		if v.MMSI != "" && v.IMO != "" {
			w.mmsiMap[v.MMSI] = v.IMO
		}
	}
	slog.Info("watchlist refreshed", "vessels_with_mmsi", len(w.mmsiMap))
}

// ── AIS Poller ────────────────────────────────────────────────────────────────

// AISPoller connects to aisstream.io and dispatches AIS updates to the tracker.
type AISPoller struct {
	cfg       *config.Config
	watchlist *vesselWatchlist
	// discoveredMMSIs tracks unseen MMSIs in monitored areas (analyst discovery mode).
	discoveredMMSIs sync.Map
}

// NewAISPoller creates a new AIS poller but does not start it.
func NewAISPoller(cfg *config.Config) *AISPoller {
	return &AISPoller{
		cfg:       cfg,
		watchlist: newWatchlist(),
	}
}

// Start launches the poller loop. Blocks until ctx is cancelled.
// Automatically reconnects on disconnection.
func (p *AISPoller) Start(ctx context.Context) {
	if !p.cfg.AISStreamEnabled {
		slog.Warn("AIS poller disabled (AIS_STREAM_API_KEY not set)")
		return
	}

	// Initial watchlist load
	p.watchlist.refresh(ctx)

	// Refresh watchlist every 10 minutes to pick up newly added vessels
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.watchlist.refresh(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()

	// Main reconnect loop
	for {
		select {
		case <-ctx.Done():
			slog.Info("AIS poller stopped")
			return
		default:
		}

		slog.Info("AIS poller connecting", "url", p.cfg.AISStreamURL)
		if err := p.runSession(ctx); err != nil {
			slog.Warn("AIS session ended, will reconnect",
				"err", err,
				"delay", p.cfg.AISReconnectDelay,
			)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(p.cfg.AISReconnectDelay):
		}
	}
}

// runSession opens one WebSocket session and processes messages until error or ctx cancel.
func (p *AISPoller) runSession(ctx context.Context) error {
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, p.cfg.AISStreamURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// Build subscription — bounding boxes + MMSI watchlist
	sub := p.buildSubscription()
	if err := conn.WriteJSON(sub); err != nil {
		return fmt.Errorf("subscription write: %w", err)
	}
	slog.Info("AIS subscription sent",
		"bounding_boxes", len(sub.BoundingBoxes),
		"mmsi_watchlist", len(sub.FiltersShipMMSI),
	)

	// Read loop
	msgCount := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var env aisEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			slog.Debug("failed to parse AIS message", "err", err)
			continue
		}

		p.dispatch(ctx, env)
		msgCount++

		if msgCount%500 == 0 {
			slog.Debug("AIS messages processed", "count", msgCount)
		}
	}
}

// dispatch routes an AIS envelope to the appropriate handler.
func (p *AISPoller) dispatch(ctx context.Context, env aisEnvelope) {
	switch env.MessageType {
	case "PositionReport":
		if env.Message.PositionReport != nil {
			p.handlePosition(ctx, env)
		}
	case "ShipStaticData":
		if env.Message.ShipStaticData != nil {
			p.handleStaticData(ctx, env)
		}
	}
}

// handlePosition processes a PositionReport message.
func (p *AISPoller) handlePosition(ctx context.Context, env aisEnvelope) {
	mmsiStr := env.MetaData.MMSIString
	if mmsiStr == "" {
		mmsiStr = fmt.Sprintf("%d", env.MetaData.MMSI)
	}

	// Check watchlist: is this a vessel we track?
	imo, tracked := p.watchlist.get(mmsiStr)
	if !tracked {
		// Position reports (type 1/2/3) carry no IMO — we cannot create a vessel
		// record without one.  Log once per session for analyst awareness.
		if _, seen := p.discoveredMMSIs.LoadOrStore(mmsiStr, true); !seen {
			slog.Debug("AIS: untracked vessel in monitored area (awaiting type-5 for IMO)",
				"mmsi", mmsiStr,
				"name", env.MetaData.ShipName,
				"lat", env.MetaData.Latitude,
				"lon", env.MetaData.Longitude,
			)
		}
		return
	}

	pr := env.Message.PositionReport
	aisStatus := navStatusToAISStatus(pr.NavigationalStatus)

	// Parse timestamp
	ts, err := parseAISTime(env.MetaData.TimeUTC)
	if err != nil {
		ts = time.Now().UTC()
	}

	update := AISUpdate{
		IMO:       imo,
		AISStatus: aisStatus,
		Timestamp: ts,
		Lat:       env.MetaData.Latitude,
		Lon:       env.MetaData.Longitude,
	}

	if err := ProcessAISUpdate(ctx, update); err != nil {
		slog.Warn("AIS update failed", "imo", imo, "mmsi", mmsiStr, "err", err)
	} else {
		slog.Debug("AIS position updated",
			"imo", imo,
			"mmsi", mmsiStr,
			"name", env.MetaData.ShipName,
			"status", aisStatus,
			"lat", env.MetaData.Latitude,
			"lon", env.MetaData.Longitude,
		)
	}
}

// handleStaticData processes a ShipStaticData message (AIS type 5).
// These messages carry IMO number, draught, and destination — extremely valuable.
func (p *AISPoller) handleStaticData(ctx context.Context, env aisEnvelope) {
	sd := env.Message.ShipStaticData
	mmsiStr := fmt.Sprintf("%d", sd.UserID)

	imo, tracked := p.watchlist.get(mmsiStr)

	// If the AIS message carries an IMO, use it for active discovery.
	if sd.ImoNumber != 0 {
		imoFromAIS := fmt.Sprintf("%d", sd.ImoNumber)
		if !tracked {
			// Upsert the vessel and create zone-appropriate shipment.
			p.discoverVesselFromAIS(ctx, imoFromAIS, mmsiStr,
				strings.TrimSpace(sd.Name),
				env.MetaData.Latitude, env.MetaData.Longitude,
				sd.Draught,
				strings.TrimSpace(sd.Destination),
			)
			// Continue below to process the static update for the now-tracked vessel.
		}
		imo = imoFromAIS
	} else if !tracked {
		return
	}

	ts, _ := parseAISTime(env.MetaData.TimeUTC)
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	update := AISUpdate{
		IMO:         imo,
		AISStatus:   "active",
		Timestamp:   ts,
		DraftLoaded: sd.Draught,
		Lat:         env.MetaData.Latitude,
		Lon:         env.MetaData.Longitude,
	}

	// If destination matches a known Iranian port, flag it
	dest := strings.ToUpper(strings.TrimSpace(sd.Destination))
	if looksIranian(dest) {
		slog.Warn("AIS static data: Iranian destination declared",
			"imo", imo,
			"mmsi", mmsiStr,
			"name", strings.TrimSpace(sd.Name),
			"destination", dest,
		)
	}

	if err := ProcessAISUpdate(ctx, update); err != nil {
		slog.Warn("AIS static update failed", "imo", imo, "err", err)
	}
}

// discoverVesselFromAIS upserts a vessel seen in a monitored bounding box via AIS
// type-5 (ShipStaticData) — the only AIS message type that carries the IMO number.
// It adds the vessel to the in-memory watchlist immediately so subsequent position
// reports for this MMSI are tracked without waiting for the next watchlist refresh.
// Zone-based shipment creation mirrors the logic of the other OSINT pollers.
func (p *AISPoller) discoverVesselFromAIS(ctx context.Context,
	imo, mmsi, name string,
	lat, lon, draught float64,
	dest string,
) {
	vesselUID, err := osintUpsertVessel(ctx, imo, mmsi, name, "", "", "aisstream",
		lat, lon, draught, 0, 0)
	if err != nil || vesselUID == "" {
		slog.Warn("AIS discovery: vessel upsert failed", "imo", imo, "err", err)
		return
	}
	// Immediately hot-add to watchlist so the next position report is tracked.
	p.watchlist.set(mmsi, imo)

	zone := findZoneForPosition(lat, lon)
	if zone == nil {
		// Transit zone (Malacca, Indian Ocean, Hormuz).
		// If the vessel has an explicit Iranian destination in its AIS type-5,
		// create a monitoring/active shipment immediately rather than waiting
		// for it to physically enter an Iranian port zone.
		destUPPER := strings.ToUpper(strings.TrimSpace(dest))
		if looksIranian(destUPPER) {
			existing, _ := osintFindMonitoringShipment(ctx, imo)
			if existing != "" {
				return // already tracked
			}
			destUNLOCODE := parseIranianDestinationPW(destUPPER)
			destUID := ""
			if destUNLOCODE != "" {
				destUID, _ = pwResolvePort(ctx, destUNLOCODE)
			}
			status := "monitoring"
			if destUNLOCODE != "" {
				status = "active"
			}
			shipmentID := "AIS-TRANSIT-" + uuid.New().String()[:8]
			sourceRef := fmt.Sprintf(
				"Auto-discovered via AISStream in transit (Iranian AIS destination: %q); draught: %.1f m",
				dest, draught,
			)
			if err := osintCreateShipment(ctx, shipmentID, vesselUID, "", destUID, status, sourceRef); err != nil {
				slog.Warn("AIS transit discovery: shipment creation failed", "imo", imo, "err", err)
				return
			}
			slog.Info("AIS discovery: transit vessel with Iranian destination tracked",
				"id", shipmentID, "imo", imo, "vessel", name, "dest", dest, "status", status)
		} else {
			slog.Info("AIS discovery: vessel upserted (transit, no IR dest)",
				"imo", imo, "mmsi", mmsi, "name", name, "dest", dest)
		}
		return
	}

	switch zone.Country {
	case "CN":
		if osintHasRecentShipmentFromPort(ctx, imo, zone.UNLOCODE, 60) {
			return
		}
		destUNLOCODE := parseIranianDestinationPW(dest)
		status := "monitoring"
		if destUNLOCODE != "" {
			status = "active"
		}
		originUID, err := pwResolvePort(ctx, zone.UNLOCODE)
		if err != nil || originUID == "" {
			return
		}
		destUID := ""
		if destUNLOCODE != "" {
			destUID, _ = pwResolvePort(ctx, destUNLOCODE)
		}
		shipmentID := "AIS-" + uuid.New().String()[:8]
		sourceRef := fmt.Sprintf(
			"Auto-discovered via AISStream in CN zone %s; AIS dest: %q; draught: %.1f m",
			zone.UNLOCODE, dest, draught,
		)
		if err := osintCreateShipment(ctx, shipmentID, vesselUID, originUID, destUID, status, sourceRef); err != nil {
			slog.Warn("AIS discovery: shipment creation failed", "imo", imo, "err", err)
			return
		}
		slog.Info("AIS discovery: CN shipment auto-created",
			"id", shipmentID, "imo", imo, "vessel", name,
			"origin", zone.UNLOCODE, "dest", destUNLOCODE, "status", status)

	case "IR":
		osintUpsertIranPresence(ctx, imo, mmsi, name, "", "", "aisstream",
			lat, lon, draught, 0, 0, zone.UNLOCODE, "AIS")
	}
}

// buildSubscription constructs the aisstream.io subscription message.
func (p *AISPoller) buildSubscription() aisSubscription {
	boxes := make([][][2]float64, 0, len(p.cfg.AISWatchBoxes))
	for _, b := range p.cfg.AISWatchBoxes {
		// aisstream.io format: [[north_lat, west_lon], [south_lat, east_lon]]
		boxes = append(boxes, [][2]float64{
			{b.NorthLat, b.WestLon},
			{b.SouthLat, b.EastLon},
		})
	}

	sub := aisSubscription{
		APIKey:             p.cfg.AISStreamAPIKey,
		BoundingBoxes:      boxes,
		FilterMessageTypes: []string{"PositionReport", "ShipStaticData"},
	}

	// Add MMSI watchlist (max 50 per aisstream.io docs)
	mmsis := p.watchlist.mmsis()
	if len(mmsis) > 50 {
		mmsis = mmsis[:50]
	}
	if len(mmsis) > 0 {
		sub.FiltersShipMMSI = mmsis
	}

	return sub
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// looksIranian returns true when a destination string contains Iranian port
// identifiers or city names commonly declared in AIS type 5 messages.
var iranianKeywords = []string{
	"BANDAR ABBAS", "BANDARABBAS", "BANDAR_ABBAS",
	"CHABAHAR", "CHAHBAHAR",
	"BUSHEHR", "BUSHIRE",
	"KHORRAMSHAHR",
	"KHARG", "KHARK",
	"IRAN", "IR ",
	"IRBND", "IRCHB",
}

func looksIranian(dest string) bool {
	dest = strings.ToUpper(dest)
	for _, kw := range iranianKeywords {
		if strings.Contains(dest, kw) {
			return true
		}
	}
	return false
}

// parseAISTime parses the time_utc field from aisstream.io metadata.
// Format observed: "2024-05-20 09:21:31.781972101 +0000 UTC"
func parseAISTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time string")
	}
	// Truncate nanoseconds for parsing
	formats := []string{
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		time.RFC3339,
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable time: %s", s)
}
