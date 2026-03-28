package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/gin-gonic/gin"
	"github.com/proptrack/proptrack/internal/db"
	"github.com/proptrack/proptrack/internal/models"
	"github.com/proptrack/proptrack/internal/services"
)

// Verifier is injected from main at startup.
var Verifier *services.VesselVerifier

// CreateVessel handles POST /api/v1/vessels
func CreateVessel(c *gin.Context) {
	var req models.CreateVesselRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := context.Background()

	// Resolve or create owner company
	companyUID := ""
	if req.OwnerCompanyName != "" {
		uid, err := resolveOrCreateCompany(ctx, req.OwnerCompanyName)
		if err != nil {
			slog.Error("company resolution failed", "err", err)
		} else {
			companyUID = uid
		}
	}

	// Build upsert: find existing vessel by IMO, update or insert
	q := `query {
  vessel as var(func: eq(vessel.imo, "` + req.IMO + `"))
}`

	type uidRef struct {
		UID string `json:"uid"`
	}
	type vesselNode struct {
		UID              string   `json:"uid"`
		DgraphType       []string `json:"dgraph.type"`
		IMO              string   `json:"vessel.imo"`
		Name             string   `json:"vessel.name"`
		Flag             string   `json:"vessel.flag,omitempty"`
		Type             string   `json:"vessel.type,omitempty"`
		Sanctioned       bool     `json:"vessel.sanctioned,omitempty"`
		SanctionPrograms string   `json:"vessel.sanction_programs,omitempty"`
		AISStatus        string   `json:"vessel.ais_status,omitempty"`
		DraftLoaded      float64  `json:"vessel.draft_loaded,omitempty"`
		DraftBallast     float64  `json:"vessel.draft_ballast,omitempty"`
		OwnerCompany     *uidRef  `json:"vessel.owner_company,omitempty"`
	}

	node := vesselNode{
		UID:              "uid(vessel)",
		DgraphType:       []string{"Vessel"},
		IMO:              req.IMO,
		Name:             req.Name,
		Flag:             req.Flag,
		Type:             req.Type,
		Sanctioned:       req.Sanctioned,
		SanctionPrograms: req.SanctionPrograms,
		AISStatus:        req.AISStatus,
		DraftLoaded:      req.DraftLoaded,
		DraftBallast:     req.DraftBallast,
	}
	if companyUID != "" {
		node.OwnerCompany = &uidRef{UID: companyUID}
	}

	b, err := json.Marshal(node)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal error"})
		return
	}

	// Blank node fallback for new vessels
	newNode := vesselNode{
		UID:              "_:vessel",
		DgraphType:       []string{"Vessel"},
		IMO:              req.IMO,
		Name:             req.Name,
		Flag:             req.Flag,
		Type:             req.Type,
		Sanctioned:       req.Sanctioned,
		SanctionPrograms: req.SanctionPrograms,
		AISStatus:        req.AISStatus,
		DraftLoaded:      req.DraftLoaded,
		DraftBallast:     req.DraftBallast,
	}
	if companyUID != "" {
		newNode.OwnerCompany = &uidRef{UID: companyUID}
	}
	bNew, _ := json.Marshal(newNode)

	upsertMu := &api.Mutation{
		SetJson: b,
		Cond:    `@if(gt(len(vessel), 0))`,
	}
	insertMu := &api.Mutation{
		SetJson: bNew,
		Cond:    `@if(eq(len(vessel), 0))`,
	}

	req2 := &api.Request{
		Query:        q,
		Mutations:    []*api.Mutation{upsertMu, insertMu},
		CommitNow:    true,
	}

	txn := db.NewTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Do(ctx, req2)
	if err != nil {
		slog.Error("vessel upsert failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	uid := ""
	for k, v := range resp.Uids {
		_ = k
		uid = v
		break
	}

	slog.Info("vessel upserted", "imo", req.IMO, "uid", uid)
	c.JSON(http.StatusOK, gin.H{"uid": uid, "imo": req.IMO, "name": req.Name})
}

// GetVessel handles GET /api/v1/vessels/:imo
func GetVessel(c *gin.Context) {
	imo := c.Param("imo")
	ctx := context.Background()

	q := `query q($imo: string) {
  vessel(func: eq(vessel.imo, $imo)) {
    uid
    dgraph.type
    vessel.imo
    vessel.name
    vessel.flag
    vessel.type
    vessel.sanctioned
    vessel.sanction_programs
    vessel.ais_status
    vessel.last_seen_time
    vessel.draft_loaded
    vessel.draft_ballast
    vessel.suspicion_score
    vessel.owner_company {
      uid
      company.name
      company.country
      company.sanctioned
      company.ofac_id
      company.known_aliases
    }
    vessel.last_seen_port {
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

	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$imo": imo})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var result struct {
		Vessel []models.Vessel `json:"vessel"`
	}
	if err := json.Unmarshal(resp.Json, &result); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unmarshal error"})
		return
	}
	if len(result.Vessel) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "vessel not found"})
		return
	}
	c.JSON(http.StatusOK, result.Vessel[0])
}

// UpdateVesselPosition handles PUT /api/v1/vessels/:imo/position
func UpdateVesselPosition(c *gin.Context) {
	imo := c.Param("imo")
	var req models.UpdatePositionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.LastSeenTime.IsZero() {
		req.LastSeenTime = time.Now().UTC()
	}

	ctx := context.Background()
	update := services.AISUpdate{
		IMO:          imo,
		AISStatus:    req.AISStatus,
		Timestamp:    req.LastSeenTime,
		PortUNLOCODE: req.LastPortCode,
		DraftLoaded:  req.DraftLoaded,
		DraftBallast: req.DraftBallast,
	}

	if err := services.ProcessAISUpdate(ctx, update); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"imo": imo, "status": req.AISStatus})
}

// ListSanctionedVessels handles GET /api/v1/vessels/sanctioned
func ListSanctionedVessels(c *gin.Context) {
	ctx := context.Background()
	q := `{
  vessels(func: eq(vessel.sanctioned, true)) {
    uid
    vessel.imo
    vessel.name
    vessel.flag
    vessel.type
    vessel.sanctioned
    vessel.sanction_programs
    vessel.ais_status
    vessel.suspicion_score
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
		Vessels []models.Vessel `json:"vessels"`
	}
	if err := json.Unmarshal(resp.Json, &result); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unmarshal error"})
		return
	}
	c.JSON(http.StatusOK, result.Vessels)
}

// ListDarkVessels handles GET /api/v1/vessels/dark
func ListDarkVessels(c *gin.Context) {
	ctx := context.Background()
	q := `{
  vessels(func: eq(vessel.ais_status, "dark")) {
    uid
    vessel.imo
    vessel.name
    vessel.flag
    vessel.ais_status
    vessel.last_seen_time
    vessel.suspicion_score
    vessel.last_seen_port {
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
		Vessels []models.Vessel `json:"vessels"`
	}
	if err := json.Unmarshal(resp.Json, &result); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unmarshal error"})
		return
	}
	c.JSON(http.StatusOK, result.Vessels)
}

// VerifyVessel handles POST /api/v1/vessels/:imo/verify
// Triggers an on-demand cross-source identity check and returns the result immediately.
func VerifyVessel(c *gin.Context) {
	imo := c.Param("imo")
	ctx := context.Background()

	if Verifier == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "verification service not configured (set VESSEL_FINDER_API_KEY or enable MYSHIPTRACKING_ENABLED)",
		})
		return
	}

	result, err := Verifier.VerifyVessel(ctx, imo)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Also return current stored verification data from DB for comparison
	q := `query q($imo: string) {
  vessel(func: eq(vessel.imo, $imo)) {
    uid vessel.imo vessel.name vessel.flag vessel.mmsi
    vessel.verify_status vessel.verify_sources vessel.verify_last_at
    vessel.verify_name_ext vessel.verify_flag_ext
    vessel.verify_pos_gap_km vessel.verify_discrepancies
  }
}`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$imo": imo})
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"verification": result})
		return
	}
	var stored struct {
		Vessel []models.Vessel `json:"vessel"`
	}
	_ = json.Unmarshal(resp.Json, &stored)

	var storedVessel *models.Vessel
	if len(stored.Vessel) > 0 {
		storedVessel = &stored.Vessel[0]
	}

	slog.Info("on-demand verification complete",
		"imo", imo,
		"status", result.Status,
		"discrepancies", result.Discrepancies,
		"pos_gap_km", result.PosGapKm,
	)

	c.JSON(http.StatusOK, gin.H{
		"imo":          imo,
		"verification": result,
		"stored":       storedVessel,
	})
}

// resolveOrCreateCompany finds a company by name or creates it, returning its UID.
func resolveOrCreateCompany(ctx context.Context, name string) (string, error) {
	q := `query q($name: string) {
  company(func: eq(company.name, $name)) { uid }
}`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$name": name})
	if err != nil {
		return "", err
	}
	var res struct {
		Company []struct{ UID string `json:"uid"` } `json:"company"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return "", err
	}
	if len(res.Company) > 0 {
		return res.Company[0].UID, nil
	}

	// Create new company
	node := map[string]interface{}{
		"uid":          "_:company",
		"dgraph.type":  []string{"Company"},
		"company.name": name,
	}
	b, _ := json.Marshal(node)

	txn2 := db.NewTxn()
	defer txn2.Discard(ctx)

	mu := &api.Mutation{SetJson: b, CommitNow: true}
	mutResp, err := txn2.Mutate(ctx, mu)
	if err != nil {
		return "", fmt.Errorf("create company: %w", err)
	}
	for _, uid := range mutResp.Uids {
		return uid, nil
	}
	return "", fmt.Errorf("company create returned no UID")
}
