package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/gin-gonic/gin"
	"github.com/proptrack/proptrack/internal/db"
	"github.com/proptrack/proptrack/internal/models"
)

// VesselConnections handles GET /api/v1/graph/vessel/:imo/connections
// Returns the full graph neighbourhood: vessel → company → shipments → ports.
func VesselConnections(c *gin.Context) {
	imo := c.Param("imo")
	ctx := context.Background()

	q := `query q($imo: string) {
  vessel(func: eq(vessel.imo, $imo)) {
    uid
    vessel.imo
    vessel.name
    vessel.flag
    vessel.type
    vessel.sanctioned
    vessel.sanction_programs
    vessel.ais_status
    vessel.suspicion_score
    vessel.owner_company {
      uid
      company.name
      company.country
      company.sanctioned
      company.ofac_id
      company.operates_vessels {
        uid
        vessel.imo
        vessel.name
        vessel.flag
        vessel.sanctioned
      }
    }
    ~shipment.vessel {
      uid
      shipment.id
      shipment.cargo_type
      shipment.status
      shipment.departure_time
      shipment.suspicion_score
      shipment.suspicion_flags
      shipment.origin_port {
        uid port.unlocode port.name port.country port.risk_level port.known_chemical_hub
      }
      shipment.destination_port {
        uid port.unlocode port.name port.country port.risk_level
      }
      shipment.waypoints {
        uid port.unlocode port.name port.country
      }
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

	var raw map[string]interface{}
	if err := json.Unmarshal(resp.Json, &raw); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unmarshal error"})
		return
	}
	vessels, ok := raw["vessel"].([]interface{})
	if !ok || len(vessels) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "vessel not found"})
		return
	}
	c.JSON(http.StatusOK, vessels[0])
}

// CompanyFleet handles GET /api/v1/graph/company/:name/fleet
// Returns all vessels linked to a company by name.
func CompanyFleet(c *gin.Context) {
	name := c.Param("name")
	ctx := context.Background()

	q := `query q($name: string) {
  company(func: anyofterms(company.name, $name)) {
    uid
    company.name
    company.country
    company.sanctioned
    company.ofac_id
    company.known_aliases
    company.operates_vessels {
      uid
      vessel.imo
      vessel.name
      vessel.flag
      vessel.type
      vessel.sanctioned
      vessel.ais_status
      vessel.suspicion_score
    }
  }
}`

	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$name": name})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(resp.Json, &raw); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unmarshal error"})
		return
	}
	companies, _ := raw["company"].([]interface{})
	c.JSON(http.StatusOK, gin.H{"companies": companies})
}

// RawDQLQuery handles POST /api/v1/graph/query (admin only)
// Executes a raw DQL query passed in the request body.
func RawDQLQuery(c *gin.Context) {
	adminToken := os.Getenv("API_ADMIN_TOKEN")
	if adminToken == "" {
		adminToken = "changeme"
	}
	if c.GetHeader("X-Admin-Token") != adminToken {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}

	var body struct {
		Query     string            `json:"query" binding:"required"`
		Variables map[string]string `json:"variables"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := context.Background()
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	var resp interface{}
	var err error

	if len(body.Variables) > 0 {
		dResp, e := db.NewReadOnlyTxn().QueryWithVars(ctx, body.Query, body.Variables)
		err = e
		if dResp != nil {
			var raw interface{}
			json.Unmarshal(dResp.Json, &raw)
			resp = raw
		}
	} else {
		dResp, e := txn.Query(ctx, body.Query)
		err = e
		if dResp != nil {
			var raw interface{}
			json.Unmarshal(dResp.Json, &raw)
			resp = raw
		}
	}

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("DQL error: %s", err.Error())})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// PortsByRisk handles GET /api/v1/ports/risk/:level
func PortsByRisk(c *gin.Context) {
	level := c.Param("level")
	ctx := context.Background()

	q := `query q($level: string) {
  ports(func: eq(port.risk_level, $level)) {
    uid
    port.unlocode
    port.name
    port.country
    port.risk_level
    port.known_chemical_hub
    port.coordinates_lat
    port.coordinates_lon
  }
}`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$level": level})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var result struct {
		Ports []models.Port `json:"ports"`
	}
	if err := json.Unmarshal(resp.Json, &result); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unmarshal error"})
		return
	}
	c.JSON(http.StatusOK, result.Ports)
}

// UpsertPort handles POST /api/v1/ports
func UpsertPort(c *gin.Context) {
	var req models.UpsertPortRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := context.Background()

	q := `query {
  port as var(func: eq(port.unlocode, "` + req.UNLOCODE + `"))
}`

	type portNode struct {
		UID              string   `json:"uid"`
		DgraphType       []string `json:"dgraph.type"`
		UNLOCODE         string   `json:"port.unlocode"`
		Name             string   `json:"port.name"`
		Country          string   `json:"port.country,omitempty"`
		RiskLevel        string   `json:"port.risk_level,omitempty"`
		KnownChemicalHub bool     `json:"port.known_chemical_hub,omitempty"`
		Lat              float64  `json:"port.coordinates_lat,omitempty"`
		Lon              float64  `json:"port.coordinates_lon,omitempty"`
	}

	updateNode := portNode{
		UID:              "uid(port)",
		DgraphType:       []string{"Port"},
		UNLOCODE:         req.UNLOCODE,
		Name:             req.Name,
		Country:          req.Country,
		RiskLevel:        req.RiskLevel,
		KnownChemicalHub: req.KnownChemicalHub,
		Lat:              req.Lat,
		Lon:              req.Lon,
	}
	insertNode := updateNode
	insertNode.UID = "_:port"

	bUpdate, _ := json.Marshal(updateNode)
	bInsert, _ := json.Marshal(insertNode)

	upsertReq := &api.Request{
		Query: q,
		Mutations: []*api.Mutation{
			{SetJson: bUpdate, Cond: `@if(gt(len(port), 0))`},
			{SetJson: bInsert, Cond: `@if(eq(len(port), 0))`},
		},
		CommitNow: true,
	}

	txn := db.NewTxn()
	defer txn.Discard(ctx)

	dgraphResp, err := txn.Do(ctx, upsertReq)
	if err != nil {
		slog.Error("port upsert failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	uid := ""
	for _, v := range dgraphResp.Uids {
		uid = v
		break
	}
	c.JSON(http.StatusOK, gin.H{"uid": uid, "unlocode": req.UNLOCODE, "name": req.Name})
}

// PortShipments handles GET /api/v1/ports/:unlocode/shipments
func PortShipments(c *gin.Context) {
	code := c.Param("unlocode")
	ctx := context.Background()

	q := `query q($code: string) {
  shipments(func: has(shipment.origin_port)) @filter(
    uid_in(shipment.origin_port, uid(port)) OR uid_in(shipment.destination_port, uid(port))
  ) {
    uid
    shipment.id
    shipment.cargo_type
    shipment.status
    shipment.departure_time
    shipment.suspicion_score
    shipment.vessel { vessel.imo vessel.name }
    shipment.origin_port { port.unlocode port.name }
    shipment.destination_port { port.unlocode port.name }
  }
  port as var(func: eq(port.unlocode, $code))
}`

	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$code": code})
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
