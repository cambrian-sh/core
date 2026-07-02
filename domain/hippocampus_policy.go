package domain

// HippocampusPolicy defines the retrieval thresholds for a named plan category (ADR-0027).
// Operators configure these values in Koanf config; the Hippocampus looks them up at
// retrieval time so no code deployment is needed to tune thresholds.
type HippocampusPolicy struct {
	SimilarityThreshold float64 `json:"similarity"`
	ConfidenceFloor     float64 `json:"confidence"`
	MaxAgeHours         int     `json:"max_age_hours"`
}

// PolicyProvider is the port for resolving a retrieval policy by name.
// Implementations may read from static config (StaticPolicyProvider) or a
// future dynamic store without changing the Hippocampus.
type PolicyProvider interface {
	GetPolicy(name string) (HippocampusPolicy, bool)
	DefaultPolicy() HippocampusPolicy
}
