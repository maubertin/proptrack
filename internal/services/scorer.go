package services

import (
	"fmt"
	"strings"

	"github.com/proptrack/proptrack/internal/models"
)

// ── Static intelligence tables ────────────────────────────────────────────────

// flagsOfConvenience: registries commonly used to obscure Iranian vessel ownership.
var flagsOfConvenience = map[string]bool{
	"TG": true, // Tonga
	"PW": true, // Palau
	"SL": true, // Sierra Leone
	"CM": true, // Cameroon
	"KM": true, // Comoros
	"MH": true, // Marshall Islands
	"TZ": true, // Tanzania
	"BZ": true, // Belize
	"PA": true, // Panama (context-dependent)
}

// cargoTypeRisk maps a cargo type string to its scoring weight and triggered flag.
// Organized by programme: propellant precursors, guidance systems, drone
// components, nuclear dual-use materials, and air-defence equipment.
type cargoTypeRisk struct {
	score    int
	flag     string
	programme string // analyst label: "ballistic" | "drone" | "nuclear" | "air_defense"
}

var cargoTypeMatrix = map[string]cargoTypeRisk{
	// ── PROGRAMME BALISTIQUE — précurseurs propergol ──────────────────────────
	// Codes HS 2829.11 / 2829.90 — perchlorates. Réseau MVM documenté OFAC 2024.
	"sodium_perchlorate":   {20, "high_risk_cargo", "ballistic"},
	"ammonium_perchlorate": {20, "high_risk_cargo", "ballistic"},
	"perchlorate":          {18, "high_risk_cargo", "ballistic"},
	"oxidizer":             {16, "high_risk_cargo", "ballistic"},
	"solid_propellant":     {20, "high_risk_cargo", "ballistic"},
	// HS 2829.11 — chlorate de sodium : précurseur direct du perchlorate de sodium.
	// Réseau MVM Partnership a coordonné approvisionnement NaClO3 + NaClO4 depuis CN
	// pour Parchin Chemical Industries (US Treasury 2024).
	"sodium_chlorate":      {18, "sodium_chlorate_precursor", "ballistic"},
	// HS 2917.13 — acide sébacique : plastifiant HTPB pour propergol composite.
	// Même réseau MVM-Parchin (US Treasury 2024).
	"sebacic_acid":         {15, "sebacic_acid_plasticizer", "ballistic"},
	"dual_use_chemical":    {15, "high_risk_cargo", "ballistic"},
	"nitrate_compound":     {15, "high_risk_cargo", "ballistic"},
	"planetary_mixer":      {18, "high_risk_cargo", "ballistic"},

	// ── PROGRAMME BALISTIQUE — systèmes de guidage ───────────────────────────
	// HS 8457/8458 — machines CNC pour fabrication de gyroscopes à fibre optique.
	// Navire Honestar (ex-Shun Kai Xing) intercepté avec CNC → gyros → missiles/drones.
	// Source : US Treasury novembre 2024.
	"cnc_machine":               {20, "cnc_dual_use_guidance", "guidance"},
	// HS 9014.20 — gyroscopes à fibre optique (FOG) pour INS missilier.
	"fiber_optic_gyroscope":     {22, "fiber_optic_gyroscope", "guidance"},
	// HS 9014.20 / 9015 — unité de navigation inertielle (IMU/INS).
	"inertial_navigation_unit":  {20, "inertial_navigation_unit", "guidance"},
	// HS 7601/7604 — alliages aluminium haute résistance (structures, réservoirs).
	// Contrôle MTCR. Indicateur secondaire — vérification contexte requise.
	"aluminum_alloy_structural": {10, "aluminum_alloy_mtcr", "ballistic"},
	// HS 7224 — acier maraging : centrifugeuses, ogives. Contrôle nucléaire NRC.
	// Partagé avec programme nucléaire.
	"maraging_steel":            {22, "maraging_steel_dual_use", "ballistic"},

	// ── PROGRAMME DRONES — composants Shahed et dérivés ──────────────────────
	// Sociétés écrans HK documentées : Dingtai Technology, Yonghongan Electronics,
	// Tianle Trading → entité iranienne PKGB. Source : FDD 2024.
	// HS 9014.20 — gyroscopes MEMS / IMU pour autopilote drone.
	"mems_gyroscope":        {18, "mems_gyroscope_drone", "drone"},
	// HS 8407.90 — moteurs à piston 50-100cc (clone MD550 Shahed-136).
	// Déclarés "moteurs de loisir" pour contournement export controls.
	"uav_engine":            {15, "drone_engine_piston", "drone"},
	// HS 8507.60 — batteries Li-Po haute densité pour drones longue endurance.
	"lipo_battery":          {12, "drone_lipo_battery", "drone"},
	// HS 8542.31 — microcontrôleurs STM32 / FPGA autopilote.
	// Origine US/EU mais transit via distributeurs CN/HK.
	"fpga_microcontroller":  {12, "fpga_microcontroller_drone", "drone"},
	// HS 9002.11 — capteurs optiques / caméras IR dual-use.
	"ir_optical_sensor":     {12, "ir_optical_sensor_drone", "drone"},
	// HS 5402.11 — fibres de carbone pour structure airframe.
	"carbon_fiber":          {10, "carbon_fiber_airframe", "drone"},
	// Générique composants électroniques UAV.
	"uav_electronics":       {15, "drone_electronics", "drone"},

	// ── PROGRAMME NUCLÉAIRE — matériaux dual-use ─────────────────────────────
	// HS 8414.10 — pompes turbomoléculaires : cascades centrifugeuses d'enrichissement.
	// Très contrôlé — quasi exclusivement par fret aérien + sociétés écrans.
	"vacuum_pump_turbomolecular": {18, "vacuum_pump_nuclear", "nuclear"},
	// HS 8504.40 — convertisseurs de fréquence 600 Hz : entraînement centrifugeuses.
	// Contrôle NSG dual-use Cat. 0B001.
	"frequency_converter_600hz": {20, "frequency_converter_nuclear", "nuclear"},
	// HS 6815.10 — graphite isotrope haute densité : réflecteurs neutrons.
	// Contrôle NSG.
	"isotropic_graphite":        {15, "isotropic_graphite_nuclear", "nuclear"},
	// HS 8109.20 — zirconium métal : gaine de combustible nucléaire.
	// Liste de surveillance IAEA.
	"zirconium_metal":           {18, "zirconium_nuclear", "nuclear"},
	// HS 2812.10 — hexafluorure d'uranium UF6 : enrichissement.
	// Quasi impossible à masquer — signal quasi certain si détecté.
	"uranium_hexafluoride":      {30, "uf6_enrichment", "nuclear"},
	// HS 2827.32 — trichlorure d'aluminium : conversion uranium.
	"aluminum_trichloride":      {12, "aluminum_trichloride_nuclear", "nuclear"},

	// ── DÉFENSE AÉRIENNE — HQ-9 / remplacement S-300 ─────────────────────────
	// HS 8526.10 — composants radar sol-air.
	// Navires Ro-Ro depuis Chine → ports militaires iraniens (imagerie satellite).
	"radar_components":     {15, "radar_air_defense", "air_defense"},
	"roro_military_cargo":  {12, "roro_military_equipment", "air_defense"},
}

// transportModeRisk scores the shipment transport modality.
// Drone components and nuclear dual-use materials predominantly travel by air
// or express courier (DHL/FedEx/SF Express via HKG → DXB → IKA) rather than
// maritime bulk — this evades standard customs inspection on cargo manifests.
type transportModeRisk struct {
	score int
	flag  string
}

var transportModeMatrix = map[string]transportModeRisk{
	// Air freight: harder to inspect, faster transit, favored for small dual-use items.
	"air": {8, "air_freight_mode"},
	// Express courier: DHL/FedEx/SF Express pattern documented for drone components
	// (Dingtai → PKGB supply chain, FDD 2024). Small packages, false manifests.
	"express_courier": {12, "express_courier_mode"},
	// Standard maritime (sea) = baseline, no additional score.
	"sea": {0, ""},
}

// chemicalRelayCountries: waypoint states used for AIS history laundering.
var chemicalRelayCountries = map[string]bool{
	"MY": true, // Malaysia (Penang, Port Klang)
	"ID": true, // Indonesia (Batam)
	"SG": true, // Singapore
	"AE": true, // UAE (Jebel Ali transshipment — sea)
}

// airHubRelayRisk: air freight hubs used as relay points on drone/dual-use routes.
// Hong Kong is the primary aggregation point for drone electronics (Dingtai network).
// Dubai (DXB) is the primary air relay to Tehran (IKA) for sanctioned goods.
type airHubRisk struct {
	score int
	flag  string
}

var airHubRelayMatrix = map[string]airHubRisk{
	// Hong Kong: primary drone electronics aggregation hub; Dingtai/Yonghongan/Tianle
	// documented transit point for PKGB-destined UAV components (FDD 2024).
	"HKHKG": {8, "hkg_air_hub"},
	// Dubai: primary air relay to Tehran — DXB → IKA documented route for
	// sanctioned goods avoiding direct China–Iran air routes.
	"AEDXB": {10, "dubai_air_relay"},
}

// irisOperatorKeywords: substrings present in IRISL-linked or sanctioned company names.
// Extended with MVM and proxy networks for ballistic/drone supply chains.
var irisOperatorKeywords = []string{
	// IRISL fleet
	"irisl",
	"islamic republic of iran shipping",
	"hafiz darya",
	"iran shipping",
	"valfajr",
	"sapid",
	"irinvestship",
	// MVM network (US Treasury 2024) — NaClO3/NaClO4/sebacic acid supply chain
	"mvm",
	// Parchin — Iranian end-user for propellant precursors
	"parchin",
	// HK drone proxy network (FDD 2024)
	"dingtai",
	"yonghongan",
	"tianle",
	// PKGB — Iranian UAV procurement entity
	"pkgb",
}

// ── VOLET 1 — Port d'origine + Type de navire ─────────────────────────────────

type portOriginRisk struct {
	score  int
	flag   string
	reason string
}

var portOriginMatrix = map[string]portOriginRisk{
	// ── Très élevé ────────────────────────────────────────────────────────────
	// Gaolan (Zhuhai): hub perchlorate documenté, Shenzhen Amor + China Chlorate.
	"CNZHL": {30, "gaolan_origin", "Gaolan (Zhuhai): documented NaClO4 export hub; sanctioned Shenzhen Amor Logistics and China Chlorate Tech Co."},
	// ── Élevé ─────────────────────────────────────────────────────────────────
	// Taicang: Golbon ~1 000t NaClO4 (CNN Feb 2025), Yanling Chuanxing.
	"CNTAI": {25, "taicang_origin", "Taicang: Golbon departed with ~1000t NaClO4 (CNN Feb 2025); Yanling Chuanxing Chemical Plant"},
	// Hong Kong: hub aggregation pour composants drones (Dingtai/Yonghongan/Tianle → PKGB).
	// Fret maritime et aérien. Source : FDD 2024.
	"HKHKG": {12, "hkg_origin", "Hong Kong: primary drone electronics aggregation hub; Dingtai/Yonghongan/Tianle → PKGB Iran route (FDD 2024)"},
	// ── Modéré ────────────────────────────────────────────────────────────────
	"CNTJN": {10, "moderate_risk_origin", "Tianjin: major industrial port; requires draft/cargo verification"},
	"CNTAO": {10, "moderate_risk_origin", "Qingdao: industrial port with petrochemical infrastructure"},
	// ── Faible ────────────────────────────────────────────────────────────────
	"CNSHA": {5, "low_risk_origin", "Shanghai: high volume hub; less probable for sensitive cargo"},
	"CNNGB": {5, "low_risk_origin", "Ningbo: high volume container hub; low signal-to-noise ratio"},
}

type vesselTypeRisk struct {
	score  int
	flag   string
	reason string
}

var vesselTypeMatrix = map[string]vesselTypeRisk{
	"chemical_tanker": {15, "chemical_tanker_type", "Chemical tanker: highest cargo-type specificity for liquid chemicals"},
	"general_cargo":   {8, "general_cargo_type", "General cargo: IRISL preferred type (Shabdiz, Golbon); flexible for bulk chemical loads"},
	"multipurpose":    {8, "general_cargo_type", "Multipurpose: equivalent to general cargo for precursor transport"},
	"bulk_carrier":    {5, "bulk_carrier_type", "Bulk carrier: suited for solid perchlorate in supersacks (Barzin profile)"},
	// Ro-Ro vessels: adapted for heavy military/air-defense equipment.
	"ro_ro":           {10, "roro_vessel_type", "Ro-Ro: adapted for heavy military equipment (HQ-9/air-defense delivery profile)"},
}

// ── VOLET 2 — Destination et routes alternatives ──────────────────────────────

type destinationRisk struct {
	score         int
	flag          string
	outsideHormuz bool
	reason        string
}

var destinationMatrix = map[string]destinationRisk{
	// Chabahar: hors Hormuz, accès direct océan Indien, route active mars 2026.
	"IRCHB": {20, "chabahar_outside_hormuz", true,
		"Chabahar: sole Iranian port with Indian Ocean direct access; outside Hormuz; active route Mar 2026"},
	// Bandar Abbas: terminal principal, installations IRGC.
	"IRBND": {15, "bandar_abbas_destination", false,
		"Bandar Abbas: primary Iranian freight terminal; adjacent IRGC naval facilities"},
	"IRKHO": {10, "iran_destination", false, "Khorramshahr: Iranian river port"},
	"IRBUZ": {10, "iran_destination", false, "Bushehr: Iranian nuclear site port"},
	// Téhéran (IKA) : destination fret aérien pour composants drones/nucléaire.
	// Route DXB → IKA documentée pour composants sanctionnés.
	"IRTHR": {18, "tehran_air_destination", false,
		"Tehran (IKA): primary air freight destination; drone components and guidance systems via DXB → IKA route"},
}

var gulfOfOmanBBox = struct{ minLat, maxLat, minLon, maxLon float64 }{
	minLat: 22.0, maxLat: 27.0,
	minLon: 55.0, maxLon: 65.0,
}

const pakistanBorderClosed = true
const pakistanBorderClosureDate = "2025-06-16"

// ── Main scoring function ─────────────────────────────────────────────────────

func ComputeShipmentScore(s models.Shipment) models.SuspicionResult {
	var (
		total int
		flags []string
	)

	// ════════════════════════════════════════════════════════════════════════
	// VOLET 1 — PORT D'ORIGINE + TYPE DE NAVIRE
	// ════════════════════════════════════════════════════════════════════════

	// ── Criterion 1: Granular origin port scoring ─────────────────────────
	if s.OriginPort != nil {
		portScore := 0
		portFlag := ""
		if risk, ok := portOriginMatrix[s.OriginPort.UNLOCODE]; ok {
			portScore = risk.score
			portFlag = risk.flag
		} else if s.OriginPort.ChemicalRiskScore > 0 {
			portScore = int(s.OriginPort.ChemicalRiskScore)
			portFlag = "chemical_hub_origin"
		} else if s.OriginPort.KnownChemicalHub {
			portScore = 15
			portFlag = "chemical_hub_origin"
		}
		if portScore > 0 {
			total += portScore
			flags = append(flags, portFlag)
		}

		// ── Criterion 1b: Origin port linked to sanctioned company ────────
		// Covers CN chemical exporters (Shenzhen Amor, Yanling) AND HK drone
		// proxies (Dingtai, Yonghongan, Tianle) linked to their respective ports.
		for _, co := range s.OriginPort.LinkedCompanies {
			if co != nil && co.Sanctioned {
				total += 10
				flags = append(flags, "sanctioned_origin_company")
				break
			}
		}
	}

	// ── Criterion 2: Vessel type specificity ─────────────────────────────
	if s.Vessel != nil && s.Vessel.Type != "" {
		vtype := strings.ToLower(s.Vessel.Type)
		if risk, ok := vesselTypeMatrix[vtype]; ok {
			total += risk.score
			flags = append(flags, risk.flag)
		}
	}

	// ── Criterion 3: Sanctioned operator (IRISL / MVM / proxy networks) ──
	if s.Vessel != nil && s.Vessel.OwnerCompany != nil {
		cn := strings.ToLower(s.Vessel.OwnerCompany.Name)
		isProxied := false
		for _, kw := range irisOperatorKeywords {
			if strings.Contains(cn, kw) {
				isProxied = true
				break
			}
		}
		if !isProxied && s.Vessel.OwnerCompany.Sanctioned {
			isProxied = true
		}
		if isProxied {
			total += 25
			flags = append(flags, "irisl_operator")
		}
	}

	// ── Criterion 4: Vessel on OFAC/EU/UK sanctions list (+20) ───────────
	if s.Vessel != nil && s.Vessel.Sanctioned {
		total += 20
		flags = append(flags, "sanctioned_vessel")
	}

	// ── Criterion 5: Cargo type — differentiated risk matrix ─────────────
	// Replaces flat +20 boolean with per-cargo-type score based on programme
	// and intelligence confidence level.
	if risk, ok := cargoTypeMatrix[strings.ToLower(s.CargoType)]; ok {
		total += risk.score
		flags = append(flags, risk.flag)
	}

	// ── Criterion 5b: Transport mode ─────────────────────────────────────
	// Air freight and express courier routes are harder to inspect and are the
	// documented vectors for drone components and nuclear dual-use materials.
	if s.TransportMode != "" {
		if risk, ok := transportModeMatrix[strings.ToLower(s.TransportMode)]; ok && risk.score > 0 {
			total += risk.score
			flags = append(flags, risk.flag)
		}
	}

	// ── Criterion 6: Draft delta suggests heavy chemical load (+10) ───────
	if s.Vessel != nil {
		diff := s.Vessel.DraftLoaded - s.Vessel.DraftBallast
		if diff > 3.0 {
			total += 10
			flags = append(flags, "heavy_draft_delta")
		}
	}

	// ── Criterion 7: Flag of convenience (+10) ────────────────────────────
	if s.Vessel != nil && flagsOfConvenience[strings.ToUpper(s.Vessel.Flag)] {
		total += 10
		flags = append(flags, "flag_of_convenience")
	}

	// ── Criterion 7b: Vessel tonnage — bulk/general cargo profile ─────────
	// DWT 10k–35k t is the typical range for CN→IR chemical/precursor loads
	// (Handysize / Handymax bulkers, IRISL general cargo vessels).
	// Uses DWT preferentially; falls back to GRT×0.65 estimate if DWT absent.
	if s.Vessel != nil {
		dwt := s.Vessel.DWT
		if dwt <= 0 && s.Vessel.GRT > 0 {
			dwt = s.Vessel.GRT * 0.65
		}
		switch {
		case dwt >= 10000 && dwt <= 35000:
			total += 8
			flags = append(flags, "bulk_carrier_tonnage_range")
		case dwt > 35000:
			total += 4
			flags = append(flags, "large_vessel_size")
		case dwt >= 3000:
			total += 2
			flags = append(flags, "medium_vessel_size")
		}
	}

	// ════════════════════════════════════════════════════════════════════════
	// VOLET 2 — DESTINATION + ROUTES ALTERNATIVES
	// ════════════════════════════════════════════════════════════════════════

	// ── Criterion 8: Granular destination scoring ─────────────────────────
	destOutsideHormuz := false
	if s.DestinationPort != nil {
		if risk, ok := destinationMatrix[s.DestinationPort.UNLOCODE]; ok {
			total += risk.score
			flags = append(flags, risk.flag)
			destOutsideHormuz = risk.outsideHormuz
		} else if s.DestinationPort.OutsideHormuz {
			total += 15
			flags = append(flags, "iran_destination_outside_hormuz")
			destOutsideHormuz = true
		} else if strings.ToUpper(s.DestinationPort.Country) == "IR" {
			total += 10
			flags = append(flags, "iran_destination")
		}
	}

	// ── Criterion 9: AIS dark — Gulf of Oman vs elsewhere ────────────────
	if s.AISDarkSegments > 0 {
		darkInGulfOfOman := false
		if s.Vessel != nil {
			lat := s.Vessel.LastDarkLat
			lon := s.Vessel.LastDarkLon
			if lat >= gulfOfOmanBBox.minLat && lat <= gulfOfOmanBBox.maxLat &&
				lon >= gulfOfOmanBBox.minLon && lon <= gulfOfOmanBBox.maxLon {
				darkInGulfOfOman = true
			}
		}
		if darkInGulfOfOman {
			total += 15
			flags = append(flags, "ais_dark_gulf_oman")
		} else {
			total += 10
			flags = append(flags, "ais_dark")
		}
	}

	// ── Criterion 10: Pakistan border closure + Chabahar (+5) ─────────────
	if pakistanBorderClosed && destOutsideHormuz {
		total += 5
		flags = append(flags, "pakistan_border_alt_route")
	}

	// ── Criterion 11: Waypoint relay — maritime (sea) ─────────────────────
	// Malaysia/Indonesia/Singapore: AIS history laundering for bulk cargo.
	seaRelayFound := false
	airHubFound := false
	for _, wp := range s.Waypoints {
		if wp == nil {
			continue
		}
		if !seaRelayFound && chemicalRelayCountries[strings.ToUpper(wp.Country)] {
			total += 5
			flags = append(flags, "relay_waypoint")
			seaRelayFound = true
		}
		// ── Criterion 11b: Waypoint relay — air hub ───────────────────────
		// HKG (drone electronics aggregation) and DXB (Iran air relay).
		if !airHubFound {
			if risk, ok := airHubRelayMatrix[wp.UNLOCODE]; ok {
				total += risk.score
				flags = append(flags, risk.flag)
				airHubFound = true
			}
		}
	}

	// ── Criterion 12: Prior Iran delivery by same vessel (+5) ─────────────
	if strings.Contains(s.SuspicionFlags, "prior_iran_delivery") {
		total += 5
		flags = append(flags, "prior_iran_delivery")
	}

	// ── Criterion 13: Cross-source identity discrepancies ─────────────────
	// Injected from vessel.verify_discrepancies field, populated by
	// VesselVerifier background service (VesselFinder + MyShipTracking).
	// Discrepancies indicate identity fraud or sanctions evasion tactics.
	if s.Vessel != nil && s.Vessel.VerifyDiscrepancies != "" {
		disc := s.Vessel.VerifyDiscrepancies
		if strings.Contains(disc, "name_mismatch") || strings.Contains(disc, "flag_mismatch") ||
			strings.Contains(disc, "mmsi_conflict") {
			total += 15
			flags = append(flags, "identity_discrepancy")
		}
		if strings.Contains(disc, "position_spoofing") {
			total += 20
			flags = append(flags, "ais_position_spoofing")
		}
		if strings.Contains(disc, "source_not_found") {
			// Vessel absent from tracking databases — reinforces AIS dark signal
			total += 10
			flags = append(flags, "unverifiable_vessel")
		}
	}

	threshold := classifyThreshold(total)
	summary := buildSummary(s, total, threshold, flags)

	return models.SuspicionResult{
		TotalScore:     total,
		Threshold:      threshold,
		TriggeredFlags: flags,
		Summary:        summary,
	}
}

func classifyThreshold(score int) models.ScoreThreshold {
	switch {
	case score >= 76:
		return models.ThresholdCritical
	case score >= 56:
		return models.ThresholdHigh
	case score >= 31:
		return models.ThresholdMedium
	default:
		return models.ThresholdLow
	}
}

func buildSummary(s models.Shipment, score int, threshold models.ScoreThreshold, flags []string) string {
	vesselName := "unknown vessel"
	if s.Vessel != nil && s.Vessel.Name != "" {
		vesselName = s.Vessel.Name
	}
	originName := "unknown origin"
	if s.OriginPort != nil && s.OriginPort.Name != "" {
		originName = s.OriginPort.Name
	}
	destName := "unknown destination"
	if s.DestinationPort != nil && s.DestinationPort.Name != "" {
		destName = s.DestinationPort.Name
	}

	flagStr := "none"
	if len(flags) > 0 {
		flagStr = strings.Join(flags, ", ")
	}

	actionMap := map[models.ScoreThreshold]string{
		models.ThresholdLow:      "Routine monitoring — no immediate action required.",
		models.ThresholdMedium:   "Enhanced tracking — assign analyst review.",
		models.ThresholdHigh:     "ALERT — escalate to senior analyst; coordinate with partner agencies.",
		models.ThresholdCritical: "CRITICAL — immediate report; consider interdiction referral.",
	}

	contextNote := ""
	for _, f := range flags {
		switch f {
		case "pakistan_border_alt_route":
			contextNote += fmt.Sprintf(" [Pakistan–Iran land border closed since %s — Chabahar is primary remaining delivery route.]", pakistanBorderClosureDate)
		case "ais_dark_gulf_oman":
			contextNote += " [AIS blackout in Gulf of Oman — likely heading to Chabahar/Hormuz under cover of darkness.]"
		case "chabahar_outside_hormuz":
			contextNote += " [Chabahar: outside Hormuz chokepoint, Indian Ocean direct access — active March 2026 route.]"
		case "express_courier_mode":
			contextNote += " [Express courier pattern — cross-reference ImportYeti air freight manifests and US CBP data.]"
		case "hkg_air_hub":
			contextNote += " [HKG transit — check Dingtai/Yonghongan/Tianle consignee records (FDD 2024 PKGB network).]"
		case "maraging_steel_dual_use":
			contextNote += " [Maraging steel detected — MTCR Annex Cat. 0B001; notify NSG partner states.]"
		case "frequency_converter_nuclear":
			contextNote += " [600 Hz frequency converter — NSG dual-use Cat. 0B001; potential centrifuge drive system.]"
		case "uf6_enrichment":
			contextNote += " [UF6 detected — IAEA immediate notification required.]"
		}
	}

	return fmt.Sprintf(
		"Shipment %s: %s routing %s → %s scores %d (%s). "+
			"Triggered criteria: [%s]. %s%s",
		s.ID, vesselName, originName, destName,
		score, strings.ToUpper(string(threshold)),
		flagStr, actionMap[threshold], contextNote,
	)
}
