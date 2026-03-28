package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/proptrack/proptrack/internal/db"
)

// AISUpdate represents an incoming AIS position report.
type AISUpdate struct {
	IMO          string    `json:"imo"`
	MMSI         string    `json:"mmsi"`
	AISStatus    string    `json:"ais_status"` // "active", "dark", "spoofing"
	Timestamp    time.Time `json:"timestamp"`
	PortUNLOCODE string    `json:"port_unlocode"`
	DraftLoaded  float64   `json:"draft_loaded"`
	DraftBallast float64   `json:"draft_ballast"`
	Lat          float64   `json:"lat"`
	Lon          float64   `json:"lon"`
}

// ProcessAISUpdate applies an AIS position update to Dgraph and handles
// AIS dark detection: when a vessel goes dark mid-voyage, the active shipment's
// ais_dark_segments counter is incremented.
func ProcessAISUpdate(ctx context.Context, update AISUpdate) error {
	// 1. Resolve vessel UID by IMO
	vesselUID, err := resolveVesselUID(ctx, update.IMO)
	if err != nil {
		return fmt.Errorf("resolve vessel: %w", err)
	}
	if vesselUID == "" {
		return fmt.Errorf("vessel with IMO %s not found", update.IMO)
	}

	// 2. Resolve port UID if provided
	portUID := ""
	if update.PortUNLOCODE != "" {
		portUID, err = resolvePortUID(ctx, update.PortUNLOCODE)
		if err != nil {
			slog.Warn("port not found for AIS update", "unlocode", update.PortUNLOCODE)
		}
	}

	// 3. Fetch current AIS status to detect transition to dark
	prevStatus, err := getVesselAISStatus(ctx, vesselUID)
	if err != nil {
		slog.Warn("could not fetch previous AIS status", "err", err)
	}
	wentDark := prevStatus == "active" && update.AISStatus == "dark"

	// 4. Build vessel update mutation
	type portStub struct {
		UID string `json:"uid"`
	}
	type vesselPatch struct {
		UID          string    `json:"uid"`
		AISStatus    string    `json:"vessel.ais_status,omitempty"`
		LastSeenTime time.Time `json:"vessel.last_seen_time,omitempty"`
		LastLat      float64   `json:"vessel.last_lat,omitempty"`
		LastLon      float64   `json:"vessel.last_lon,omitempty"`
		LastDarkLat  float64   `json:"vessel.last_dark_lat,omitempty"`
		LastDarkLon  float64   `json:"vessel.last_dark_lon,omitempty"`
		DraftLoaded  float64   `json:"vessel.draft_loaded,omitempty"`
		DraftBallast float64   `json:"vessel.draft_ballast,omitempty"`
		LastSeenPort *portStub `json:"vessel.last_seen_port,omitempty"`
	}

	patch := vesselPatch{
		UID:          vesselUID,
		AISStatus:    update.AISStatus,
		LastSeenTime: update.Timestamp,
		DraftLoaded:  update.DraftLoaded,
		DraftBallast: update.DraftBallast,
	}
	if update.Lat != 0 || update.Lon != 0 {
		patch.LastLat = update.Lat
		patch.LastLon = update.Lon
	}
	// Capture the last known position at the moment the transponder goes dark
	if wentDark && (update.Lat != 0 || update.Lon != 0) {
		patch.LastDarkLat = update.Lat
		patch.LastDarkLon = update.Lon
	}
	if portUID != "" {
		patch.LastSeenPort = &portStub{UID: portUID}
	}

	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	txn := db.NewTxn()
	defer txn.Discard(ctx)

	mu := &api.Mutation{SetJson: b, CommitNow: true}
	if _, err := txn.Mutate(ctx, mu); err != nil {
		return fmt.Errorf("vessel AIS update: %w", err)
	}

	slog.Info("AIS update applied",
		"imo", update.IMO,
		"status", update.AISStatus,
		"port", update.PortUNLOCODE,
	)

	// 5. If vessel went dark, increment dark segments on active shipment
	if wentDark {
		if err := incrementDarkSegments(ctx, vesselUID); err != nil {
			slog.Error("failed to increment dark segments", "vessel_uid", vesselUID, "err", err)
		} else {
			slog.Warn("vessel went AIS-dark mid-voyage, dark segment recorded",
				"imo", update.IMO,
			)
		}
	}

	return nil
}

// incrementDarkSegments finds the vessel's active shipment and increments
// the ais_dark_segments counter by 1 using a DQL upsert.
func incrementDarkSegments(ctx context.Context, vesselUID string) error {
	q := fmt.Sprintf(`
{
  shipment(func: has(shipment.vessel)) @filter(uid_in(shipment.vessel, %s) AND eq(shipment.status, "active")) {
    uid
    shipment.ais_dark_segments
  }
}`, vesselUID)

	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Query(ctx, q)
	if err != nil {
		return err
	}

	var result struct {
		Shipment []struct {
			UID             string `json:"uid"`
			AISDarkSegments int    `json:"shipment.ais_dark_segments"`
		} `json:"shipment"`
	}
	if err := json.Unmarshal(resp.Json, &result); err != nil {
		return err
	}
	if len(result.Shipment) == 0 {
		return nil // no active shipment, nothing to update
	}

	for _, sh := range result.Shipment {
		newCount := sh.AISDarkSegments + 1
		patch := map[string]interface{}{
			"uid":                     sh.UID,
			"shipment.ais_dark_segments": newCount,
		}
		b, _ := json.Marshal(patch)

		txn2 := db.NewTxn()
		mu := &api.Mutation{SetJson: b, CommitNow: true}
		if _, err := txn2.Mutate(ctx, mu); err != nil {
			txn2.Discard(ctx)
			return fmt.Errorf("increment dark segments for %s: %w", sh.UID, err)
		}
		slog.Info("dark segment recorded", "shipment_uid", sh.UID, "total_dark_segments", newCount)
	}
	return nil
}

// resolveVesselUID returns the Dgraph UID for a vessel identified by IMO.
func resolveVesselUID(ctx context.Context, imo string) (string, error) {
	q := `query q($imo: string) {
  vessel(func: eq(vessel.imo, $imo)) { uid }
}`
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

// resolvePortUID returns the Dgraph UID for a port identified by UN/LOCODE.
func resolvePortUID(ctx context.Context, unlocode string) (string, error) {
	q := `query q($code: string) {
  port(func: eq(port.unlocode, $code)) { uid }
}`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$code": unlocode})
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
		return "", fmt.Errorf("port not found: %s", unlocode)
	}
	return res.Port[0].UID, nil
}

// getVesselAISStatus returns the current ais_status string for a vessel UID.
func getVesselAISStatus(ctx context.Context, vesselUID string) (string, error) {
	q := fmt.Sprintf(`{ v(func: uid(%s)) { vessel.ais_status } }`, vesselUID)
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Query(ctx, q)
	if err != nil {
		return "", err
	}
	var res struct {
		V []struct {
			Status string `json:"vessel.ais_status"`
		} `json:"v"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return "", err
	}
	if len(res.V) == 0 {
		return "", nil
	}
	return res.V[0].Status, nil
}
