package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/gin-gonic/gin"
	"github.com/proptrack/proptrack/internal/db"
	"github.com/proptrack/proptrack/internal/models"
	"github.com/proptrack/proptrack/internal/services"
)

// RecomputeVesselScore handles POST /api/v1/score/vessel/:imo
// Fetches all active shipments for the vessel and recomputes their scores.
func RecomputeVesselScore(c *gin.Context) {
	imo := c.Param("imo")
	ctx := context.Background()

	uid, err := resolveVesselByIMO(ctx, imo)
	if err != nil || uid == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("vessel %s not found", imo)})
		return
	}

	// Fetch vessel details for vessel-level score
	q := `query q($imo: string) {
  vessel(func: eq(vessel.imo, $imo)) {
    uid
    vessel.imo
    vessel.name
    vessel.flag
    vessel.type
    vessel.sanctioned
    vessel.draft_loaded
    vessel.draft_ballast
    vessel.last_dark_lat
    vessel.last_dark_lon
    vessel.owner_company {
      uid
      company.name
      company.sanctioned
    }
  }
}`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$imo": imo})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var vResult struct {
		Vessel []models.Vessel `json:"vessel"`
	}
	if err := json.Unmarshal(resp.Json, &vResult); err != nil || len(vResult.Vessel) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "vessel not found"})
		return
	}
	vessel := vResult.Vessel[0]

	// Compute a proxy score using an empty shipment carrying vessel details
	proxyShipment := models.Shipment{
		ID:     "proxy-" + imo,
		Vessel: &vessel,
	}
	result := services.ComputeShipmentScore(proxyShipment)

	// Persist vessel-level score
	patch := map[string]interface{}{
		"uid":                  vessel.UID,
		"vessel.suspicion_score": float64(result.TotalScore),
	}
	b, _ := json.Marshal(patch)
	txn2 := db.NewTxn()
	defer txn2.Discard(ctx)
	if _, err := txn2.Mutate(ctx, &api.Mutation{SetJson: b, CommitNow: true}); err != nil {
		slog.Error("failed to persist vessel score", "imo", imo, "err", err)
	}

	c.JSON(http.StatusOK, gin.H{"imo": imo, "score": result})
}

// RecomputeShipmentScore handles POST /api/v1/score/shipment/:id
func RecomputeShipmentScore(c *gin.Context) {
	id := c.Param("id")
	ctx := context.Background()

	shipment, err := fetchFullShipment(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if shipment == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "shipment not found"})
		return
	}

	// Check for prior Iran deliveries by this vessel
	if shipment.Vessel != nil {
		hasPrior, err := vesselHasPriorIranDelivery(ctx, shipment.Vessel.UID, shipment.UID)
		if err != nil {
			slog.Warn("prior delivery check failed", "err", err)
		}
		if hasPrior {
			if shipment.SuspicionFlags != "" {
				shipment.SuspicionFlags += " prior_iran_delivery"
			} else {
				shipment.SuspicionFlags = "prior_iran_delivery"
			}
		}
	}

	result := services.ComputeShipmentScore(*shipment)

	// Persist computed score and flags back to Dgraph
	uid := shipment.UID
	flagsStr := joinFlags(result.TriggeredFlags)
	patch := map[string]interface{}{
		"uid":                    uid,
		"shipment.suspicion_score": float64(result.TotalScore),
		"shipment.suspicion_flags": flagsStr,
	}
	b, _ := json.Marshal(patch)
	txn := db.NewTxn()
	defer txn.Discard(ctx)
	if _, err := txn.Mutate(ctx, &api.Mutation{SetJson: b, CommitNow: true}); err != nil {
		slog.Error("failed to persist shipment score", "id", id, "err", err)
	}

	// Generate alert if threshold crossed
	alert := services.GenerateAlertIfWarranted(ctx, *shipment, result)
	c.JSON(http.StatusOK, gin.H{"shipment_id": id, "score": result, "alert": alert})
}

// RecomputeAllShipments handles POST /api/v1/score/recompute-all
// Triggers a bulk recompute of suspicion scores for every active shipment.
func RecomputeAllShipments(c *gin.Context) {
	ctx := context.Background()
	updated, failed := services.RecomputeAllActive(ctx)
	c.JSON(http.StatusOK, gin.H{"updated": updated, "failed": failed})
}

// ScoreLeaderboard handles GET /api/v1/score/leaderboard
// Returns the top 20 active shipments sorted by descending suspicion score.
func ScoreLeaderboard(c *gin.Context) {
	ctx := context.Background()
	q := `{
  shipments(func: anyofterms(shipment.status, "active arrived monitoring"), orderdesc: shipment.suspicion_score, first: 20) {
    uid
    shipment.id
    shipment.cargo_type
    shipment.suspicion_score
    shipment.suspicion_flags
    shipment.status
    shipment.departure_time
    shipment.vessel {
      vessel.imo
      vessel.name
      vessel.flag
      vessel.sanctioned
    }
    shipment.origin_port {
      port.unlocode
      port.name
      port.country
    }
    shipment.destination_port {
      port.unlocode
      port.name
      port.country
    }
  }
}`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Query(ctx, q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var result struct {
		Shipments []models.Shipment `json:"shipments"`
	}
	if err := json.Unmarshal(resp.Json, &result); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unmarshal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"leaderboard": result.Shipments, "count": len(result.Shipments)})
}

// vesselHasPriorIranDelivery checks whether a vessel has previously arrived at an Iranian port.
func vesselHasPriorIranDelivery(ctx context.Context, vesselUID, excludeShipmentUID string) (bool, error) {
	iranPorts := `["IRCHB","IRBND","IRKHO","IRBUZ"]`
	q := fmt.Sprintf(`{
  shipments(func: has(shipment.vessel)) @filter(
    uid_in(shipment.vessel, %s)
    AND eq(shipment.status, "arrived")
    AND NOT uid(%s)
  ) {
    uid
    shipment.destination_port @filter(regexp(port.country, /^IR/)) {
      uid
      port.unlocode
    }
  }
}`, vesselUID, excludeShipmentUID)
	_ = iranPorts

	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Query(ctx, q)
	if err != nil {
		return false, err
	}
	var res struct {
		Shipments []struct {
			DestinationPort *models.Port `json:"shipment.destination_port"`
		} `json:"shipments"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return false, err
	}
	for _, s := range res.Shipments {
		if s.DestinationPort != nil && s.DestinationPort.UID != "" {
			return true, nil
		}
	}
	return false, nil
}

func joinFlags(flags []string) string {
	if len(flags) == 0 {
		return ""
	}
	out := ""
	for i, f := range flags {
		if i > 0 {
			out += " "
		}
		out += f
	}
	return out
}
