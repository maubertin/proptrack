# PropTrack — API Subscriptions

URLs directes pour souscrire ou créer un compte sur chaque source de données
utilisée par PropTrack.  Les sources sont classées par priorité d'activation.

---

## Priorité 0 — Sans inscription, sans clé

### MarineTraffic web scraper — découverte par tuiles cartographiques
| | |
|---|---|
| **Site** | https://www.marinetraffic.com |
| **Clé API** | **Aucune** — utilise l'endpoint JSON interne de la carte publique |
| **Inscription** | Non requise |
| **Variable `.env`** | `MT_SCRAPER_ENABLED=true` (activé par défaut) |
| **Cadence** | `MT_SCRAPE_INTERVAL=3h` (configurable) |

Scrape l'endpoint interne `getData/get_data_json_4` qui alimente la carte web de
MarineTraffic.  Couvre les mêmes 9 zones portuaires que les autres pollers avec des
en-têtes navigateur standards.  Pas de garantie de stabilité (endpoint non documenté) —
les erreurs de parsing sont loggées avec un extrait brut pour diagnostic.

Préfixe des shipments auto-créés : `MTS-xxxxxxxx`

---

## Priorité 1 — Gratuit ou trial suffisant pour valider le pipeline

### AISStream.io — flux AIS temps réel (WebSocket)
| | |
|---|---|
| **Inscription** | https://aisstream.io/authenticate |
| **Dashboard / clé API** | https://aisstream.io/dashboard |
| **Documentation** | https://aisstream.io/documentation |
| **Tarif** | Gratuit — clé API obtenue immédiatement à l'inscription |
| **Variable `.env`** | `AIS_STREAM_API_KEY` |

Couvre le flux de positions en temps réel sur les 5 corridors surveillés
(côte chinoise → détroit de Malacca → océan Indien → Hormuz → golfe Persique).

---

### MyShipTracking — découverte par zone portuaire
| | |
|---|---|
| **Inscription** | https://api.myshiptracking.com/register |
| **Dashboard / clé API** | https://api.myshiptracking.com/dashboard |
| **Documentation** | https://api.myshiptracking.com/docs |
| **Tarif** | Trial gratuit : **2 000 crédits / 10 jours** — suffisant pour ~3 cycles complets |
| **Plans payants** | https://api.myshiptracking.com/pricing |
| **Variable `.env`** | `MYSHIPTRACKING_API_KEY` |

Endpoint utilisé : `GET /vessel/zone` (réponse extended = 3 crédits/navire).

---

## Priorité 2 — Trial courte durée, enrichissement haute valeur

### Datalastic — découverte par pavillon + enrichissement destination
| | |
|---|---|
| **Inscription** | https://datalastic.com/register/ |
| **Dashboard / clé API** | https://datalastic.com/dashboard/ |
| **Documentation API** | https://datalastic.com/api-reference/ |
| **Trial** | 14 jours à partir de 9 EUR — https://datalastic.com/pricing/ |
| **Plans payants** | Starter 199 EUR/mois (20 000 req) · Mid ~300 EUR/mois · Enterprise 679 EUR/mois |
| **Variable `.env`** | `DATALASTIC_API_KEY` |

Endpoints utilisés :
- `GET /vessel_find?flag=ir,km,mn,...` (1 crédit/req) — discovery pavillon
- `GET /vessel_pro?imo=...` (1 crédit/req) — enrichissement destination + tonnage

---

### VesselFinder — vérification croisée d'identité
| | |
|---|---|
| **Inscription** | https://www.vesselfinder.com/register |
| **Demande de clé API** | https://api.vesselfinder.com/docs/implementation.html → *Request access* |
| **Documentation** | https://api.vesselfinder.com/docs/ |
| **Trial** | Gratuit sur demande (examiné manuellement) |
| **Plans payants** | Crédit-based : 330 EUR / 10 000 crédits → https://www.vesselfinder.com/vessel-positions-api/ |
| **Variable `.env`** | `VESSEL_FINDER_API_KEY` |

Endpoint utilisé : `GET /vessels?userkey=KEY&mmsi=MMSI`
(vérification d'identité croisée dans `POST /api/v1/vessels/:imo/verify`).

---

## Priorité 3 — Données de volume, coût plus élevé

### MarineTraffic — surveillance zone portuaire (alternative haute qualité)
| | |
|---|---|
| **Inscription** | https://www.marinetraffic.com/en/users/registration |
| **API Services** | https://www.marinetraffic.com/en/p/api-services |
| **Documentation** | https://servicedocs.marinetraffic.com/ |
| **Trial** | Gratuit, examiné manuellement — https://help.marinetraffic.com/hc/en-us/requests/new |
| **Plans payants** | Abonnement mensuel ou crédit à la demande — contacter le support pour tarif PS06 |
| **Variable `.env`** | `MARINETRAFFIC_API_KEY` |

Endpoint utilisé : `exportvessel v8` (service PS06 — bounding box, msgtype:extended).

---

## Sources sanctions (sans clé API)

Ces sources ne nécessitent aucun compte : PropTrack les télécharge directement.

| Source | URL | Mise à jour | Variable `.env` |
|---|---|---|---|
| **OFAC SDN** (US Treasury) | https://ofac.treasury.gov/specially-designated-nationals-list-data-formats-data-schemas | Quotidienne | `OFAC_ENABLED=true` |
| **UN SC Consolidated** | https://www.un.org/securitycouncil/content/un-sc-consolidated-list | Hebdomadaire | `UN_SANCTIONS_ENABLED=true` |
| **EU RELEX** | https://www.sanctionsmap.eu/#/main | Variable | `EU_SANCTIONS_ENABLED=true` + `EU_SANCTIONS_TOKEN` |

Pour l'EU RELEX, le token s'obtient ici :
https://webgate.ec.europa.eu/europeaid/fsd/fsf → *Demande d'accès XML*

---

## Récapitulatif

| Service | URL d'inscription | Trial gratuit | Variable `.env` |
|---|---|---|---|
| **MT web scraper** | *(aucune)* | ✅ Gratuit, sans compte | `MT_SCRAPER_ENABLED=true` |
| AISStream.io | https://aisstream.io/authenticate | ✅ Gratuit | `AIS_STREAM_API_KEY` |
| MyShipTracking | https://api.myshiptracking.com/register | ✅ 2 000 cr / 10 j | `MYSHIPTRACKING_API_KEY` |
| Datalastic | https://datalastic.com/register/ | ✅ 14 j / 9 EUR | `DATALASTIC_API_KEY` |
| VesselFinder | https://api.vesselfinder.com/docs/implementation.html | ✅ Sur demande | `VESSEL_FINDER_API_KEY` |
| MarineTraffic API | https://www.marinetraffic.com/en/p/api-services | ✅ Sur demande | `MARINETRAFFIC_API_KEY` |

---

## Vérification après configuration

Une fois les clés ajoutées dans `.env`, vérifier le bon fonctionnement :

```bash
# Test de connectivité et d'authentification (toutes les sources en parallèle)
curl -s http://localhost:9090/api/v1/sources/health | jq .

# Exemple de réponse attendue
{
  "checked_at": "2026-03-28T14:32:00Z",
  "sources": [
    {"name": "aisstream.io",    "enabled": true, "healthy": true,  "latency_ms": 187},
    {"name": "myshiptracking",  "enabled": true, "healthy": true,  "latency_ms": 312},
    {"name": "datalastic",      "enabled": false,"healthy": false},
    {"name": "marinetraffic",   "enabled": false,"healthy": false},
    {"name": "vesselfinder",    "enabled": false,"healthy": false}
  ]
}
```

Les mêmes résultats sont loggés au démarrage de l'application (level `INFO` si healthy,
`WARN` si unhealthy).
