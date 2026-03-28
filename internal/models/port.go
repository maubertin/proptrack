package models

// Port represents a maritime port node in Dgraph.
type Port struct {
	UID              string     `json:"uid,omitempty"`
	DgraphType       []string   `json:"dgraph.type,omitempty"`
	UNLOCODE         string     `json:"port.unlocode,omitempty"`
	Name             string     `json:"port.name,omitempty"`
	Country          string     `json:"port.country,omitempty"`
	RiskLevel          string     `json:"port.risk_level,omitempty"`
	KnownChemicalHub   bool       `json:"port.known_chemical_hub,omitempty"`
	ChemicalRiskScore  float64    `json:"port.chemical_risk_score,omitempty"`
	OutsideHormuz      bool       `json:"port.outside_hormuz,omitempty"`
	Lat                float64    `json:"port.coordinates_lat,omitempty"`
	Lon                float64    `json:"port.coordinates_lon,omitempty"`
	LinkedCompanies  []*Company `json:"port.linked_companies,omitempty"`
}

// UpsertPortRequest is the body for POST /api/v1/ports.
type UpsertPortRequest struct {
	UNLOCODE           string  `json:"unlocode" binding:"required"`
	Name               string  `json:"name" binding:"required"`
	Country            string  `json:"country"`
	RiskLevel          string  `json:"risk_level"`
	KnownChemicalHub   bool    `json:"known_chemical_hub"`
	ChemicalRiskScore  float64 `json:"chemical_risk_score"`
	OutsideHormuz      bool    `json:"outside_hormuz"`
	Lat                float64 `json:"lat"`
	Lon                float64 `json:"lon"`
}
