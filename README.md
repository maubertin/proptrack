# PropTrack — OSINT Propellant Precursor Tracking System

PropTrack est un système de renseignement maritime open-source (OSINT) focalisé sur
la détection des expéditions suspectes de précurseurs de propergol solide et de
matériaux à double usage à destination de l'Iran — principalement depuis les ports
d'exportation chinois à haut risque (Gaolan, Taïcang, Tianjin) via le corridor
Malacca–Hormuz.

Le moteur de détection croise en continu flux AIS temps réel, listes de sanctions
internationales (OFAC, ONU, UE) et historique de visites portuaires (Global Fishing
Watch) pour alimenter un score de suspicion additif sur plus de vingt critères :
port d'origine, type de cargo, opérateur, pavillon de complaisance, périodes de
silence AIS, escales de transbordement et écart de tirant d'eau. Les expéditions
dépassant le seuil critique (≥ 76) déclenchent une alerte automatique à destination
des analystes.

L'interface web embarquée propose une carte Leaflet avec marqueurs colorés par
niveau de risque, un panneau de détail par expédition (score visuel, décomposition
des critères, curseur de chronologie) et un tableau de bord statistique en temps réel.

---

PropTrack is an open-source maritime intelligence (OSINT) system focused on detecting
suspected shipments of solid-rocket propellant precursors and dual-use materials bound
for Iran — primarily from high-risk Chinese export ports (Gaolan, Taicang, Tianjin)
through the Malacca–Hormuz corridor.

The detection engine continuously cross-references real-time AIS feeds, international
sanctions lists (OFAC, UN, EU), and port-visit history (Global Fishing Watch) to
populate an additive suspicion score built on more than twenty criteria: origin port,
cargo type, vessel operator, flag of convenience, AIS dark periods, transshipment
relay calls, and draft differential. Shipments exceeding the critical threshold (≥ 76)
trigger an automatic analyst alert.

The embedded web interface provides a Leaflet map with risk-coded vessel markers,
a per-shipment detail panel (visual score gauge, criterion breakdown, timeline
scrubber), and a live summary dashboard.

---

## Stack

| Component | Technology |
|---|---|
| Backend | Go 1.22 + Gin |
| Graph DB | Dgraph v23 (Zero + Alpha) |
| API | REST on `:9090` |
| Container | Docker Compose |

---

## Quickstart

```bash
# 1. Copy env file
cp .env.example .env

# 2. Start everything (Dgraph Zero, Alpha, Go API)
docker compose up --build

# 3. Verify API is up
curl http://localhost:9090/health
```

On first boot the API applies the DQL schema and seeds known entities
(IRISL fleet, Chinese chemical ports, Iranian destination ports).

---

## Dgraph UI (Ratel)

Dgraph ships with a built-in GraphQL/DQL playground at:

```
http://localhost:8080
```

To query Dgraph directly via the HTTP API:

```bash
curl -s -X POST http://localhost:8080/query \
  -H "Content-Type: application/dql" \
  -d '{
    vessels(func: eq(vessel.sanctioned, true)) {
      vessel.imo vessel.name vessel.flag vessel.sanction_programs
    }
  }' | jq .
```

---

## Manually Reset the Schema

```bash
./scripts/init_schema.sh          # against localhost:8080
./scripts/init_schema.sh dgraph-alpha 8080   # against Docker container
```

---

## API Reference

### Health

```bash
GET /health
```

---

### Vessels

**Add / upsert a vessel**
```bash
curl -X POST http://localhost:9090/api/v1/vessels \
  -H "Content-Type: application/json" \
  -d '{
    "imo": "9283760",
    "name": "Barzin",
    "flag": "TG",
    "type": "bulk_carrier",
    "owner_company_name": "Islamic Republic of Iran Shipping Lines",
    "sanctioned": true,
    "sanction_programs": "IRAN NPWMD",
    "ais_status": "dark",
    "draft_loaded": 10.4,
    "draft_ballast": 5.9
  }'
```

**Get vessel with full graph context**
```bash
curl http://localhost:9090/api/v1/vessels/9283760 | jq .
```

**Update AIS position / draft**
```bash
curl -X PUT http://localhost:9090/api/v1/vessels/9283760/position \
  -H "Content-Type: application/json" \
  -d '{
    "ais_status": "dark",
    "last_port_unlocode": "CNZHL",
    "last_seen_time": "2025-03-10T08:00:00Z",
    "draft_loaded": 10.4,
    "draft_ballast": 5.9
  }'
```
> When `ais_status` transitions from `active` → `dark` while an active shipment
> exists for this vessel, `shipment.ais_dark_segments` is auto-incremented.

**List sanctioned vessels**
```bash
curl http://localhost:9090/api/v1/vessels/sanctioned | jq .
```

**List AIS-dark vessels**
```bash
curl http://localhost:9090/api/v1/vessels/dark | jq .
```

---

### Shipments

**Create a tracked shipment**
```bash
curl -X POST http://localhost:9090/api/v1/shipments \
  -H "Content-Type: application/json" \
  -d '{
    "vessel_imo": "9283760",
    "origin_unlocode": "CNZHL",
    "destination_unlocode": "IRBND",
    "departure_time": "2025-03-01T00:00:00Z",
    "estimated_arrival": "2025-04-15T00:00:00Z",
    "cargo_type": "sodium_perchlorate",
    "cargo_weight_tons": 4800,
    "source_references": "UN PoE S/2025/xxx annex B; MarineTraffic voyage history"
  }'
```

**Get shipment with live score**
```bash
curl http://localhost:9090/api/v1/shipments/SEED-S001 | jq .
```

**List all active shipments**
```bash
curl http://localhost:9090/api/v1/shipments/active | jq .
```

**List critical shipments (score ≥ 76)**
```bash
curl http://localhost:9090/api/v1/shipments/critical | jq .
```

**Update shipment status**
```bash
curl -X PUT http://localhost:9090/api/v1/shipments/SEED-S001/status \
  -H "Content-Type: application/json" \
  -d '{"status": "arrived", "actual_arrival": "2025-04-10T06:00:00Z"}'
```

**Add a waypoint (relay port)**
```bash
curl -X POST http://localhost:9090/api/v1/shipments/SEED-S002/waypoint \
  -H "Content-Type: application/json" \
  -d '{"port_unlocode": "IDBTM"}'
```

---

### Scoring

**Force-recompute a shipment score**
```bash
curl -X POST http://localhost:9090/api/v1/score/shipment/SEED-S002 | jq .
```
Returns score, triggered flags, human-readable summary, and any generated alert.

**Leaderboard — top 20 highest-suspicion active shipments**
```bash
curl http://localhost:9090/api/v1/score/leaderboard | jq .
```

---

### Graph Queries

**Full vessel neighbourhood**
```bash
curl http://localhost:9090/api/v1/graph/vessel/9354746/connections | jq .
```

**Company fleet**
```bash
curl "http://localhost:9090/api/v1/graph/company/IRISL/fleet" | jq .
```

**Raw DQL passthrough (admin)**
```bash
curl -X POST http://localhost:9090/api/v1/graph/query \
  -H "Content-Type: application/json" \
  -H "X-Admin-Token: changeme" \
  -d '{
    "query": "{ ports(func: eq(port.known_chemical_hub, true)) { port.unlocode port.name port.country } }"
  }' | jq .
```

---

### Ports

**Add / upsert a port**
```bash
curl -X POST http://localhost:9090/api/v1/ports \
  -H "Content-Type: application/json" \
  -d '{
    "unlocode": "CNPVG",
    "name": "Shanghai Yangshan",
    "country": "CN",
    "risk_level": "medium",
    "known_chemical_hub": false,
    "lat": 30.62,
    "lon": 122.05
  }'
```

**Ports by risk level**
```bash
curl http://localhost:9090/api/v1/ports/risk/critical | jq .
```

**Recent shipments through a port**
```bash
curl http://localhost:9090/api/v1/ports/CNZHL/shipments | jq .
```

---

## Scoring Criteria

| Criterion | Points |
|---|---|
| Origin is a known chemical hub (Gaolan, Taicang) | +30 |
| Vessel operated by IRISL or IRISL subsidiary | +25 |
| Vessel under OFAC/EU/UK sanctions | +20 |
| High-risk cargo type (perchlorate, oxidizer, etc.) | +20 |
| Destination is Chabahar or Bandar Abbas | +15 |
| AIS dark segments during voyage | +10 |
| Draft difference > 3 m (heavy chemical load) | +10 |
| Flag of convenience (Tonga, Palau, Sierra Leone, Cameroon…) | +10 |
| Waypoint in relay country (Malaysia, Indonesia, UAE) | +5 |
| Vessel has prior confirmed delivery to Iran | +5 |

**Thresholds**

| Range | Level | Action |
|---|---|---|
| 0–30 | Low | Routine monitoring |
| 31–55 | Medium | Assign analyst review |
| 56–75 | High | Alert — escalate to senior analyst |
| 76–100 | Critical | Immediate report; consider interdiction referral |

---

## Seeded Entities

The system pre-populates the graph on startup with publicly known entities:

### Ports
- `CNZHL` — Gaolan Port (Zhuhai, CN) — chemical hub, risk: critical
- `CNTAI` — Taicang Port (CN) — chemical hub, risk: high
- `IRBND` — Bandar Abbas / Shahid Rajaee (IR) — risk: critical
- `IRCHB` — Chabahar (IR) — risk: critical
- `MYPEN` / `IDBTM` — relay hubs (Malaysia, Indonesia)

### Companies
- Islamic Republic of Iran Shipping Lines (IRISL) — OFAC SDN-22873
- Hafiz Darya Shipping Lines (HDSL) — OFAC SDN-22874

### Vessels
| IMO | Name | Notes |
|---|---|---|
| 9283760 | Barzin | IRISL bulk carrier, known NaClO₄ hauler |
| 9354746 | Golbon | HDSL, departed Taicang Feb 2025 |
| 9167289 | Shabdiz | IRISL general cargo, Gaolan routes |

> IMO numbers sourced from: OFAC SDN public search, UN Panel of Experts
> Iran reports, MarineTraffic public voyage history, Middlebury Institute
> monitoring data (2024–2025).

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `DGRAPH_ALPHA_HOST` | `dgraph-alpha` | Dgraph Alpha hostname |
| `DGRAPH_ALPHA_PORT` | `9080` | Dgraph gRPC port |
| `API_PORT` | `9090` | API listen port |
| `API_ADMIN_TOKEN` | `changeme` | Token for `/api/v1/graph/query` |
| `LOG_LEVEL` | `info` | `info` or `debug` |

---

## Data Persistence

Dgraph data is persisted in bind-mount volumes under `./data/`:

```
./data/dgraph/zero/   — Dgraph Zero node data
./data/dgraph/alpha/  — Dgraph Alpha node data
```

To wipe and start fresh:
```bash
docker compose down
rm -rf ./data/
docker compose up --build
```
