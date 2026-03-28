package services

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/proptrack/proptrack/internal/models"
)

// AlertLevel maps score thresholds to named alert levels.
type AlertLevel string

const (
	AlertHigh     AlertLevel = "HIGH"
	AlertCritical AlertLevel = "CRITICAL"
)

// Alert represents a generated intelligence alert for a suspicious shipment.
type Alert struct {
	ID           string                 `json:"id"`
	GeneratedAt  time.Time              `json:"generated_at"`
	Level        AlertLevel             `json:"level"`
	ShipmentID   string                 `json:"shipment_id"`
	VesselIMO    string                 `json:"vessel_imo"`
	VesselName   string                 `json:"vessel_name"`
	Route        string                 `json:"route"`
	Score        int                    `json:"score"`
	Flags        []string               `json:"flags"`
	Summary      string                 `json:"summary"`
	Recommended  string                 `json:"recommended_action"`
}

// GenerateAlertIfWarranted creates an Alert when a shipment's score crosses
// the HIGH (56+) or CRITICAL (76+) threshold. Returns nil if score is below threshold.
func GenerateAlertIfWarranted(ctx context.Context, shipment models.Shipment, result models.SuspicionResult) *Alert {
	if result.TotalScore < 56 {
		return nil
	}

	level := AlertHigh
	if result.TotalScore >= 76 {
		level = AlertCritical
	}

	vesselIMO, vesselName := "", ""
	if shipment.Vessel != nil {
		vesselIMO = shipment.Vessel.IMO
		vesselName = shipment.Vessel.Name
	}
	originName, destName := "unknown", "unknown"
	if shipment.OriginPort != nil {
		originName = shipment.OriginPort.Name
	}
	if shipment.DestinationPort != nil {
		destName = shipment.DestinationPort.Name
	}

	recommended := recommendedAction(level, result.TriggeredFlags)

	alert := &Alert{
		ID:          fmt.Sprintf("ALT-%s-%d", shipment.ID, time.Now().Unix()),
		GeneratedAt: time.Now().UTC(),
		Level:       level,
		ShipmentID:  shipment.ID,
		VesselIMO:   vesselIMO,
		VesselName:  vesselName,
		Route:       fmt.Sprintf("%s → %s", originName, destName),
		Score:       result.TotalScore,
		Flags:       result.TriggeredFlags,
		Summary:     result.Summary,
		Recommended: recommended,
	}

	slog.Warn("alert generated",
		"level", string(level),
		"shipment_id", shipment.ID,
		"vessel_imo", vesselIMO,
		"score", result.TotalScore,
		"flags", strings.Join(result.TriggeredFlags, ","),
	)

	return alert
}

func recommendedAction(level AlertLevel, flags []string) string {
	hasFlag := func(f string) bool {
		for _, fl := range flags {
			if fl == f {
				return true
			}
		}
		return false
	}

	actions := []string{}

	if level == AlertCritical {
		actions = append(actions, "File immediate intelligence report to counter-proliferation desk.")
	}

	if hasFlag("sanctioned_vessel") || hasFlag("irisl_operator") {
		actions = append(actions, "Cross-reference OFAC SDN list and notify Treasury liaison.")
	}
	if hasFlag("chemical_hub_origin") || hasFlag("high_risk_cargo") {
		actions = append(actions, "Request manifest data via port state control authorities at origin.")
	}
	if hasFlag("ais_dark") {
		actions = append(actions, "Task satellite AIS coverage or Sentinel-1 SAR for vessel location.")
	}
	if hasFlag("iran_destination") {
		actions = append(actions, "Notify partner agencies with Iran sanctions coverage (OFAC, OFSI, EU RELEX).")
	}
	if hasFlag("relay_waypoint") {
		actions = append(actions, "Request inspection at next port of call via PSC MOU network.")
	}

	if len(actions) == 0 {
		actions = append(actions, "Assign to analyst for enhanced monitoring.")
	}

	return strings.Join(actions, " ")
}
