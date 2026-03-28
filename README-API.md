# PropTrack — API Keys & External Services

This document describes every external service integrated into PropTrack, what each
key unlocks, how to obtain it, and the exact environment variable to add to your `.env`
(or to export before running the binary).

---

## Quick-start `.env` template

Pour démarrer sans coût : active uniquement AIS_STREAM_API_KEY (gratuit) + MYSHIPTRACKING_API_KEY (trial gratuit 10 jours). Le GET /health indique quelles sources sont actives.


```env
# ── MarineTraffic web scraper (no key — enabled by default) ──────────────────
MT_SCRAPER_ENABLED=true
MT_SCRAPE_INTERVAL=3h

# ── AIS real-time stream ──────────────────────────────────────────────────────
AIS_STREAM_API_KEY=

# ── Vessel cross-source verification ─────────────────────────────────────────
VESSEL_FINDER_API_KEY=

# ── Port surveillance / bounding-box discovery ───────────────────────────────
MARINETRAFFIC_API_KEY=
MYSHIPTRACKING_API_KEY=

# ── Flag-based discovery + vessel enrichment ─────────────────────────────────
DATALASTIC_API_KEY=

# ── Sanctions lists ───────────────────────────────────────────────────────────
OFAC_ENABLED=true
UN_SANCTIONS_ENABLED=true
EU_SANCTIONS_ENABLED=false
EU_SANCTIONS_TOKEN=

# ── Tuning (all optional — defaults shown) ───────────────────────────────────
PORT_WATCH_INTERVAL=1h
MST_POLL_INTERVAL=2h
DATALASTIC_ENRICH_INTERVAL=4h
DATALASTIC_ENRICH_BATCH=25
ROUTE_PRUNE_DAYS=30
AIS_RECONNECT_DELAY=30s
LOG_LEVEL=info
API_PORT=9090
```

---

## 1. AISStream.io — real-time AIS WebSocket

| | |
|---|---|
| **Variable** | `AIS_STREAM_API_KEY` |
| **Service** | Live AIS position stream over WebSocket |
| **Used for** | Continuous position tracking of known vessels, dark-segment detection |
| **Free tier** | Yes — free API key, limited bounding boxes |
| **Sign-up** | https://aisstream.io → *Get API Key* |
| **Format** | 40-character hex string |

PropTrack subscribes to 5 bounding boxes covering the China–Strait of Malacca–Indian
Ocean–Strait of Hormuz–Persian Gulf corridor.  The poller starts automatically when
the key is set.

---

## 2. VesselFinder — vessel verification

| | |
|---|---|
| **Variable** | `VESSEL_FINDER_API_KEY` |
| **Service** | AIS position + master data per vessel |
| **Used for** | Cross-source identity verification (`POST /api/v1/vessels/:imo/verify`) |
| **Free tier** | Free trial on request |
| **Sign-up** | https://api.vesselfinder.com → *API Access* → request trial |
| **Auth format** | `userkey=XXXXXXXXXXXXXXXX` (16-char alphanumeric) |
| **Pricing** | Credit-based: 330 EUR / 10 000 credits (1 cr = terrestrial AIS pos) |

The verifier runs every 6 h (configurable via `VESSEL_VERIFY_INTERVAL`).  It is
enabled when the key is set; disabled gracefully otherwise.

**Relevant endpoints used:**
```
GET https://api.vesselfinder.com/vessels?userkey={KEY}&mmsi={MMSI}
GET https://api.vesselfinder.com/masterdata?userkey={KEY}&mmsi={MMSI}
```

---

## 3. MarineTraffic — port-area bounding-box discovery

| | |
|---|---|
| **Variable** | `MARINETRAFFIC_API_KEY` |
| **Service** | Vessel positions in a dynamic bounding box (API service PS06) |
| **Used for** | Port surveillance: discovering vessels in Chinese/Iranian port zones |
| **Free tier** | Trial on request (manually reviewed) |
| **Sign-up** | https://www.marinetraffic.com/en/p/api-services → *Get API Key* |
| **Auth format** | Alphanumeric key embedded in URL path |
| **Rate limit** | 100 req/min default |

**Endpoint used:**
```
GET https://services.marinetraffic.com/api/exportvessel/v:8/{KEY}/
    minlat:{}/maxlat:{}/minlon:{}/maxlon:{}/msgtype:extended/protocol:jsono
```

**Optional tuning:**
```env
PORT_WATCH_INTERVAL=1h     # how often to poll all 9 zones (default: 1 h)
ROUTE_PRUNE_DAYS=30        # mark monitoring routes as pruned after N days with no Iran link
```

---

## 4. MyShipTracking — zone-based discovery (recommended first source)

| | |
|---|---|
| **Variable** | `MYSHIPTRACKING_API_KEY` |
| **Service** | AIS vessel positions in a circular zone (`/vessel/zone`) |
| **Used for** | Bounding-box discovery — same 9 port zones as MarineTraffic PortWatcher |
| **Free tier** | **Yes — 2 000 coins (credits), 10-day trial** |
| **Sign-up** | https://api.myshiptracking.com → *Register* → *API Key* |
| **Auth format** | Bearer token: `Authorization: Bearer <key>` |
| **Rate limit** | 90 req/min (trial) / 2 000 req/min (paid) |
| **Pricing** | Credit-based; extended response = 3 credits per vessel |

MyShipTracking is the easiest entry point: the free trial gives enough credits to run
several full discovery cycles before committing to a plan.

**Endpoint used:**
```
GET https://api.myshiptracking.com/api/v2/vessel/zone
    ?lat={center_lat}&lon={center_lon}&radius={km}&response=extended
Authorization: Bearer <MYSHIPTRACKING_API_KEY>
```

**Optional tuning:**
```env
MST_POLL_INTERVAL=2h    # polling cadence per zone group (default: 2 h)
```

**Extended response fields used:** `imo`, `mmsi`, `vessel_name`, `flag`,
`vessel_type`, `latitude`, `longitude`, `speed`, `draught`, `gross_tonnage`,
`deadweight`, `destination`, `next_port`, `eta`.

---

## 5. Datalastic — flag-based discovery + vessel enrichment

| | |
|---|---|
| **Variable** | `DATALASTIC_API_KEY` |
| **Service** | Vessel search by flag + per-vessel enrichment with standardised destination |
| **Used for** | (a) Discover vessels flying Iran shadow-fleet flags inside Chinese zones; (b) Enrich active/monitoring vessels with current destination, ETA, and tonnage |
| **Free tier** | 14-day trial — starting at 9 EUR |
| **Sign-up** | https://datalastic.com → *Try for Free* |
| **Auth format** | Query parameter: `?api-key=<key>` |
| **Rate limit** | 600 req/min |
| **Pricing** | Credit-based: Starter 199 EUR/month / 20 000 requests |

Datalastic adds a **semantic layer** that complements zone-based pollers: instead of
asking "what ships are physically in port X?", it asks "what ships are *flying these
flags*?" — catching vessels that transit the zone without stopping.

**Endpoints used:**

*Discovery — vessels by flag:*
```
GET https://api.datalastic.com/api/v0/vessel_find
    ?api-key={KEY}&flag={iso2_lower}&page=0&size=100
```
Flags queried: `ir`, `km`, `mn`, `tz`, `tv`, `pw`, `cm`, `bo`, `gw`, `ga`

*Enrichment — per-vessel detail with standardised destination:*
```
GET https://api.datalastic.com/api/v0/vessel_pro
    ?api-key={KEY}&imo={IMO}
```
Fields used: `lat`, `lon`, `draught`, `gross_tonnage`, `deadweight`,
`destination`, `destination_port_unlo`, `eta`.

**Optional tuning:**
```env
DATALASTIC_ENRICH_INTERVAL=4h   # enrichment cycle cadence (default: 4 h)
DATALASTIC_ENRICH_BATCH=25      # max vessels enriched via /vessel_pro per cycle
                                 # (controls credit spend; raise once on paid plan)
```

---

## 6. Sanctions lists (no API key required)

| Service | Variable | Default URL | Notes |
|---|---|---|---|
| **OFAC SDN** | `OFAC_SDN_URL` | `https://www.treasury.gov/ofac/downloads/sdn.xml` | Free, updated daily |
| **UN SC Consolidated** | `UN_SANCTIONS_URL` | `https://scsanctions.un.org/resources/xml/en/consolidated.xml` | Free, updated weekly |
| **EU RELEX** | `EU_SANCTIONS_URL` | `https://webgate.ec.europa.eu/europeaid/fsd/fsf/...` | Requires `EU_SANCTIONS_TOKEN` |

```env
OFAC_ENABLED=true
UN_SANCTIONS_ENABLED=true
EU_SANCTIONS_ENABLED=false    # set true + EU_SANCTIONS_TOKEN to activate
EU_SANCTIONS_TOKEN=           # obtain from webgate.ec.europa.eu
OFAC_UPDATE_INTERVAL=24h
UN_UPDATE_INTERVAL=24h
EU_UPDATE_INTERVAL=24h
```

---

## Discovery source comparison

| Source | Method | Strength | Credit cost |
|---|---|---|---|
| **AISStream** | WebSocket stream | Real-time; dark-segment detection | Free tier |
| **MarineTraffic** | Bounding box (port zone) | High-quality extended data | Paid |
| **MyShipTracking** | Bounding box (zone radius) | Free trial; `next_port` field | 3 cr/vessel (extended) |
| **Datalastic /vessel_find** | Flag-country filter | Catches vessels not physically in zone | 1 cr/req |
| **Datalastic /vessel_pro** | Per-IMO enrichment | Standardised UNLOCODE destination | 1 cr/req |

For a zero-cost start, activate **AISStream** (free key) + **MyShipTracking** (free
trial).  Add **Datalastic** once you have confirmed the pipeline works end-to-end.

---

## Health endpoint

`GET /health` reports which sources are currently active:

```json
{
  "ais_enabled": true,
  "port_watch_enabled": false,
  "mst_enabled": true,
  "datalastic_enabled": false,
  "ofac_enabled": true,
  "un_enabled": true,
  "eu_enabled": false
}
```
