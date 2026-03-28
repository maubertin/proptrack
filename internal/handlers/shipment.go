package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/proptrack/proptrack/internal/db"
	"github.com/proptrack/proptrack/internal/models"
	"github.com/proptrack/proptrack/internal/services"
)

// CreateShipment handles POST /api/v1/shipments
func CreateShipment(c *gin.Context) {
	var req models.CreateShipmentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := context.Background()

	vesselUID, err := resolveVesselByIMO(ctx, req.VesselIMO)
	if err != nil || vesselUID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("vessel IMO %s not found", req.VesselIMO)})
		return
	}
	originUID, err := resolvePortByCode(ctx, req.OriginUNLOCODE)
	if err != nil || originUID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("origin port %s not found", req.OriginUNLOCODE)})
		return
	}
	destUID, err := resolvePortByCode(ctx, req.DestUNLOCODE)
	if err != nil || destUID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("destination port %s not found", req.DestUNLOCODE)})
		return
	}

	shipmentID := uuid.New().String()

	type uidRef struct{ UID string `json:"uid"` }
	type shipmentNode struct {
		UID              string    `json:"uid"`
		DgraphType       []string  `json:"dgraph.type"`
		ID               string    `json:"shipment.id"`
		Vessel           uidRef    `json:"shipment.vessel"`
		OriginPort       uidRef    `json:"shipment.origin_port"`
		DestinationPort  uidRef    `json:"shipment.destination_port"`
		DepartureTime    time.Time `json:"shipment.departure_time"`
		EstimatedArrival time.Time `json:"shipment.estimated_arrival,omitempty"`
		CargoType        string    `json:"shipment.cargo_type,omitempty"`
		CargoWeightTons  float64   `json:"shipment.cargo_weight_tons,omitempty"`
		AISDarkSegments  int       `json:"shipment.ais_dark_segments"`
		SuspicionScore   float64   `json:"shipment.suspicion_score"`
		Status           string    `json:"shipment.status"`
		SourceReferences string    `json:"shipment.source_references,omitempty"`
		TransportMode    string    `json:"shipment.transport_mode,omitempty"`
		HSCode           string    `json:"shipment.hs_code,omitempty"`
	}

	node := shipmentNode{
		UID:              "_:shipment",
		DgraphType:       []string{"Shipment"},
		ID:               shipmentID,
		Vessel:           uidRef{UID: vesselUID},
		OriginPort:       uidRef{UID: originUID},
		DestinationPort:  uidRef{UID: destUID},
		DepartureTime:    req.DepartureTime,
		EstimatedArrival: req.EstimatedArrival,
		CargoType:        req.CargoType,
		CargoWeightTons:  req.CargoWeightTons,
		AISDarkSegments:  0,
		SuspicionScore:   0,
		Status:           "active",
		SourceReferences: req.SourceReferences,
		TransportMode:    req.TransportMode,
		HSCode:           req.HSCode,
	}

	b, err := json.Marshal(node)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal error"})
		return
	}

	txn := db.NewTxn()
	defer txn.Discard(ctx)

	mu := &api.Mutation{SetJson: b, CommitNow: true}
	resp, err := txn.Mutate(ctx, mu)
	if err != nil {
		slog.Error("shipment create failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	newUID := ""
	for _, v := range resp.Uids {
		newUID = v
		break
	}

	slog.Info("shipment created", "id", shipmentID, "uid", newUID)
	c.JSON(http.StatusCreated, gin.H{"uid": newUID, "shipment_id": shipmentID, "status": "active"})
}

// GetShipment handles GET /api/v1/shipments/:id
func GetShipment(c *gin.Context) {
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

	result := services.ComputeShipmentScore(*shipment)
	c.JSON(http.StatusOK, gin.H{"shipment": shipment, "score": result})
}

// ListActiveShipments handles GET /api/v1/shipments/active
// Optional query params: from=RFC3339, to=RFC3339, order=asc|desc (default: desc)
func ListActiveShipments(c *gin.Context) {
	ctx := context.Background()

	fromStr := c.Query("from")
	toStr := c.Query("to")
	order := "orderdesc"
	if c.Query("order") == "asc" {
		order = "orderasc"
	}

	// Build optional date range filter
	dateFilter := ""
	if fromStr != "" || toStr != "" {
		parts := []string{}
		if fromStr != "" {
			parts = append(parts, fmt.Sprintf(`gt(shipment.departure_time, "%s")`, fromStr))
		}
		if toStr != "" {
			parts = append(parts, fmt.Sprintf(`lt(shipment.departure_time, "%s")`, toStr))
		}
		dateFilter = " AND " + strings.Join(parts, " AND ")
	}

	q := fmt.Sprintf(`{
  shipments(func: anyofterms(shipment.status, "active arrived monitoring"), %s: shipment.suspicion_score)
    @filter(NOT eq(shipment.status, "pruned")%s) {
    uid
    shipment.id
    shipment.cargo_type
    shipment.transport_mode
    shipment.cargo_weight_tons
    shipment.ais_dark_segments
    shipment.suspicion_score
    shipment.suspicion_flags
    shipment.status
    shipment.departure_time
    shipment.estimated_arrival
    shipment.vessel { uid vessel.imo vessel.name vessel.flag vessel.sanctioned vessel.ais_status vessel.grt vessel.dwt }
    shipment.origin_port { uid port.unlocode port.name port.country port.risk_level }
    shipment.destination_port { uid port.unlocode port.name port.country port.risk_level }
  }
}`, order, dateFilter)

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
	c.JSON(http.StatusOK, result.Shipments)
}

// UpdateShipmentStatus handles PUT /api/v1/shipments/:id/status
func UpdateShipmentStatus(c *gin.Context) {
	id := c.Param("id")
	var req models.UpdateStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := context.Background()
	uid, err := resolveShipmentUID(ctx, id)
	if err != nil || uid == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "shipment not found"})
		return
	}

	patch := map[string]interface{}{
		"uid":              uid,
		"shipment.status":  req.Status,
	}
	if !req.ActualArrival.IsZero() {
		patch["shipment.actual_arrival"] = req.ActualArrival
	}

	b, _ := json.Marshal(patch)
	txn := db.NewTxn()
	defer txn.Discard(ctx)

	if _, err := txn.Mutate(ctx, &api.Mutation{SetJson: b, CommitNow: true}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"shipment_id": id, "status": req.Status})
}

// ListCriticalShipments handles GET /api/v1/shipments/critical
func ListCriticalShipments(c *gin.Context) {
	ctx := context.Background()
	q := `{
  shipments(func: ge(shipment.suspicion_score, 76.0), orderdesc: shipment.suspicion_score) {
    uid
    shipment.id
    shipment.cargo_type
    shipment.suspicion_score
    shipment.suspicion_flags
    shipment.status
    shipment.vessel { uid vessel.imo vessel.name vessel.sanctioned }
    shipment.origin_port { uid port.unlocode port.name port.country }
    shipment.destination_port { uid port.unlocode port.name port.country }
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
	c.JSON(http.StatusOK, result.Shipments)
}

// AddWaypoint handles POST /api/v1/shipments/:id/waypoint
func AddWaypoint(c *gin.Context) {
	id := c.Param("id")
	var req models.AddWaypointRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := context.Background()
	shipUID, err := resolveShipmentUID(ctx, id)
	if err != nil || shipUID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "shipment not found"})
		return
	}
	portUID, err := resolvePortByCode(ctx, req.PortUNLOCODE)
	if err != nil || portUID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("port %s not found", req.PortUNLOCODE)})
		return
	}

	type wpRef struct{ UID string `json:"uid"` }
	patch := map[string]interface{}{
		"uid":               shipUID,
		"shipment.waypoints": wpRef{UID: portUID},
	}
	b, _ := json.Marshal(patch)

	txn := db.NewTxn()
	defer txn.Discard(ctx)

	if _, err := txn.Mutate(ctx, &api.Mutation{SetJson: b, CommitNow: true}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"shipment_id": id, "waypoint_added": req.PortUNLOCODE})
}

// fetchFullShipment queries Dgraph for a complete shipment subgraph.
func fetchFullShipment(ctx context.Context, id string) (*models.Shipment, error) {
	q := `query q($id: string) {
  shipment(func: eq(shipment.id, $id)) {
    uid
    dgraph.type
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
    shipment.actual_arrival
    shipment.source_references
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
      vessel.suspicion_score
      vessel.verify_discrepancies
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
      port.coordinates_lat
      port.coordinates_lon
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
      port.coordinates_lat
      port.coordinates_lon
    }
    shipment.waypoints {
      uid
      port.unlocode
      port.name
      port.country
      port.risk_level
      port.coordinates_lat
      port.coordinates_lon
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

// resolveVesselByIMO returns the UID of a vessel by its IMO number.
func resolveVesselByIMO(ctx context.Context, imo string) (string, error) {
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

// resolvePortByCode returns the UID of a port by its UN/LOCODE.
func resolvePortByCode(ctx context.Context, code string) (string, error) {
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

// resolveShipmentUID returns the Dgraph UID for a shipment by its string ID.
func resolveShipmentUID(ctx context.Context, id string) (string, error) {
	q := `query q($id: string) { shipment(func: eq(shipment.id, $id)) { uid } }`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)
	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$id": id})
	if err != nil {
		return "", err
	}
	var res struct {
		Shipment []struct{ UID string `json:"uid"` } `json:"shipment"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return "", err
	}
	if len(res.Shipment) == 0 {
		return "", nil
	}
	return res.Shipment[0].UID, nil
}
