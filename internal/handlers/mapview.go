package handlers

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/proptrack/proptrack/internal/db"
	"github.com/proptrack/proptrack/internal/models"
	"github.com/proptrack/proptrack/internal/services"
)

// ── Response types ────────────────────────────────────────────────────────────

type VesselMapPoint struct {
	IMO                 string       `json:"imo"`
	Name                string       `json:"name"`
	Flag                string       `json:"flag"`
	Type                string       `json:"type"`
	Sanctioned          bool         `json:"sanctioned"`
	AISStatus           string       `json:"ais_status"`
	Lat                 float64      `json:"lat"`
	Lon                 float64      `json:"lon"`
	DraftLoaded         float64      `json:"draft_loaded"`
	SuspicionScore      float64      `json:"suspicion_score"`
	ScoreLevel          string       `json:"score_level"`
	VerifyStatus        string       `json:"verify_status"`
	VerifyDiscrepancies string       `json:"verify_discrepancies"`
	VerifyPosGapKm      float64      `json:"verify_pos_gap_km,omitempty"`
	ActiveShipment      *ShipSummary `json:"active_shipment,omitempty"`
}

type ShipSummary struct {
	ID          string  `json:"id"`
	CargoType   string  `json:"cargo_type"`
	CargoWeight float64 `json:"cargo_weight_tons"`
	Status      string  `json:"status"`
	OriginCode  string  `json:"origin_code"`
	OriginName  string  `json:"origin_name"`
	DestCode    string  `json:"dest_code"`
	DestName    string  `json:"dest_name"`
	Score       float64 `json:"score"`
	Flags       string  `json:"flags"`
}

type TrackPoint struct {
	UNLOCODE string    `json:"unlocode"`
	Name     string    `json:"name"`
	Country  string    `json:"country"`
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	Role     string    `json:"role"` // "origin" | "waypoint" | "destination"
	ETA      time.Time `json:"eta,omitempty"`
}

type CriterionDetail struct {
	Flag   string `json:"flag"`
	Points int    `json:"points"`
	Label  string `json:"label"`
}

type ShipmentTrackResponse struct {
	ShipmentID    string            `json:"shipment_id"`
	VesselIMO     string            `json:"vessel_imo"`
	VesselName    string            `json:"vessel_name"`
	VesselFlag    string            `json:"vessel_flag"`
	VesselType    string            `json:"vessel_type"`
	DraftLoaded   float64           `json:"draft_loaded"`
	CargoType     string            `json:"cargo_type"`
	CargoWeight   float64           `json:"cargo_weight_tons"`
	Track         []TrackPoint      `json:"track"`
	DepartureTime time.Time         `json:"departure_time"`
	EstArrival    time.Time         `json:"estimated_arrival"`
	ActualArrival time.Time         `json:"actual_arrival,omitempty"`
	AISDarkSegs   int               `json:"ais_dark_segments"`
	LastDarkLat   float64           `json:"last_dark_lat,omitempty"`
	LastDarkLon   float64           `json:"last_dark_lon,omitempty"`
	Score         int               `json:"score"`
	ScoreLevel    string            `json:"score_level"`
	Flags         []string          `json:"flags"`
	Criteria      []CriterionDetail `json:"criteria"`
	Summary       string            `json:"summary"`
	Status        string            `json:"status"`
}

type MapOverviewResponse struct {
	TotalVessels    int `json:"total_vessels"`
	DarkVessels     int `json:"dark_vessels"`
	SanctionedCount int `json:"sanctioned_count"`
	ActiveShipments int `json:"active_shipments"`
	CriticalCount   int `json:"critical_count"`
	HighCount       int `json:"high_count"`
	MediumCount     int `json:"medium_count"`
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// MapVessels handles GET /api/v1/map/vessels
// Returns all tracked vessels with position and active shipment for map rendering.
func MapVessels(c *gin.Context) {
	ctx := context.Background()

	q := `{
  vessels(func: has(vessel.imo)) {
    uid
    vessel.imo
    vessel.name
    vessel.flag
    vessel.type
    vessel.sanctioned
    vessel.ais_status
    vessel.last_lat
    vessel.last_lon
    vessel.draft_loaded
    vessel.suspicion_score
    vessel.verify_status
    vessel.verify_discrepancies
    vessel.verify_pos_gap_km
    vessel.last_seen_port {
      port.coordinates_lat
      port.coordinates_lon
      port.name
    }
    ~shipment.vessel @filter(eq(shipment.status, "active") OR eq(shipment.status, "arrived") OR eq(shipment.status, "monitoring")) {
      shipment.id
      shipment.cargo_type
      shipment.cargo_weight_tons
      shipment.suspicion_score
      shipment.suspicion_flags
      shipment.status
      shipment.origin_port { port.unlocode port.name port.coordinates_lat port.coordinates_lon }
      shipment.destination_port { port.unlocode port.name port.coordinates_lat port.coordinates_lon }
    }
  }
}`

	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Query(ctx, q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var raw struct {
		Vessels []struct {
			UID                 string  `json:"uid"`
			IMO                 string  `json:"vessel.imo"`
			Name                string  `json:"vessel.name"`
			Flag                string  `json:"vessel.flag"`
			Type                string  `json:"vessel.type"`
			Sanctioned          bool    `json:"vessel.sanctioned"`
			AISStatus           string  `json:"vessel.ais_status"`
			LastLat             float64 `json:"vessel.last_lat"`
			LastLon             float64 `json:"vessel.last_lon"`
			DraftLoaded         float64 `json:"vessel.draft_loaded"`
			Score               float64 `json:"vessel.suspicion_score"`
			VerifyStatus        string  `json:"vessel.verify_status"`
			VerifyDiscrepancies string  `json:"vessel.verify_discrepancies"`
			VerifyPosGapKm      float64 `json:"vessel.verify_pos_gap_km"`
			LastSeenPort        []struct {
				Lat  float64 `json:"port.coordinates_lat"`
				Lon  float64 `json:"port.coordinates_lon"`
				Name string  `json:"port.name"`
			} `json:"vessel.last_seen_port"`
			Shipments []struct {
				ID          string  `json:"shipment.id"`
				CargoType   string  `json:"shipment.cargo_type"`
				CargoWeight float64 `json:"shipment.cargo_weight_tons"`
				Score       float64 `json:"shipment.suspicion_score"`
				Flags       string  `json:"shipment.suspicion_flags"`
				Status      string  `json:"shipment.status"`
				Origin []struct {
					Code string  `json:"port.unlocode"`
					Name string  `json:"port.name"`
					Lat  float64 `json:"port.coordinates_lat"`
					Lon  float64 `json:"port.coordinates_lon"`
				} `json:"shipment.origin_port"`
				Dest []struct {
					Code string  `json:"port.unlocode"`
					Name string  `json:"port.name"`
					Lat  float64 `json:"port.coordinates_lat"`
					Lon  float64 `json:"port.coordinates_lon"`
				} `json:"shipment.destination_port"`
			} `json:"~shipment.vessel"`
		} `json:"vessels"`
	}

	if err := json.Unmarshal(resp.Json, &raw); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unmarshal error"})
		return
	}

	out := make([]VesselMapPoint, 0, len(raw.Vessels))
	for _, v := range raw.Vessels {
		lat, lon := v.LastLat, v.LastLon
		// Fall back to last seen port position if no direct coordinates
		if lat == 0 && lon == 0 {
			if len(v.LastSeenPort) > 0 {
				lat = v.LastSeenPort[0].Lat
				lon = v.LastSeenPort[0].Lon
			} else if len(v.Shipments) > 0 && len(v.Shipments[0].Origin) > 0 {
				// Use active shipment origin port as last resort
				lat = v.Shipments[0].Origin[0].Lat
				lon = v.Shipments[0].Origin[0].Lon
			}
		}

		pt := VesselMapPoint{
			IMO:                 v.IMO,
			Name:                v.Name,
			Flag:                v.Flag,
			Type:                v.Type,
			Sanctioned:          v.Sanctioned,
			AISStatus:           v.AISStatus,
			Lat:                 lat,
			Lon:                 lon,
			DraftLoaded:         v.DraftLoaded,
			SuspicionScore:      v.Score,
			ScoreLevel:          scoreLevel(int(v.Score)),
			VerifyStatus:        v.VerifyStatus,
			VerifyDiscrepancies: v.VerifyDiscrepancies,
			VerifyPosGapKm:      v.VerifyPosGapKm,
		}

		if len(v.Shipments) > 0 {
			s := v.Shipments[0]
			ss := &ShipSummary{
				ID:          s.ID,
				CargoType:   s.CargoType,
				CargoWeight: s.CargoWeight,
				Score:       s.Score,
				Flags:       s.Flags,
				Status:      s.Status,
			}
			if len(s.Origin) > 0 {
				ss.OriginCode = s.Origin[0].Code
				ss.OriginName = s.Origin[0].Name
			}
			if len(s.Dest) > 0 {
				ss.DestCode = s.Dest[0].Code
				ss.DestName = s.Dest[0].Name
			}
			pt.ActiveShipment = ss
		}
		out = append(out, pt)
	}
	c.JSON(http.StatusOK, out)
}

// MapShipmentTrack handles GET /api/v1/map/shipment/:id/track
// Returns the full voyage track with time interpolation and scoring breakdown.
func MapShipmentTrack(c *gin.Context) {
	id := c.Param("id")
	ctx := context.Background()

	shipment, err := fetchFullShipment(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if shipment == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "shipment not found"})
		return
	}

	// Build geographic track
	track := buildTrack(shipment)

	// Interpolate ETAs for each track point
	interpolateETAs(track, shipment.DepartureTime, shipment.EstimatedArrival)

	// Compute score with criteria breakdown
	result := services.ComputeShipmentScore(*shipment)
	criteria := expandCriteria(result.TriggeredFlags)

	lastDarkLat, lastDarkLon := 0.0, 0.0
	if shipment.Vessel != nil {
		lastDarkLat = shipment.Vessel.LastDarkLat
		lastDarkLon = shipment.Vessel.LastDarkLon
	}

	resp := ShipmentTrackResponse{
		ShipmentID:    shipment.ID,
		CargoType:     shipment.CargoType,
		CargoWeight:   shipment.CargoWeightTons,
		Track:         track,
		DepartureTime: shipment.DepartureTime,
		EstArrival:    shipment.EstimatedArrival,
		ActualArrival: shipment.ActualArrival,
		AISDarkSegs:   shipment.AISDarkSegments,
		LastDarkLat:   lastDarkLat,
		LastDarkLon:   lastDarkLon,
		Score:         result.TotalScore,
		ScoreLevel:    string(result.Threshold),
		Flags:         result.TriggeredFlags,
		Criteria:      criteria,
		Summary:       result.Summary,
		Status:        shipment.Status,
	}
	if shipment.Vessel != nil {
		resp.VesselIMO = shipment.Vessel.IMO
		resp.VesselName = shipment.Vessel.Name
		resp.VesselFlag = shipment.Vessel.Flag
		resp.VesselType = shipment.Vessel.Type
		resp.DraftLoaded = shipment.Vessel.DraftLoaded
	}

	c.JSON(http.StatusOK, resp)
}

// MapOverview handles GET /api/v1/map/overview
// Returns summary counters for the dashboard header.
func MapOverview(c *gin.Context) {
	ctx := context.Background()
	q := `{
  all(func: has(vessel.imo)) { uid vessel.sanctioned vessel.ais_status }
  active(func: anyofterms(shipment.status, "active arrived monitoring")) { uid shipment.suspicion_score }
}`
	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.Query(ctx, q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var raw struct {
		All []struct {
			Sanctioned bool   `json:"vessel.sanctioned"`
			AISStatus  string `json:"vessel.ais_status"`
		} `json:"all"`
		Active []struct {
			Score float64 `json:"shipment.suspicion_score"`
		} `json:"active"`
	}
	if err := json.Unmarshal(resp.Json, &raw); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unmarshal"})
		return
	}

	ov := MapOverviewResponse{
		TotalVessels:    len(raw.All),
		ActiveShipments: len(raw.Active),
	}
	for _, v := range raw.All {
		if v.Sanctioned {
			ov.SanctionedCount++
		}
		if v.AISStatus == "dark" {
			ov.DarkVessels++
		}
	}
	for _, s := range raw.Active {
		switch {
		case s.Score >= 76:
			ov.CriticalCount++
		case s.Score >= 56:
			ov.HighCount++
		case s.Score >= 31:
			ov.MediumCount++
		}
	}
	c.JSON(http.StatusOK, ov)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func buildTrack(s *models.Shipment) []TrackPoint {
	var pts []TrackPoint
	if s.OriginPort != nil {
		pts = append(pts, portToTrackPoint(s.OriginPort, "origin"))
	}
	for _, wp := range s.Waypoints {
		if wp != nil {
			pts = append(pts, portToTrackPoint(wp, "waypoint"))
		}
	}
	if s.DestinationPort != nil {
		pts = append(pts, portToTrackPoint(s.DestinationPort, "destination"))
	}
	return pts
}

func portToTrackPoint(p *models.Port, role string) TrackPoint {
	return TrackPoint{
		UNLOCODE: p.UNLOCODE,
		Name:     p.Name,
		Country:  p.Country,
		Lat:      p.Lat,
		Lon:      p.Lon,
		Role:     role,
	}
}

// interpolateETAs assigns an estimated time to each track point based on
// proportional distance along the route (Haversine).
func interpolateETAs(track []TrackPoint, departure, arrival time.Time) {
	if len(track) < 2 || departure.IsZero() {
		return
	}
	// Default arrival if not set: departure + 45 days (typical China→Iran voyage)
	if arrival.IsZero() {
		arrival = departure.Add(45 * 24 * time.Hour)
	}
	totalDuration := arrival.Sub(departure)

	// Compute cumulative distances
	dists := make([]float64, len(track))
	dists[0] = 0
	total := 0.0
	for i := 1; i < len(track); i++ {
		d := haversineKm(track[i-1].Lat, track[i-1].Lon, track[i].Lat, track[i].Lon)
		total += d
		dists[i] = total
	}
	if total == 0 {
		return
	}
	for i := range track {
		ratio := dists[i] / total
		track[i].ETA = departure.Add(time.Duration(float64(totalDuration) * ratio))
	}
}

func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371.0
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func scoreLevel(score int) string {
	switch {
	case score >= 76:
		return "critical"
	case score >= 56:
		return "high"
	case score >= 31:
		return "medium"
	default:
		return "low"
	}
}

// criteriaLabels maps flag identifiers to human-readable analyst labels and point values.
var criteriaLabels = map[string]struct {
	Label  string
	Points int
}{
	// ── Ports d'origine ───────────────────────────────────────────────────
	"gaolan_origin":             {"Gaolan (Zhuhai) — documented NaClO₄ hub", 30},
	"taicang_origin":            {"Taicang — Golbon/NaClO₄ documented departure", 25},
	"hkg_origin":                {"Hong Kong — drone electronics hub (Dingtai/PKGB)", 12},
	"moderate_risk_origin":      {"Moderate-risk CN port (Tianjin / Qingdao)", 10},
	"low_risk_origin":           {"Low-risk CN port (Shanghai / Ningbo)", 5},
	"chemical_hub_origin":       {"Known chemical export hub", 15},
	"sanctioned_origin_company": {"Sanctioned company at origin port", 10},
	// ── Type de navire ────────────────────────────────────────────────────
	"chemical_tanker_type": {"Chemical tanker vessel type", 15},
	"general_cargo_type":   {"General cargo — IRISL preferred type", 8},
	"bulk_carrier_type":    {"Bulk carrier", 5},
	"roro_vessel_type":     {"Ro-Ro vessel — air-defense / heavy military cargo profile", 10},
	// ── Opérateurs ────────────────────────────────────────────────────────
	"irisl_operator":   {"IRISL / MVM / sanctioned proxy operator", 25},
	"sanctioned_vessel": {"Vessel on OFAC / EU / UK sanctions list", 20},
	// ── Programme balistique — précurseurs propergol ──────────────────────
	"high_risk_cargo":           {"Perchlorate / oxidiser / propellant precursor", 20},
	"sodium_chlorate_precursor": {"Sodium chlorate — NaClO₄ precursor (MVM network)", 18},
	"sebacic_acid_plasticizer":  {"Sebacic acid — HTPB plasticizer for solid propellant", 15},
	// ── Systèmes de guidage ───────────────────────────────────────────────
	"cnc_dual_use_guidance":   {"CNC machine — gyroscope/guidance mfg. (Honestar case)", 20},
	"fiber_optic_gyroscope":   {"Fiber optic gyroscope — missile guidance (FOG/INS)", 22},
	"inertial_navigation_unit": {"Inertial navigation unit (IMU/INS) — guidance dual-use", 20},
	"aluminum_alloy_mtcr":     {"Aluminum alloy — MTCR controlled (structures/tanks)", 10},
	"maraging_steel_dual_use": {"Maraging steel — MTCR Cat. 0 (centrifuges/warheads)", 22},
	// ── Composants drones ────────────────────────────────────────────────
	"mems_gyroscope_drone":      {"MEMS gyroscope / IMU — UAV autopilot sensor", 18},
	"drone_engine_piston":       {"UAV piston engine 50-100cc — MD550/Shahed clone", 15},
	"drone_lipo_battery":        {"LiPo battery high-density — UAV endurance", 12},
	"fpga_microcontroller_drone": {"FPGA / STM32 microcontroller — drone autopilot", 12},
	"ir_optical_sensor_drone":   {"IR optical sensor / camera — UAV guidance", 12},
	"carbon_fiber_airframe":     {"Carbon fiber — UAV airframe material", 10},
	"drone_electronics":         {"UAV electronics — Shahed drone component network", 15},
	// ── Programme nucléaire ───────────────────────────────────────────────
	"vacuum_pump_nuclear":        {"Turbomolecular vacuum pump — centrifuge cascade (NSG)", 18},
	"frequency_converter_nuclear": {"600 Hz frequency converter — centrifuge drive (NSG Cat. 0B001)", 20},
	"isotropic_graphite_nuclear": {"Isotropic graphite — neutron reflector (NSG controlled)", 15},
	"zirconium_nuclear":          {"Zirconium metal — fuel cladding (IAEA watchlist)", 18},
	"uf6_enrichment":             {"UF6 — uranium enrichment (IAEA notify required)", 30},
	"aluminum_trichloride_nuclear": {"Aluminum trichloride — uranium conversion", 12},
	// ── Défense aérienne ─────────────────────────────────────────────────
	"radar_air_defense":     {"Radar components — HQ-9 / air-defense system", 15},
	"roro_military_equipment": {"Ro-Ro heavy cargo — military equipment delivery profile", 12},
	// ── Destination ───────────────────────────────────────────────────────
	"chabahar_outside_hormuz":        {"Chabahar — outside Hormuz, active Mar 2026 route", 20},
	"bandar_abbas_destination":       {"Bandar Abbas — IRGC naval base", 15},
	"tehran_air_destination":         {"Tehran (IKA) — air freight destination via DXB", 18},
	"iran_destination":               {"Iranian destination port", 10},
	"iran_destination_outside_hormuz": {"Iranian port — outside Hormuz", 15},
	// ── AIS / Routes ──────────────────────────────────────────────────────
	"ais_dark_gulf_oman":        {"AIS blackout — Gulf of Oman chokepoint", 15},
	"ais_dark":                  {"AIS dark segments during voyage", 10},
	"pakistan_border_alt_route": {"Pakistan border closed — Chabahar only remaining route", 5},
	"flag_of_convenience":       {"Flag of convenience (FOC registry)", 10},
	"relay_waypoint":            {"Sea relay waypoint (MY / ID / SG)", 5},
	"hkg_air_hub":               {"HKG air hub — Dingtai/Yonghongan/Tianle transit (FDD 2024)", 8},
	"dubai_air_relay":           {"Dubai air relay — DXB → IKA sanctioned goods route", 10},
	"air_freight_mode":          {"Air freight — controlled goods, evades maritime inspection", 8},
	"express_courier_mode":      {"Express courier (DHL/FedEx) — drone component pattern", 12},
	"prior_iran_delivery":       {"Prior confirmed delivery to Iran", 5},
	"heavy_draft_delta":         {"Heavy load — draft delta > 3 m", 10},
	// ── Vérification inter-sources ────────────────────────────────────────
	"identity_discrepancy":   {"Identity discrepancy — name/flag/MMSI mismatch between sources", 15},
	"ais_position_spoofing":  {"AIS position spoofing — GPS gap > 50 km vs secondary source", 20},
	"unverifiable_vessel":    {"Vessel absent from all tracking databases (dark/spoofed IMO)", 10},
}

func expandCriteria(flags []string) []CriterionDetail {
	out := make([]CriterionDetail, 0, len(flags))
	for _, f := range flags {
		if meta, ok := criteriaLabels[f]; ok {
			out = append(out, CriterionDetail{Flag: f, Points: meta.Points, Label: meta.Label})
		} else {
			out = append(out, CriterionDetail{Flag: f, Points: 0, Label: strings.ReplaceAll(f, "_", " ")})
		}
	}
	return out
}
