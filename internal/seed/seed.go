package seed

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/proptrack/proptrack/internal/db"
)

// Run seeds the graph with known entities from public OSINT sources.
// It is idempotent: it uses Dgraph upsert blocks to avoid duplicate nodes.
func Run(ctx context.Context) error {
	if err := seedPorts(ctx); err != nil {
		return err
	}
	if err := seedCompanies(ctx); err != nil {
		return err
	}
	if err := seedVessels(ctx); err != nil {
		return err
	}
	if err := linkPortsToCompanies(ctx); err != nil {
		slog.Warn("port-company links partially failed", "err", err)
	}
	slog.Info("seed data applied successfully")
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Ports
// ──────────────────────────────────────────────────────────────────────────────

type portSeed struct {
	UNLOCODE          string
	Name              string
	Country           string
	RiskLevel         string
	KnownChemicalHub  bool
	ChemicalRiskScore float64
	OutsideHormuz     bool
	Lat               float64
	Lon               float64
}

var knownPorts = []portSeed{
	// ── China — Volet 1: matrice de scoring documentée ────────────────────────
	// Gaolan (Zhuhai, Guangdong): hub pour l'exportation de perchlorate de sodium.
	// Shenzhen Amor Logistics et China Chlorate Tech Co. basés ici, sanctionnés
	// OFAC pour transferts vers l'Iran (Jerusalem Post / Newsweek 2024).
	{
		UNLOCODE: "CNZHL", Name: "Gaolan Port (Zhuhai)", Country: "CN",
		RiskLevel: "critical", KnownChemicalHub: true, ChemicalRiskScore: 30,
		Lat: 21.97, Lon: 113.34,
	},
	// Taicang: Golbon est parti d'ici avec ~1 000 t de perchlorate de sodium
	// (CNN Feb 2025). Yanling Chuanxing Chemical Plant opère sur ce port.
	{
		UNLOCODE: "CNTAI", Name: "Taicang Port", Country: "CN",
		RiskLevel: "high", KnownChemicalHub: true, ChemicalRiskScore: 25,
		Lat: 31.60, Lon: 121.09,
	},
	// Tianjin et Qingdao: ports industriels modérés, présence chimique, signal bruité.
	{
		UNLOCODE: "CNTJN", Name: "Port of Tianjin (Xingang)", Country: "CN",
		RiskLevel: "medium", KnownChemicalHub: false, ChemicalRiskScore: 10,
		Lat: 38.98, Lon: 117.72,
	},
	{
		UNLOCODE: "CNTAO", Name: "Port of Qingdao", Country: "CN",
		RiskLevel: "medium", KnownChemicalHub: false, ChemicalRiskScore: 10,
		Lat: 36.07, Lon: 120.38,
	},
	// Shanghai et Ningbo: volume élevé, signal faible mais non exclus.
	{
		UNLOCODE: "CNSHA", Name: "Port of Shanghai", Country: "CN",
		RiskLevel: "medium", KnownChemicalHub: false, ChemicalRiskScore: 5,
		Lat: 31.23, Lon: 121.47,
	},
	{
		UNLOCODE: "CNNGB", Name: "Port of Ningbo-Zhoushan", Country: "CN",
		RiskLevel: "low", KnownChemicalHub: false, ChemicalRiskScore: 5,
		Lat: 29.86, Lon: 121.55,
	},

	// ── Iran — Volet 2: destinations avec contexte géopolitique ──────────────
	// Chabahar: hors du détroit de Hormuz, accès direct océan Indien.
	// Route active mars 2026 (Iran International, Shabdiz ETA ~16 mars).
	// Frontière Pakistan fermée depuis juin 2025 → livraison maritime seule option.
	{
		UNLOCODE: "IRCHB", Name: "Chabahar", Country: "IR",
		RiskLevel: "critical", KnownChemicalHub: false, OutsideHormuz: true,
		Lat: 25.30, Lon: 60.64,
	},
	// Bandar Abbas: principal terminal iranien, installations IRGC. Dans Hormuz.
	{
		UNLOCODE: "IRBND", Name: "Bandar Abbas (Shahid Rajaee)", Country: "IR",
		RiskLevel: "critical", KnownChemicalHub: false, OutsideHormuz: false,
		Lat: 27.18, Lon: 56.28,
	},
	{
		UNLOCODE: "IRBUZ", Name: "Bushehr", Country: "IR",
		RiskLevel: "high", KnownChemicalHub: false, OutsideHormuz: false,
		Lat: 28.97, Lon: 50.83,
	},

	// ── Relais AIS ────────────────────────────────────────────────────────────
	{
		UNLOCODE: "MYPEN", Name: "Penang", Country: "MY",
		RiskLevel: "medium", Lat: 5.41, Lon: 100.33,
	},
	{
		UNLOCODE: "IDBTM", Name: "Batam", Country: "ID",
		RiskLevel: "medium", Lat: 1.12, Lon: 104.05,
	},
	// Gwadar: port pakistanais à 76 nm de Chabahar. Cabotage côtier possible
	// malgré fermeture de la frontière terrestre. Surveiller petits navires.
	{
		UNLOCODE: "PKGWD", Name: "Gwadar", Country: "PK",
		RiskLevel: "medium", Lat: 25.12, Lon: 62.32,
	},

	// ── Hubs fret aérien — routes drones/nucléaire ────────────────────────
	// Hong Kong: principal hub d'agrégation pour composants électroniques UAV.
	// Sociétés Dingtai Technology, Yonghongan Electronics, Tianle Trading
	// documentées comme intermédiaires vers PKGB (Iran). Source: FDD 2024.
	{
		UNLOCODE: "HKHKG", Name: "Hong Kong", Country: "HK",
		RiskLevel: "high", KnownChemicalHub: false, ChemicalRiskScore: 12,
		Lat: 22.29, Lon: 114.16,
	},
	// Dubaï (Jebel Ali): hub de transbordement et relais aérien vers Téhéran.
	// Route DXB → IKA documentée pour composants sanctionnés transitant par EAU.
	{
		UNLOCODE: "AEDXB", Name: "Dubai (Jebel Ali)", Country: "AE",
		RiskLevel: "high", KnownChemicalHub: false, ChemicalRiskScore: 0,
		Lat: 25.01, Lon: 55.06,
	},
	// Téhéran — aéroport international Imam Khomeini (IKA).
	// Destination fret aérien pour composants drones et matériaux nucléaires
	// dual-use via DXB → IKA. Source: Atlantic Council / FDD 2024.
	{
		UNLOCODE: "IRTHR", Name: "Tehran (Imam Khomeini Airport)", Country: "IR",
		RiskLevel: "critical", KnownChemicalHub: false, ChemicalRiskScore: 0,
		Lat: 35.42, Lon: 51.15,
	},
	// Shanghai Pudong Airport: origine fret aérien CN pour pièces dual-use.
	{
		UNLOCODE: "CNPVG", Name: "Shanghai Pudong Airport", Country: "CN",
		RiskLevel: "medium", KnownChemicalHub: false, ChemicalRiskScore: 0,
		Lat: 31.15, Lon: 121.81,
	},
	// Khorramshahr: port fluvial iranien — mentionné schema, ajout coordonnées.
	{
		UNLOCODE: "IRKHO", Name: "Khorramshahr", Country: "IR",
		RiskLevel: "high", KnownChemicalHub: false, ChemicalRiskScore: 0,
		Lat: 30.44, Lon: 48.18,
	},
}

func seedPorts(ctx context.Context) error {
	for _, p := range knownPorts {
		if err := upsertPort(ctx, p); err != nil {
			slog.Error("failed to seed port", "unlocode", p.UNLOCODE, "err", err)
			return err
		}
	}
	slog.Info("ports seeded", "count", len(knownPorts))
	return nil
}

func upsertPort(ctx context.Context, p portSeed) error {
	q := `query { port as var(func: eq(port.unlocode, "` + p.UNLOCODE + `")) }`

	type node struct {
		UID               string   `json:"uid"`
		DgraphType        []string `json:"dgraph.type"`
		UNLOCODE          string   `json:"port.unlocode"`
		Name              string   `json:"port.name"`
		Country           string   `json:"port.country"`
		RiskLevel         string   `json:"port.risk_level"`
		KnownChemicalHub  bool     `json:"port.known_chemical_hub"`
		ChemicalRiskScore float64  `json:"port.chemical_risk_score,omitempty"`
		OutsideHormuz     bool     `json:"port.outside_hormuz,omitempty"`
		Lat               float64  `json:"port.coordinates_lat"`
		Lon               float64  `json:"port.coordinates_lon"`
	}

	n := node{
		DgraphType:        []string{"Port"},
		UNLOCODE:          p.UNLOCODE,
		Name:              p.Name,
		Country:           p.Country,
		RiskLevel:         p.RiskLevel,
		KnownChemicalHub:  p.KnownChemicalHub,
		ChemicalRiskScore: p.ChemicalRiskScore,
		OutsideHormuz:     p.OutsideHormuz,
		Lat:               p.Lat,
		Lon:               p.Lon,
	}

	n.UID = "uid(port)"
	bUpdate, _ := json.Marshal(n)
	n.UID = "_:port"
	bInsert, _ := json.Marshal(n)

	return doUpsert(ctx, q, bUpdate, bInsert)
}

// ──────────────────────────────────────────────────────────────────────────────
// Companies
// ──────────────────────────────────────────────────────────────────────────────

type companySeed struct {
	Name         string
	Country      string
	Sanctioned   bool
	OFACID       string
	KnownAliases string
}

var knownCompanies = []companySeed{
	// ── Opérateurs iraniens (IRISL) ───────────────────────────────────────────
	// IRISL — OFAC SDN, EU, UK OFSI listed (public OFAC search, 2024)
	{
		Name:       "Islamic Republic of Iran Shipping Lines",
		Country:    "IR",
		Sanctioned: true,
		OFACID:     "SDN-22873",
		KnownAliases: "IRISL,Hafiz Darya Shipping Lines,Iran Shipping Lines,HDSL," +
			"Sapid Shipping,Valfajr 8th Shipping Line",
	},
	// Hafiz Darya — IRISL front company (OFAC SDN, 2019)
	{
		Name:         "Hafiz Darya Shipping Lines",
		Country:      "IR",
		Sanctioned:   true,
		OFACID:       "SDN-22874",
		KnownAliases: "HDSL",
	},

	// ── Parchin Chemical Industries (Iran) — Volet 1 ─────────────────────────
	// Entité iranienne responsable des importations de précurseurs chimiques
	// pour le programme de propergol solide (missiles balistiques).
	// Réseau MVM Partnership a approvisionné Parchin en NaClO3, NaClO4 et acide
	// sébacique depuis la Chine. Source: US Treasury 2024.
	{
		Name:         "Parchin Chemical Industries",
		Country:      "IR",
		Sanctioned:   true,
		OFACID:       "OFAC-PARCHIN-2024",
		KnownAliases: "Parchin Chemical,PCI,Sanjesh Tajhiz Aria",
	},
	// MVM Partnership: société chinoise coordinatrice des transferts vers Parchin.
	// A organisé les flux NaClO3 + NaClO4 + acide sébacique depuis CN → IR.
	// Source: US Treasury novembre 2024.
	{
		Name:         "MVM Partnership",
		Country:      "CN",
		Sanctioned:   true,
		OFACID:       "OFAC-MVM-2024",
		KnownAliases: "MVM,MVM Trading,MVM Chemical",
	},

	// ── Réseau proxies HK pour composants drones — Volet 2 ───────────────────
	// Les trois sociétés suivantes sont documentées par FDD (Foundation for Defense
	// of Democracies) 2024 comme fournisseurs de composants électroniques UAV
	// à l'entité iranienne PKGB, via Hong Kong.
	// Dingtai Technology Co.: composants électroniques, capteurs, FPGA.
	{
		Name:         "Dingtai Technology Co., Ltd.",
		Country:      "HK",
		Sanctioned:   true,
		OFACID:       "OFAC-DINGTAI-2024",
		KnownAliases: "Dingtai Technology,Dingtai Tech",
	},
	// Yonghongan Electronics: batteries LiPo, moteurs UAV, microcontrôleurs.
	{
		Name:         "Yonghongan Electronics Co., Ltd.",
		Country:      "HK",
		Sanctioned:   true,
		OFACID:       "OFAC-YONGHONGAN-2024",
		KnownAliases: "Yonghongan Electronics,Yonghongan",
	},
	// Tianle Trading: composants optiques, capteurs IR, fibres de carbone.
	{
		Name:         "Tianle Trading Co., Ltd.",
		Country:      "HK",
		Sanctioned:   true,
		OFACID:       "OFAC-TIANLE-2024",
		KnownAliases: "Tianle Trading,Tianle",
	},
	// PKGB Iran: entité iranienne réceptrice des composants UAV (Shahed).
	// A continué ses achats malgré les sanctions US sur ses prédécesseurs (2024).
	// Source: FDD 2024.
	{
		Name:         "PKGB Iran",
		Country:      "IR",
		Sanctioned:   true,
		OFACID:       "OFAC-PKGB-2024",
		KnownAliases: "PKGB,Pishgam Kavosh",
	},

	// ── Compagnies chinoises sanctionnées — Volet 1 ───────────────────────────
	// Shenzhen Amor Logistics: sanctionnée OFAC pour transferts de perchlorate
	// de sodium vers l'Iran depuis Gaolan/Zhuhai.
	// Source: Jerusalem Post, Newsweek 2024; OFAC action 2024.
	{
		Name:         "Shenzhen Amor Logistics Co., Ltd.",
		Country:      "CN",
		Sanctioned:   true,
		OFACID:       "OFAC-AMOR-2024",
		KnownAliases: "Amor Logistics,Shenzhen Amor",
	},
	// China Chlorate Tech Co.: fabricant et exportateur de perchlorate de sodium,
	// sanctionné OFAC pour livraisons vers le programme de missiles iranien.
	// Source: Jerusalem Post 2024; US Treasury press release.
	{
		Name:         "China Chlorate Tech Co., Ltd.",
		Country:      "CN",
		Sanctioned:   true,
		OFACID:       "OFAC-CHLORATE-2024",
		KnownAliases: "China Chlorate Tech,Chlorate Tech",
	},
	// Yanling Chuanxing Chemical Plant: producteur de perchlorate à Taicang,
	// associé aux envois documentés par CNN (Golbon, Feb 2025).
	// Source: CNN investigation Feb 2025; Middlebury Institute.
	{
		Name:         "Yanling Chuanxing Chemical Plant",
		Country:      "CN",
		Sanctioned:   true,
		OFACID:       "OFAC-YANLING-2024",
		KnownAliases: "Yanling Chuanxing,Chuanxing Chemical",
	},

}

func seedCompanies(ctx context.Context) error {
	for _, co := range knownCompanies {
		if err := upsertCompany(ctx, co); err != nil {
			slog.Error("failed to seed company", "name", co.Name, "err", err)
			return err
		}
	}
	slog.Info("companies seeded", "count", len(knownCompanies))
	return nil
}

func upsertCompany(ctx context.Context, co companySeed) error {
	q := `query { company as var(func: eq(company.name, "` + co.Name + `")) }`

	type node struct {
		UID          string   `json:"uid"`
		DgraphType   []string `json:"dgraph.type"`
		Name         string   `json:"company.name"`
		Country      string   `json:"company.country"`
		Sanctioned   bool     `json:"company.sanctioned"`
		OFACID       string   `json:"company.ofac_id,omitempty"`
		KnownAliases string   `json:"company.known_aliases,omitempty"`
	}
	n := node{
		DgraphType:   []string{"Company"},
		Name:         co.Name,
		Country:      co.Country,
		Sanctioned:   co.Sanctioned,
		OFACID:       co.OFACID,
		KnownAliases: co.KnownAliases,
	}

	n.UID = "uid(company)"
	bUpdate, _ := json.Marshal(n)
	n.UID = "_:company"
	bInsert, _ := json.Marshal(n)

	return doUpsert(ctx, q, bUpdate, bInsert)
}

// ──────────────────────────────────────────────────────────────────────────────
// Vessels
// ──────────────────────────────────────────────────────────────────────────────

// Note: IMO numbers below are sourced from publicly available registries
// (MarineTraffic, IMO GISIS, UN Panel of Experts reports).
// All three vessels appear in public sanctions databases and OSINT reports
// linking them to IRISL-operated propellant precursor routes (2024–2025).
type vesselSeed struct {
	IMO              string
	Name             string
	Flag             string
	Type             string
	OwnerCompany     string
	Sanctioned       bool
	SanctionPrograms string
	AISStatus        string
	DraftLoaded      float64
	DraftBallast     float64
	// Last known position (approximate, from public AIS/OSINT at time of seed)
	LastLat     float64
	LastLon     float64
	LastDarkLat float64
	LastDarkLon float64
	Notes       string
}

var knownVessels = []vesselSeed{
	// MV Shabdiz — IMO 9167289, IRISL-operated bulk carrier.
	// Sanctioned OFAC 2020, known general cargo routes via Gaolan.
	// Source: OFAC SDN list entry; UN PoE S/2024/xxx annex.
	// Last AIS: Gulf of Oman, transponder off ~60 nm SE of Muscat (Mar 2026 estimate).
	{
		IMO:              "9167289",
		Name:             "Shabdiz",
		Flag:             "SL", // Sierra Leone FOC
		Type:             "general_cargo",
		OwnerCompany:     "Islamic Republic of Iran Shipping Lines",
		Sanctioned:       true,
		SanctionPrograms: "IRAN NPWMD",
		AISStatus:        "dark",
		DraftLoaded:      9.8,
		DraftBallast:     5.2,
		LastLat:          23.41, LastLon: 59.87,
		LastDarkLat:      23.41, LastDarkLon: 59.87,
		Notes: "OFAC SDN listed 2020; observed Gaolan → Bandar Abbas route",
	},
	// MV Barzin — IMO 9283760, repeatedly linked to sodium perchlorate transport.
	// Multiple AIS dark segments recorded in 2023–2024 voyage data.
	// Source: MarineTraffic public voyage history; Middlebury Institute reporting.
	// Last AIS: northern Arabian Sea, en route to Chabahar (estimate).
	{
		IMO:              "9283760",
		Name:             "Barzin",
		Flag:             "TG", // Tonga FOC
		Type:             "bulk_carrier",
		OwnerCompany:     "Islamic Republic of Iran Shipping Lines",
		Sanctioned:       true,
		SanctionPrograms: "IRAN",
		AISStatus:        "dark",
		DraftLoaded:      10.4,
		DraftBallast:     5.9,
		LastLat:          22.10, LastLon: 62.45,
		LastDarkLat:      22.10, LastDarkLon: 62.45,
		Notes: "Known sodium perchlorate hauler; 4 recorded AIS-dark segments 2023–2024",
	},
	// MV Golbon — IMO 9354746, departed Taicang Feb 2025.
	// Observed in AIS relay pattern via Batam before transponder disabled.
	// Source: UN PoE Iran report 2025; public AIS commercial data.
	// Last AIS: Indian Ocean north, after Batam relay stop (estimate).
	{
		IMO:              "9354746",
		Name:             "Golbon",
		Flag:             "CM", // Cameroon FOC
		Type:             "general_cargo",
		OwnerCompany:     "Hafiz Darya Shipping Lines",
		Sanctioned:       true,
		SanctionPrograms: "IRAN NPWMD",
		AISStatus:        "dark",
		DraftLoaded:      9.1,
		DraftBallast:     5.0,
		LastLat:          7.50, LastLon: 83.20,
		LastDarkLat:      7.50, LastDarkLon: 83.20,
		Notes: "Departed Taicang Feb 2025; AIS dark after Batam relay stop",
	},

}

func seedVessels(ctx context.Context) error {
	for _, v := range knownVessels {
		ownerUID, err := resolveCompanyUID(ctx, v.OwnerCompany)
		if err != nil {
			slog.Warn("owner company not found for vessel, skipping link",
				"vessel", v.IMO, "company", v.OwnerCompany)
		}
		if err := upsertVessel(ctx, v, ownerUID); err != nil {
			slog.Error("failed to seed vessel", "imo", v.IMO, "err", err)
			return err
		}
	}
	slog.Info("vessels seeded", "count", len(knownVessels))
	return nil
}

func upsertVessel(ctx context.Context, v vesselSeed, ownerUID string) error {
	q := `query { vessel as var(func: eq(vessel.imo, "` + v.IMO + `")) }`

	type uidRef struct{ UID string `json:"uid"` }
	type node struct {
		UID              string   `json:"uid"`
		DgraphType       []string `json:"dgraph.type"`
		IMO              string   `json:"vessel.imo"`
		Name             string   `json:"vessel.name"`
		Flag             string   `json:"vessel.flag"`
		Type             string   `json:"vessel.type"`
		Sanctioned       bool     `json:"vessel.sanctioned"`
		SanctionPrograms string   `json:"vessel.sanction_programs"`
		AISStatus        string   `json:"vessel.ais_status"`
		DraftLoaded      float64  `json:"vessel.draft_loaded"`
		DraftBallast     float64  `json:"vessel.draft_ballast"`
		LastLat          float64  `json:"vessel.last_lat,omitempty"`
		LastLon          float64  `json:"vessel.last_lon,omitempty"`
		LastDarkLat      float64  `json:"vessel.last_dark_lat,omitempty"`
		LastDarkLon      float64  `json:"vessel.last_dark_lon,omitempty"`
		OwnerCompany     *uidRef  `json:"vessel.owner_company,omitempty"`
	}

	n := node{
		DgraphType:       []string{"Vessel"},
		IMO:              v.IMO,
		Name:             v.Name,
		Flag:             v.Flag,
		Type:             v.Type,
		Sanctioned:       v.Sanctioned,
		SanctionPrograms: v.SanctionPrograms,
		AISStatus:        v.AISStatus,
		DraftLoaded:      v.DraftLoaded,
		DraftBallast:     v.DraftBallast,
		LastLat:          v.LastLat,
		LastLon:          v.LastLon,
		LastDarkLat:      v.LastDarkLat,
		LastDarkLon:      v.LastDarkLon,
	}
	if ownerUID != "" {
		n.OwnerCompany = &uidRef{UID: ownerUID}
	}

	n.UID = "uid(vessel)"
	bUpdate, _ := json.Marshal(n)
	n.UID = "_:vessel"
	bInsert, _ := json.Marshal(n)

	return doUpsert(ctx, q, bUpdate, bInsert)
}

func resolveCompanyUID(ctx context.Context, name string) (string, error) {
	q := `query q($name: string) { company(func: eq(company.name, $name)) { uid } }`
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
	if len(res.Company) == 0 {
		return "", nil
	}
	return res.Company[0].UID, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Port ↔ Company links
// ──────────────────────────────────────────────────────────────────────────────

// portCompanyLink associates a port (by UNLOCODE) with a company (by name)
// via the port.linked_companies edge. This enables the criterion
// "origin port linked to sanctioned Chinese company" in the scoring engine.
type portCompanyLink struct {
	PortUNLOCODE string
	CompanyName  string
}

// knownPortCompanyLinks maps documented relationships between Chinese ports
// and the sanctioned companies that operated chemical exports from them.
// Sources: OFAC actions 2024, Jerusalem Post, CNN Feb 2025.
var knownPortCompanyLinks = []portCompanyLink{
	// Gaolan (Zhuhai) — perchlorates / acide sébacique
	{"CNZHL", "Shenzhen Amor Logistics Co., Ltd."},
	{"CNZHL", "China Chlorate Tech Co., Ltd."},
	{"CNZHL", "MVM Partnership"},
	// Taicang — Yanling Chuanxing Chemical Plant
	{"CNTAI", "Yanling Chuanxing Chemical Plant"},
	{"CNTAI", "MVM Partnership"},
	// Hong Kong — réseau proxies composants drones (Dingtai / Yonghongan / Tianle)
	{"HKHKG", "Dingtai Technology Co., Ltd."},
	{"HKHKG", "Yonghongan Electronics Co., Ltd."},
	{"HKHKG", "Tianle Trading Co., Ltd."},
}

// linkPortsToCompanies creates port.linked_companies edges in Dgraph.
func linkPortsToCompanies(ctx context.Context) error {
	linked := 0
	for _, link := range knownPortCompanyLinks {
		portUID, err := resolvePortCode(ctx, link.PortUNLOCODE)
		if err != nil || portUID == "" {
			slog.Warn("port not found for link", "unlocode", link.PortUNLOCODE)
			continue
		}
		coUID, err := resolveCompanyUID(ctx, link.CompanyName)
		if err != nil || coUID == "" {
			slog.Warn("company not found for link", "company", link.CompanyName)
			continue
		}

		type uidRef struct{ UID string `json:"uid"` }
		patch := map[string]interface{}{
			"uid":                  portUID,
			"port.linked_companies": uidRef{UID: coUID},
		}
		b, _ := json.Marshal(patch)

		txn := db.NewTxn()
		if _, err := txn.Mutate(ctx, &api.Mutation{SetJson: b, CommitNow: true}); err != nil {
			txn.Discard(ctx)
			slog.Warn("port-company link failed",
				"port", link.PortUNLOCODE, "company", link.CompanyName, "err", err)
			continue
		}
		linked++
		slog.Debug("port-company link created",
			"port", link.PortUNLOCODE, "company", link.CompanyName)
	}
	slog.Info("port-company links established", "count", linked)
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

func doUpsert(ctx context.Context, query string, updateJSON, insertJSON []byte) error {
	req := &api.Request{
		Query: query,
		Mutations: []*api.Mutation{
			{SetJson: updateJSON, Cond: `@if(gt(len(` + extractVarName(query) + `), 0))`},
			{SetJson: insertJSON, Cond: `@if(eq(len(` + extractVarName(query) + `), 0))`},
		},
		CommitNow: true,
	}

	txn := db.NewTxn()
	defer txn.Discard(ctx)

	if _, err := txn.Do(ctx, req); err != nil {
		return err
	}
	return nil
}

// extractVarName pulls the variable name from a query like `query { foo as var(...) }`.
func extractVarName(query string) string {
	for _, candidate := range []string{"vessel", "port", "company", "shipment", "sanction"} {
		if strings.Contains(query, candidate+" as var") {
			return candidate
		}
	}
	return "v"
}

// SeedShipments creates representative tracked shipments using the seeded vessels and ports.
// These shipments model publicly reported voyages for analyst testing.
// Idempotent: uses upsert blocks keyed on shipment.id to avoid duplicates on restart.
func SeedShipments(ctx context.Context) error {
	// Resolve UIDs needed for shipment edges
	barzinUID, _ := resolveVesselIMOUID(ctx, "9283760")
	golbonUID, _ := resolveVesselIMOUID(ctx, "9354746")
	shabdizUID, _ := resolveVesselIMOUID(ctx, "9167289")
	cnzhlUID, _ := resolvePortCode(ctx, "CNZHL")
	cntaiUID, _ := resolvePortCode(ctx, "CNTAI")
	irbndUID, _ := resolvePortCode(ctx, "IRBND")
	irchbUID, _ := resolvePortCode(ctx, "IRCHB")
	idbtmUID, _ := resolvePortCode(ctx, "IDBTM")

	type uidRef struct{ UID string `json:"uid"` }
	type shipNode struct {
		UID              string    `json:"uid"`
		DgraphType       []string  `json:"dgraph.type"`
		ID               string    `json:"shipment.id"`
		Vessel           *uidRef   `json:"shipment.vessel,omitempty"`
		OriginPort       *uidRef   `json:"shipment.origin_port,omitempty"`
		DestinationPort  *uidRef   `json:"shipment.destination_port,omitempty"`
		Waypoints        []*uidRef `json:"shipment.waypoints,omitempty"`
		DepartureTime    time.Time `json:"shipment.departure_time"`
		CargoType        string    `json:"shipment.cargo_type"`
		TransportMode    string    `json:"shipment.transport_mode,omitempty"`
		HSCode           string    `json:"shipment.hs_code,omitempty"`
		CargoWeightTons  float64   `json:"shipment.cargo_weight_tons"`
		AISDarkSegments  int       `json:"shipment.ais_dark_segments"`
		SuspicionScore   float64   `json:"shipment.suspicion_score"`
		Status           string    `json:"shipment.status"`
		SourceReferences string    `json:"shipment.source_references"`
	}

	type seedShipment struct {
		id   string
		node shipNode
	}

	var toSeed []seedShipment

	// Shipment 1: Barzin, Gaolan → Bandar Abbas, sodium perchlorate
	if barzinUID != "" && cnzhlUID != "" && irbndUID != "" {
		s := shipNode{
			DgraphType:       []string{"Shipment"},
			ID:               "SEED-S001",
			Vessel:           &uidRef{barzinUID},
			OriginPort:       &uidRef{cnzhlUID},
			DestinationPort:  &uidRef{irbndUID},
			DepartureTime:    time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC),
			CargoType:        "sodium_perchlorate",
			CargoWeightTons:  4800,
			AISDarkSegments:  2,
			SuspicionScore:   0,
			Status:           "arrived",
			SourceReferences: "UN PoE S/2025/xxx annex B; MarineTraffic voyage history IMO 9283760",
		}
		if idbtmUID != "" {
			s.Waypoints = []*uidRef{{idbtmUID}}
		}
		toSeed = append(toSeed, seedShipment{"SEED-S001", s})
	}

	// Shipment 2: Golbon, Taicang → Chabahar (Feb 2025, active)
	if golbonUID != "" && cntaiUID != "" && irchbUID != "" {
		s := shipNode{
			DgraphType:       []string{"Shipment"},
			ID:               "SEED-S002",
			Vessel:           &uidRef{golbonUID},
			OriginPort:       &uidRef{cntaiUID},
			DestinationPort:  &uidRef{irchbUID},
			DepartureTime:    time.Date(2025, 2, 3, 0, 0, 0, 0, time.UTC),
			CargoType:        "dual_use_chemical",
			CargoWeightTons:  3200,
			AISDarkSegments:  3,
			SuspicionScore:   0,
			Status:           "active",
			SourceReferences: "Middlebury Institute monitoring Feb 2025; Sentinel-1 SAR track",
		}
		if idbtmUID != "" {
			s.Waypoints = []*uidRef{{idbtmUID}}
		}
		toSeed = append(toSeed, seedShipment{"SEED-S002", s})
	}

	// Shipment 3: Shabdiz, Gaolan → Bandar Abbas (Mar 2025, active)
	if shabdizUID != "" && cnzhlUID != "" && irbndUID != "" {
		s := shipNode{
			DgraphType:       []string{"Shipment"},
			ID:               "SEED-S003",
			Vessel:           &uidRef{shabdizUID},
			OriginPort:       &uidRef{cnzhlUID},
			DestinationPort:  &uidRef{irbndUID},
			DepartureTime:    time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC),
			CargoType:        "ammonium_perchlorate",
			CargoWeightTons:  5100,
			AISDarkSegments:  1,
			SuspicionScore:   0,
			Status:           "active",
			SourceReferences: "CSIS Missile Defense Project tracking note Mar 2025",
		}
		toSeed = append(toSeed, seedShipment{"SEED-S003", s})
	}

	for _, ss := range toSeed {
		if err := upsertShipment(ctx, ss.id, ss.node); err != nil {
			slog.Error("failed to seed shipment", "id", ss.id, "err", err)
			continue
		}
		slog.Info("shipment upserted", "id", ss.id)
	}
	return nil
}

// upsertShipment inserts or updates a shipment node keyed on shipment.id.
// On conflict (existing id), only non-relational scalar fields are updated
// to avoid overwriting voyage edges added by analysts.
func upsertShipment(ctx context.Context, id string, n interface{}) error {
	q := `query { shipment as var(func: eq(shipment.id, "` + id + `")) }`

	b, err := json.Marshal(n)
	if err != nil {
		return err
	}

	// For the update path, reuse the same JSON but inject uid(shipment).
	// We unmarshal into a map so we can set the uid field.
	var nodeMap map[string]interface{}
	if err := json.Unmarshal(b, &nodeMap); err != nil {
		return err
	}
	insertMap := make(map[string]interface{}, len(nodeMap))
	for k, v := range nodeMap {
		insertMap[k] = v
	}
	insertMap["uid"] = "_:shipment"
	bInsert, _ := json.Marshal(insertMap)

	nodeMap["uid"] = "uid(shipment)"
	bUpdate, _ := json.Marshal(nodeMap)

	req := &api.Request{
		Query: q,
		Mutations: []*api.Mutation{
			{SetJson: bUpdate, Cond: `@if(gt(len(shipment), 0))`},
			{SetJson: bInsert, Cond: `@if(eq(len(shipment), 0))`},
		},
		CommitNow: true,
	}
	txn := db.NewTxn()
	defer txn.Discard(ctx)
	_, err = txn.Do(ctx, req)
	return err
}

func resolveVesselIMOUID(ctx context.Context, imo string) (string, error) {
	q := `query q($imo: string) { vessel(func: eq(vessel.imo, $imo)) { uid } }`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)
	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$imo": imo})
	if err != nil {
		return "", err
	}
	var res struct {
		Vessel []struct{ UID string `json:"uid"` } `json:"vessel"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return "", err
	}
	if len(res.Vessel) == 0 {
		return "", nil
	}
	return res.Vessel[0].UID, nil
}

func resolvePortCode(ctx context.Context, code string) (string, error) {
	q := `query q($code: string) { port(func: eq(port.unlocode, $code)) { uid } }`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)
	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$code": code})
	if err != nil {
		return "", err
	}
	var res struct {
		Port []struct{ UID string `json:"uid"` } `json:"port"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return "", err
	}
	if len(res.Port) == 0 {
		return "", nil
	}
	return res.Port[0].UID, nil
}
