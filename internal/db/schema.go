package db

// DQLSchema is the full Dgraph schema for the PropTrack system.
// Applied once on startup via SetupSchema().
const DQLSchema = `
# ── Vessel predicates ────────────────────────────────────────────────────────
vessel.imo:              string  @index(exact)          .
vessel.mmsi:             string  @index(exact)          .
vessel.name:             string  @index(term, trigram)  .
vessel.flag:             string  @index(exact)          .
vessel.type:             string  @index(exact)          .
vessel.owner_company:    uid     @reverse               .
vessel.sanctioned:       bool    @index(bool)           .
vessel.sanction_programs: string @index(term)           .
vessel.ais_status:       string  @index(exact)          .
vessel.last_seen_port:   uid     @reverse               .
vessel.last_seen_time:   datetime                       .
vessel.last_lat:         float                          .
vessel.last_lon:         float                          .
vessel.last_dark_lat:    float                          .
vessel.last_dark_lon:    float                          .
vessel.draft_loaded:     float                          .
vessel.draft_ballast:    float                          .
vessel.suspicion_score:  float   @index(float)          .
vessel.grt: float .
vessel.dwt: float .
vessel.verify_status:    string  @index(exact)          .
vessel.verify_sources:   string                         .
vessel.verify_last_at:   datetime                       .
vessel.verify_name_ext:  string                         .
vessel.verify_flag_ext:  string                         .
vessel.verify_pos_gap_km: float                         .
vessel.verify_discrepancies: string @index(term)        .

# ── Port predicates ───────────────────────────────────────────────────────────
port.unlocode:             string  @index(exact)          .
port.name:                 string  @index(term)           .
port.country:              string  @index(exact)          .
port.risk_level:           string  @index(exact)          .
port.known_chemical_hub:   bool    @index(bool)           .
port.chemical_risk_score:  float   @index(float)          .
port.outside_hormuz:       bool    @index(bool)           .
port.coordinates_lat:      float                          .
port.coordinates_lon:      float                          .
port.linked_companies:     [uid]   @reverse               .

# ── Company predicates ────────────────────────────────────────────────────────
company.name:             string  @index(term, trigram)  .
company.country:          string  @index(exact)          .
company.sanctioned:       bool    @index(bool)           .
company.ofac_id:          string  @index(exact)          .
company.known_aliases:    string  @index(term)           .
company.operates_vessels: [uid]   @reverse               .

# ── Shipment predicates ───────────────────────────────────────────────────────
shipment.id:                string   @index(exact)          .
shipment.vessel:            uid      @reverse               .
shipment.origin_port:       uid      @reverse               .
shipment.destination_port:  uid      @reverse               .
shipment.departure_time:    datetime @index(hour)           .
shipment.estimated_arrival: datetime                        .
shipment.actual_arrival:    datetime                        .
shipment.cargo_type:        string   @index(exact)          .
shipment.cargo_weight_tons: float                           .
shipment.ais_dark_segments: int                             .
shipment.waypoints:         [uid]    @reverse               .
shipment.suspicion_score:   float    @index(float)          .
shipment.suspicion_flags:   string   @index(term)           .
shipment.status:            string   @index(exact, term)     .
shipment.source_references: string                          .
shipment.transport_mode:    string   @index(exact)          .
shipment.hs_code:           string   @index(term)           .

# ── SanctionEntry predicates ──────────────────────────────────────────────────
sanction.list:         string   @index(exact)  .
sanction.entity_type:  string                  .
sanction.entity_ref:   uid      @reverse       .
sanction.date_listed:  datetime                .
sanction.program:      string   @index(exact)  .
sanction.notes:        string                  .

# ── dgraph.type declarations ──────────────────────────────────────────────────
type Vessel {
  vessel.imo
  vessel.mmsi
  vessel.name
  vessel.flag
  vessel.type
  vessel.owner_company
  vessel.sanctioned
  vessel.sanction_programs
  vessel.ais_status
  vessel.last_seen_port
  vessel.last_seen_time
  vessel.last_lat
  vessel.last_lon
  vessel.last_dark_lat
  vessel.last_dark_lon
  vessel.draft_loaded
  vessel.draft_ballast
  vessel.suspicion_score
  vessel.grt
  vessel.dwt
  vessel.verify_status
  vessel.verify_sources
  vessel.verify_last_at
  vessel.verify_name_ext
  vessel.verify_flag_ext
  vessel.verify_pos_gap_km
  vessel.verify_discrepancies
}

type Port {
  port.unlocode
  port.name
  port.country
  port.risk_level
  port.known_chemical_hub
  port.chemical_risk_score
  port.outside_hormuz
  port.coordinates_lat
  port.coordinates_lon
  port.linked_companies
}

type Company {
  company.name
  company.country
  company.sanctioned
  company.ofac_id
  company.known_aliases
  company.operates_vessels
}

type Shipment {
  shipment.id
  shipment.vessel
  shipment.origin_port
  shipment.destination_port
  shipment.departure_time
  shipment.estimated_arrival
  shipment.actual_arrival
  shipment.cargo_type
  shipment.cargo_weight_tons
  shipment.ais_dark_segments
  shipment.waypoints
  shipment.suspicion_score
  shipment.suspicion_flags
  shipment.status
  shipment.source_references
  shipment.transport_mode
  shipment.hs_code
}

type SanctionEntry {
  sanction.list
  sanction.entity_type
  sanction.entity_ref
  sanction.date_listed
  sanction.program
  sanction.notes
}
`
