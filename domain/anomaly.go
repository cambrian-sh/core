package domain

type AnomalyReport struct {
	IsAnomaly  bool
	Reason     string
	Confidence float64
	Severity   string // Low, Medium, High, Critical
}
