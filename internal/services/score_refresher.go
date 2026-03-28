package services

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/proptrack/proptrack/internal/db"
	"github.com/proptrack/proptrack/internal/models"
)

// RecomputeAllActive fetches every active shipment, recomputes its suspicion
// score and flags, and persists the result back to Dgraph.
// Returns the number of successfully updated shipments and the number of errors.
// Called from main.go after seeding AND exposed as POST /api/v1/score/recompute-all.
func RecomputeAllActive(ctx context.Context) (updated int, failed int) {
	ids, err := listActiveShipmentIDs(ctx)
	if err != nil {
		slog.Error("recompute-all: cannot list active shipments", "err", err)
		return
	}
	slog.Info("recompute-all: starting", "count", len(ids))

	for _, id := range ids {
		shipment, err := fetchShipmentForScoring(ctx, id)
		if err != nil || shipment == nil {
			slog.Warn("recompute-all: fetch failed", "id", id, "err", err)
			failed++
			continue
		}

		result := ComputeShipmentScore(*shipment)
		flagsStr := strings.Join(result.TriggeredFlags, " ")

		patch := map[string]interface{}{
			"uid":                     shipment.UID,
			"shipment.suspicion_score": float64(result.TotalScore),
			"shipment.suspicion_flags": flagsStr,
		}
		b, _ := json.Marshal(patch)
		txn := db.NewTxn()
		if _, err := txn.Mutate(ctx, &api.Mutation{SetJson: b, CommitNow: true}); err != nil {
			txn.Discard(ctx)
			slog.Warn("recompute-all: persist failed", "id", id, "err", err)
			failed++
			continue
		}

		slog.Debug("recompute-all: scored",
			"id", id, "score", result.TotalScore, "level", result.Threshold)
		updated++
	}

	slog.Info("recompute-all: complete", "updated", updated, "failed", failed)
	return
}

// listActiveShipmentIDs returns all shipment IDs with status active, arrived, or monitoring.
func listActiveShipmentIDs(ctx context.Context) ([]string, error) {
	q := `{ shipments(func: anyofterms(shipment.status, "active arrived monitoring")) { shipment.id } }`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	var res struct {
		Shipments []struct {
			ID string `json:"shipment.id"`
		} `json:"shipments"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res.Shipments))
	for _, s := range res.Shipments {
		if s.ID != "" {
			ids = append(ids, s.ID)
		}
	}
	return ids, nil
}

// StartHourlyRefresh runs RecomputeAllActive every hour in the background.
// Blocks until ctx is cancelled.
func StartHourlyRefresh(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	slog.Info("score refresher: hourly refresh started")
	for {
		select {
		case <-ticker.C:
			updated, failed := RecomputeAllActive(ctx)
			slog.Info("score refresher: hourly recompute", "updated", updated, "failed", failed)
		case <-ctx.Done():
			slog.Info("score refresher: stopped")
			return
		}
	}
}

// fetchShipmentForScoring builds the full shipment subgraph needed by ComputeShipmentScore.
// Mirrors fetchFullShipment in the handlers package but lives in services to avoid
// circular imports. Includes all fields used by every scoring criterion.
func fetchShipmentForScoring(ctx context.Context, id string) (*models.Shipment, error) {
	q := `query q($id: string) {
  shipment(func: eq(shipment.id, $id)) {
    uid
    shipment.id
    shipment.cargo_type
    shipment.transport_mode
    shipment.hs_code
    shipment.cargo_weight_tons
    shipment.ais_dark_segments
    shipment.suspicion_score
    shipment.suspicion_flags
    shipment.status
    shipment.departure_time
    shipment.estimated_arrival
    shipment.vessel {
      uid
      vessel.imo
      vessel.mmsi
      vessel.name
      vessel.flag
      vessel.type
      vessel.sanctioned
      vessel.sanction_programs
      vessel.ais_status
      vessel.draft_loaded
      vessel.draft_ballast
      vessel.last_dark_lat
      vessel.last_dark_lon
      vessel.verify_discrepancies
      vessel.verify_status
      vessel.owner_company {
        uid
        company.name
        company.country
        company.sanctioned
        company.ofac_id
        company.known_aliases
      }
    }
    shipment.origin_port {
      uid
      port.unlocode
      port.name
      port.country
      port.risk_level
      port.known_chemical_hub
      port.chemical_risk_score
      port.outside_hormuz
      port.linked_companies {
        uid
        company.name
        company.country
        company.sanctioned
      }
    }
    shipment.destination_port {
      uid
      port.unlocode
      port.name
      port.country
      port.risk_level
      port.outside_hormuz
    }
    shipment.waypoints {
      uid
      port.unlocode
      port.name
      port.country
      port.risk_level
    }
  }
}`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$id": id})
	if err != nil {
		return nil, err
	}
	var result struct {
		Shipment []models.Shipment `json:"shipment"`
	}
	if err := json.Unmarshal(resp.Json, &result); err != nil {
		return nil, err
	}
	if len(result.Shipment) == 0 {
		return nil, nil
	}
	return &result.Shipment[0], nil
}
