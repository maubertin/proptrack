package models

import "time"

// Shipment represents a tracked cargo movement in Dgraph.
type Shipment struct {
	UID              string    `json:"uid,omitempty"`
	DgraphType       []string  `json:"dgraph.type,omitempty"`
	ID               string    `json:"shipment.id,omitempty"`
	Vessel           *Vessel   `json:"shipment.vessel,omitempty"`
	OriginPort       *Port     `json:"shipment.origin_port,omitempty"`
	DestinationPort  *Port     `json:"shipment.destination_port,omitempty"`
	DepartureTime    time.Time `json:"shipment.departure_time,omitempty"`
	EstimatedArrival time.Time `json:"shipment.estimated_arrival,omitempty"`
	ActualArrival    time.Time `json:"shipment.actual_arrival,omitempty"`
	CargoType        string    `json:"shipment.cargo_type,omitempty"`
	CargoWeightTons  float64   `json:"shipment.cargo_weight_tons,omitempty"`
	AISDarkSegments  int       `json:"shipment.ais_dark_segments,omitempty"`
	Waypoints        []*Port   `json:"shipment.waypoints,omitempty"`
	SuspicionScore   float64   `json:"shipment.suspicion_score,omitempty"`
	SuspicionFlags   string    `json:"shipment.suspicion_flags,omitempty"`
	Status           string    `json:"shipment.status,omitempty"`
	SourceReferences string    `json:"shipment.source_references,omitempty"`
	TransportMode    string    `json:"shipment.transport_mode,omitempty"`
	HSCode           string    `json:"shipment.hs_code,omitempty"`
}

// CreateShipmentRequest is the body for POST /api/v1/shipments.
type CreateShipmentRequest struct {
	VesselIMO        string    `json:"vessel_imo" binding:"required"`
	OriginUNLOCODE   string    `json:"origin_unlocode" binding:"required"`
	DestUNLOCODE     string    `json:"destination_unlocode" binding:"required"`
	DepartureTime    time.Time `json:"departure_time" binding:"required"`
	EstimatedArrival time.Time `json:"estimated_arrival"`
	CargoType        string    `json:"cargo_type"`
	CargoWeightTons  float64   `json:"cargo_weight_tons"`
	SourceReferences string    `json:"source_references"`
	TransportMode    string    `json:"transport_mode"`
	HSCode           string    `json:"hs_code"`
}

// UpdateStatusRequest is the body for PUT /api/v1/shipments/:id/status.
type UpdateStatusRequest struct {
	Status        string    `json:"status" binding:"required"`
	ActualArrival time.Time `json:"actual_arrival"`
}

// AddWaypointRequest is the body for POST /api/v1/shipments/:id/waypoint.
type AddWaypointRequest struct {
	PortUNLOCODE string `json:"port_unlocode" binding:"required"`
}

// SanctionEntry represents an OFAC/SDN or other sanctions list entry.
type SanctionEntry struct {
	UID        string    `json:"uid,omitempty"`
	DgraphType []string  `json:"dgraph.type,omitempty"`
	List       string    `json:"sanction.list,omitempty"`
	EntityType string    `json:"sanction.entity_type,omitempty"`
	EntityRef  string    `json:"sanction.entity_ref,omitempty"`
	DateListed time.Time `json:"sanction.date_listed,omitempty"`
	Program    string    `json:"sanction.program,omitempty"`
	Notes      string    `json:"sanction.notes,omitempty"`
}
