package services

// vessel_osint_shared.go — shared Dgraph helpers used by all OSINT discovery pollers
// (PortWatcher, MSTPoller, DatalasticEnricher).  None of these functions carry API
// credentials; they only know about the graph database.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/proptrack/proptrack/internal/db"
)

// osintUpsertVessel creates or updates a Vessel node from any OSINT source.
// source is a label for logging ("marinetraffic", "myshiptracking", "datalastic").
// Returns the Dgraph UID of the (created or existing) vessel node.
func osintUpsertVessel(ctx context.Context,
	imo, mmsi, name, flag, vesselType, source string,
	lat, lon, draught, grt, dwt float64,
) (string, error) {
	q := `query { vessel as var(func: eq(vessel.imo, "` + imo + `")) }`

	type node struct {
		UID         string   `json:"uid"`
		DgraphType  []string `json:"dgraph.type"`
		IMO         string   `json:"vessel.imo"`
		MMSI        string   `json:"vessel.mmsi,omitempty"`
		Name        string   `json:"vessel.name"`
		Flag        string   `json:"vessel.flag,omitempty"`
		Type        string   `json:"vessel.type,omitempty"`
		AISStatus   string   `json:"vessel.ais_status,omitempty"`
		DraftLoaded float64  `json:"vessel.draft_loaded,omitempty"`
		GRT         float64  `json:"vessel.grt,omitempty"`
		DWT         float64  `json:"vessel.dwt,omitempty"`
		LastLat     float64  `json:"vessel.last_lat,omitempty"`
		LastLon     float64  `json:"vessel.last_lon,omitempty"`
	}

	n := node{
		DgraphType:  []string{"Vessel"},
		IMO:         imo,
		MMSI:        mmsi,
		Name:        strings.TrimSpace(name),
		Flag:        flag,
		Type:        vesselType,
		AISStatus:   "active",
		DraftLoaded: draught,
		GRT:         grt,
		DWT:         dwt,
		LastLat:     lat,
		LastLon:     lon,
	}

	n.UID = "uid(vessel)"
	bUpdate, _ := json.Marshal(n)
	n.UID = "_:vessel"
	bInsert, _ := json.Marshal(n)

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
		return "", err
	}
	for _, uid := range resp.Uids {
		return uid, nil
	}
	// Node already existed — resolve its UID
	return pwResolveVessel(ctx, imo)
}

// osintHasRecentShipmentFromPort returns true if the vessel already has a non-pruned
// shipment originating from portUNLOCODE within the last maxDays days.
func osintHasRecentShipmentFromPort(ctx context.Context, imo, portUNLOCODE string, maxDays int) bool {
	cutoff := time.Now().UTC().Add(-time.Duration(maxDays) * 24 * time.Hour)
	q := fmt.Sprintf(`{
  ships(func: has(shipment.vessel))
    @filter(NOT eq(shipment.status, "pruned") AND gt(shipment.departure_time, "%s"))
    @cascade {
    uid
    shipment.vessel @filter(eq(vessel.imo, "%s")) { uid }
    shipment.origin_port @filter(eq(port.unlocode, "%s")) { uid }
  }
}`, cutoff.Format(time.RFC3339), imo, portUNLOCODE)

	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)
	resp, err := txn.Query(ctx, q)
	if err != nil {
		return false
	}
	var res struct {
		Ships []struct{ UID string `json:"uid"` } `json:"ships"`
	}
	_ = json.Unmarshal(resp.Json, &res)
	return len(res.Ships) > 0
}

// osintCreateShipment writes a new Shipment node for an auto-discovered voyage.
// originUID and destUID are optional (pass "" to omit).
// sourceRef should describe the discovery origin (e.g. API name + raw AIS destination).
func osintCreateShipment(ctx context.Context, id, vesselUID, originUID, destUID, status, sourceRef string) error {
	type uidRef struct{ UID string `json:"uid"` }
	type shipNode struct {
		UID              string    `json:"uid"`
		DgraphType       []string  `json:"dgraph.type"`
		ID               string    `json:"shipment.id"`
		Vessel           uidRef    `json:"shipment.vessel"`
		OriginPort       *uidRef   `json:"shipment.origin_port,omitempty"`
		DestinationPort  *uidRef   `json:"shipment.destination_port,omitempty"`
		DepartureTime    time.Time `json:"shipment.departure_time"`
		SuspicionScore   float64   `json:"shipment.suspicion_score"`
		Status           string    `json:"shipment.status"`
		SourceReferences string    `json:"shipment.source_references"`
	}

	n := shipNode{
		UID:              "_:shipment",
		DgraphType:       []string{"Shipment"},
		ID:               id,
		Vessel:           uidRef{UID: vesselUID},
		DepartureTime:    time.Now().UTC(),
		SuspicionScore:   0,
		Status:           status,
		SourceReferences: sourceRef,
	}
	if originUID != "" {
		n.OriginPort = &uidRef{UID: originUID}
	}
	if destUID != "" {
		n.DestinationPort = &uidRef{UID: destUID}
	}

	b, _ := json.Marshal(n)
	txn := db.NewTxn()
	defer txn.Discard(ctx)
	_, err := txn.Mutate(ctx, &api.Mutation{SetJson: b, CommitNow: true})
	return err
}

// osintFindMonitoringShipment returns the Dgraph UID of an active or monitoring
// shipment linked to the vessel identified by imo.  Returns "" if none found.
func osintFindMonitoringShipment(ctx context.Context, imo string) (string, error) {
	q := `query q($imo: string) {
  vessels(func: eq(vessel.imo, $imo)) {
    ~shipment.vessel @filter(eq(shipment.status, "monitoring") OR eq(shipment.status, "active")) {
      uid
    }
  }
}`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)
	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$imo": imo})
	if err != nil {
		return "", err
	}
	var res struct {
		Vessels []struct {
			Shipments []struct {
				UID string `json:"uid"`
			} `json:"~shipment.vessel"`
		} `json:"vessels"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return "", err
	}
	for _, v := range res.Vessels {
		for _, s := range v.Shipments {
			if s.UID != "" {
				return s.UID, nil
			}
		}
	}
	return "", nil
}

// osintConfirmIranianArrival updates shipment status to "active" and sets the
// destination port, logging the confirmation.
func osintConfirmIranianArrival(ctx context.Context, shipUID, portUID, imo, vesselName, portCode string) {
	patch := map[string]interface{}{
		"uid":                       shipUID,
		"shipment.status":           "active",
		"shipment.destination_port": map[string]interface{}{"uid": portUID},
	}
	b, _ := json.Marshal(patch)
	txn := db.NewTxn()
	if _, err := txn.Mutate(ctx, &api.Mutation{SetJson: b, CommitNow: true}); err != nil {
		txn.Discard(ctx)
		slog.Warn("osint: arrival confirmation failed", "imo", imo, "err", err)
		return
	}
	slog.Info("osint: Iranian arrival confirmed",
		"imo", imo,
		"vessel", vesselName,
		"port", portCode,
	)
}

// osintHasRecentShipmentAtPort returns true if the vessel already has a non-pruned
// shipment whose DESTINATION is portUNLOCODE, created within the last maxDays days.
// Used to prevent re-creating duplicate "arrived" records for vessels already logged at an Iranian port.
func osintHasRecentShipmentAtPort(ctx context.Context, imo, portUNLOCODE string, maxDays int) bool {
	cutoff := time.Now().UTC().Add(-time.Duration(maxDays) * 24 * time.Hour)
	q := fmt.Sprintf(`{
  ships(func: has(shipment.vessel))
    @filter(NOT eq(shipment.status, "pruned") AND gt(shipment.departure_time, "%s"))
    @cascade {
    uid
    shipment.vessel @filter(eq(vessel.imo, "%s")) { uid }
    shipment.destination_port @filter(eq(port.unlocode, "%s")) { uid }
  }
}`, cutoff.Format(time.RFC3339), imo, portUNLOCODE)

	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)
	resp, err := txn.Query(ctx, q)
	if err != nil {
		return false
	}
	var res struct {
		Ships []struct{ UID string `json:"uid"` } `json:"ships"`
	}
	_ = json.Unmarshal(resp.Json, &res)
	return len(res.Ships) > 0
}

// osintUpsertIranPresence handles a vessel detected in an Iranian port zone.
// Two cases:
//  1. An existing monitoring/active shipment is found → confirm arrival (update dest + status).
//  2. No prior shipment → vessel was never tracked from China; create a new "arrived"
//     shipment anchored at the Iranian port so it enters scoring regardless of its history.
//
// This ensures every vessel observed in an Iranian zone appears in the database,
// even those not previously spotted in Chinese port zones.
func osintUpsertIranPresence(ctx context.Context,
	imo, mmsi, name, flag, vesselType, source string,
	lat, lon, draught, grt, dwt float64,
	portUNLOCODE, shipmentIDPrefix string,
) {
	if imo == "" || imo == "0" {
		return
	}

	// Always upsert the vessel so it exists in the graph.
	vesselUID, err := osintUpsertVessel(ctx, imo, mmsi, name, flag, vesselType, source, lat, lon, draught, grt, dwt)
	if err != nil || vesselUID == "" {
		slog.Warn("osint: IR vessel upsert failed", "imo", imo, "err", err)
		return
	}

	portUID, err := pwResolvePort(ctx, portUNLOCODE)
	if err != nil || portUID == "" {
		slog.Warn("osint: IR port not found", "unlocode", portUNLOCODE)
		return
	}

	// Case 1: upgrade an existing monitoring/active shipment.
	if shipUID, _ := osintFindMonitoringShipment(ctx, imo); shipUID != "" {
		osintConfirmIranianArrival(ctx, shipUID, portUID, imo, name, portUNLOCODE)
		return
	}

	// Case 2: no prior shipment — create a direct "arrived" record.
	// Deduplication: skip if we already logged this vessel at this port within 30 days.
	if osintHasRecentShipmentAtPort(ctx, imo, portUNLOCODE, 30) {
		return
	}

	shipmentID := shipmentIDPrefix + "-IR-" + imo[len(imo)-6:]
	sourceRef := fmt.Sprintf(
		"Auto-discovered directly in Iranian port zone %s via %s (no prior CN route); flag: %s",
		portUNLOCODE, source, flag,
	)
	if err := osintCreateShipment(ctx, shipmentID, vesselUID, "", portUID, "arrived", sourceRef); err != nil {
		slog.Warn("osint: IR direct-arrival shipment creation failed", "imo", imo, "err", err)
		return
	}
	slog.Info("osint: direct Iranian arrival recorded",
		"id", shipmentID, "imo", imo, "vessel", name,
		"port", portUNLOCODE, "flag", flag, "source", source,
	)
}

// osintPruneNonIranRoutes marks monitoring shipments older than pruneDays as "pruned"
// when they have no Iranian port as destination or waypoint.
func osintPruneNonIranRoutes(ctx context.Context, pruneDays int) int {
	cutoff := time.Now().UTC().Add(-time.Duration(pruneDays) * 24 * time.Hour)
	q := fmt.Sprintf(`{
  shipments(func: eq(shipment.status, "monitoring"))
    @filter(lt(shipment.departure_time, "%s")) {
    uid
    shipment.id
    shipment.destination_port { port.country }
    shipment.waypoints       { port.country }
  }
}`, cutoff.Format(time.RFC3339))

	txn := db.NewReadOnlyTxn()
	resp, err := txn.Query(ctx, q)
	txn.Discard(ctx)
	if err != nil {
		slog.Warn("osint: prune query failed", "err", err)
		return 0
	}

	var res struct {
		Shipments []struct {
			UID  string `json:"uid"`
			ID   string `json:"shipment.id"`
			Dest *struct {
				Country string `json:"port.country"`
			} `json:"shipment.destination_port"`
			Waypoints []struct {
				Country string `json:"port.country"`
			} `json:"shipment.waypoints"`
		} `json:"shipments"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return 0
	}

	pruned := 0
	for _, s := range res.Shipments {
		hasIran := false
		if s.Dest != nil && strings.ToUpper(s.Dest.Country) == "IR" {
			hasIran = true
		}
		for _, wp := range s.Waypoints {
			if strings.ToUpper(wp.Country) == "IR" {
				hasIran = true
				break
			}
		}
		if hasIran {
			continue
		}

		patch := map[string]interface{}{
			"uid":             s.UID,
			"shipment.status": "pruned",
		}
		b, _ := json.Marshal(patch)
		txn2 := db.NewTxn()
		if _, err := txn2.Mutate(ctx, &api.Mutation{SetJson: b, CommitNow: true}); err != nil {
			txn2.Discard(ctx)
			slog.Warn("osint: prune mutation failed", "id", s.ID, "err", err)
			continue
		}
		slog.Info("osint: route pruned (no Iran link)", "id", s.ID)
		pruned++
	}
	return pruned
}
