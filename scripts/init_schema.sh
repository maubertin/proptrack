#!/usr/bin/env bash
# init_schema.sh — manually (re)apply the Dgraph DQL schema
# Usage: ./scripts/init_schema.sh [alpha_host] [alpha_http_port]
# Defaults: localhost 8080

set -euo pipefail

ALPHA_HOST="${1:-localhost}"
ALPHA_HTTP_PORT="${2:-8080}"
BASE_URL="http://${ALPHA_HOST}:${ALPHA_HTTP_PORT}"

echo ">> Waiting for Dgraph Alpha at ${BASE_URL} ..."
for i in $(seq 1 30); do
  if curl -sf "${BASE_URL}/health" > /dev/null 2>&1; then
    echo ">> Dgraph Alpha is up."
    break
  fi
  echo "   attempt ${i}/30 — retrying in 3s..."
  sleep 3
done

echo ">> Applying schema..."
curl -s -X POST "${BASE_URL}/alter" \
  -H "Content-Type: application/dql" \
  --data-binary @- <<'SCHEMA'
vessel.imo:              string  @index(exact)          .
vessel.name:             string  @index(term, trigram)  .
vessel.flag:             string  @index(exact)          .
vessel.type:             string                         .
vessel.owner_company:    uid     @reverse               .
vessel.sanctioned:       bool    @index(bool)           .
vessel.sanction_programs: string @index(term)           .
vessel.ais_status:       string  @index(exact)          .
vessel.last_seen_port:   uid     @reverse               .
vessel.last_seen_time:   datetime                       .
vessel.draft_loaded:     float                          .
vessel.draft_ballast:    float                          .
vessel.suspicion_score:  float   @index(float)          .

port.unlocode:            string  @index(exact)          .
port.name:                string  @index(term)           .
port.country:             string  @index(exact)          .
port.risk_level:          string  @index(exact)          .
port.known_chemical_hub:  bool    @index(bool)           .
port.coordinates_lat:     float                          .
port.coordinates_lon:     float                          .
port.linked_companies:    [uid]   @reverse               .

company.name:             string  @index(term, trigram)  .
company.country:          string  @index(exact)          .
company.sanctioned:       bool    @index(bool)           .
company.ofac_id:          string  @index(exact)          .
company.known_aliases:    string  @index(term)           .
company.operates_vessels: [uid]   @reverse               .

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
shipment.status:            string   @index(exact)          .
shipment.source_references: string                          .

sanction.list:         string   @index(exact)  .
sanction.entity_type:  string                  .
sanction.entity_ref:   uid      @reverse       .
sanction.date_listed:  datetime                .
sanction.program:      string   @index(exact)  .
sanction.notes:        string                  .

type Vessel {
  vessel.imo vessel.name vessel.flag vessel.type vessel.owner_company
  vessel.sanctioned vessel.sanction_programs vessel.ais_status
  vessel.last_seen_port vessel.last_seen_time vessel.draft_loaded
  vessel.draft_ballast vessel.suspicion_score
}
type Port {
  port.unlocode port.name port.country port.risk_level
  port.known_chemical_hub port.coordinates_lat port.coordinates_lon
  port.linked_companies
}
type Company {
  company.name company.country company.sanctioned company.ofac_id
  company.known_aliases company.operates_vessels
}
type Shipment {
  shipment.id shipment.vessel shipment.origin_port shipment.destination_port
  shipment.departure_time shipment.estimated_arrival shipment.actual_arrival
  shipment.cargo_type shipment.cargo_weight_tons shipment.ais_dark_segments
  shipment.waypoints shipment.suspicion_score shipment.suspicion_flags
  shipment.status shipment.source_references
}
type SanctionEntry {
  sanction.list sanction.entity_type sanction.entity_ref
  sanction.date_listed sanction.program sanction.notes
}
SCHEMA

echo ""
echo ">> Schema applied. Verify at ${BASE_URL}/alter or via Ratel UI."
