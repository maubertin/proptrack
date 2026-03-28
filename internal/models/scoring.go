package models

// ScoreThreshold labels the risk tier derived from a numeric score.
type ScoreThreshold string

const (
	ThresholdLow      ScoreThreshold = "low"      // 0–30   (uncapped raw score)
	ThresholdMedium   ScoreThreshold = "medium"   // 31–55
	ThresholdHigh     ScoreThreshold = "high"      // 56–75
	ThresholdCritical ScoreThreshold = "critical" // ≥76
)

// SuspicionResult is returned by the scoring engine.
type SuspicionResult struct {
	TotalScore     int            `json:"total_score"`
	Threshold      ScoreThreshold `json:"threshold"`
	TriggeredFlags []string       `json:"triggered_flags"`
	Summary        string         `json:"summary"`
}

// ScoreBreakdownItem holds a single scoring criterion result.
type ScoreBreakdownItem struct {
	Flag    string `json:"flag"`
	Points  int    `json:"points"`
	Matched bool   `json:"matched"`
	Reason  string `json:"reason"`
}
