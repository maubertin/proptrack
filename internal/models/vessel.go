package models

import "time"

// Vessel represents a tracked maritime vessel node in Dgraph.
type Vessel struct {
	UID              string    `json:"uid,omitempty"`
	DgraphType       []string  `json:"dgraph.type,omitempty"`
	IMO              string    `json:"vessel.imo,omitempty"`
	MMSI             string    `json:"vessel.mmsi,omitempty"`
	Name             string    `json:"vessel.name,omitempty"`
	Flag             string    `json:"vessel.flag,omitempty"`
	Type             string    `json:"vessel.type,omitempty"`
	OwnerCompany     *Company  `json:"vessel.owner_company,omitempty"`
	Sanctioned       bool      `json:"vessel.sanctioned,omitempty"`
	SanctionPrograms string    `json:"vessel.sanction_programs,omitempty"`
	AISStatus        string    `json:"vessel.ais_status,omitempty"`
	LastSeenPort     *Port     `json:"vessel.last_seen_port,omitempty"`
	LastSeenTime     time.Time `json:"vessel.last_seen_time,omitempty"`
	LastLat          float64   `json:"vessel.last_lat,omitempty"`
	LastLon          float64   `json:"vessel.last_lon,omitempty"`
	LastDarkLat      float64   `json:"vessel.last_dark_lat,omitempty"`
	LastDarkLon      float64   `json:"vessel.last_dark_lon,omitempty"`
	DraftLoaded      float64   `json:"vessel.draft_loaded,omitempty"`
	DraftBallast     float64   `json:"vessel.draft_ballast,omitempty"`
	SuspicionScore   float64   `json:"vessel.suspicion_score,omitempty"`
	GRT              float64   `json:"vessel.grt,omitempty"`
	DWT              float64   `json:"vessel.dwt,omitempty"`
	// Cross-source verification fields
	VerifyStatus       string    `json:"vessel.verify_status,omitempty"`
	VerifySources      string    `json:"vessel.verify_sources,omitempty"`
	VerifyLastAt       time.Time `json:"vessel.verify_last_at,omitempty"`
	VerifyNameExt      string    `json:"vessel.verify_name_ext,omitempty"`
	VerifyFlagExt      string    `json:"vessel.verify_flag_ext,omitempty"`
	VerifyPosGapKm     float64   `json:"vessel.verify_pos_gap_km,omitempty"`
	VerifyDiscrepancies string   `json:"vessel.verify_discrepancies,omitempty"`
}

// CreateVesselRequest is the request body for POST /api/v1/vessels.
type CreateVesselRequest struct {
	IMO              string  `json:"imo" binding:"required"`
	MMSI             string  `json:"mmsi"`
	Name             string  `json:"name" binding:"required"`
	Flag             string  `json:"flag"`
	Type             string  `json:"type"`
	OwnerCompanyName string  `json:"owner_company_name"`
	Sanctioned       bool    `json:"sanctioned"`
	SanctionPrograms string  `json:"sanction_programs"`
	AISStatus        string  `json:"ais_status"`
	DraftLoaded      float64 `json:"draft_loaded"`
	DraftBallast     float64 `json:"draft_ballast"`
}

// UpdatePositionRequest is the body for PUT /api/v1/vessels/:imo/position.
type UpdatePositionRequest struct {
	AISStatus    string    `json:"ais_status"`
	LastPortCode string    `json:"last_port_unlocode"`
	LastSeenTime time.Time `json:"last_seen_time"`
	DraftLoaded  float64   `json:"draft_loaded"`
	DraftBallast float64   `json:"draft_ballast"`
}
