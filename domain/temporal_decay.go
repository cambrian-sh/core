package domain

import (
	"math"
	"sort"
	"time"
)

// TemporalDecay computes the effective activation strength of a document at
// query time. It is a pure function with no side effects.
//
//	effective = baseStrength × e^(-λ × age_hours)
//
// where age_hours = max(0, now - lastAccessed) in hours.
// A future lastAccessed is clamped to age 0 (no negative decay).
//
// Default λ per document type (from DecayConfig):
//   - MnemonicFact:  0.005 /hour (slow decay)
//   - MnemonicScene: 0.02  /hour (fast decay)
//   - AgentProfile:  0.001 /hour (very slow)
//   - NegativeEdge:  0.05  /hour (fast)
func TemporalDecay(baseStrength float64, lastAccessed time.Time, lambda float64, now time.Time) float64 {
	age := now.Sub(lastAccessed).Hours()
	if age < 0 {
		age = 0
	}
	return baseStrength * math.Exp(-lambda*age)
}

// DecayConfig holds per-document-type decay rate constants (λ in e^(-λt)).
// Configurable per deployment; defaults represent editorial judgement, not
// calibrated constants.
type DecayConfig struct {
	MnemonicFactLambda  float64 // default 0.005 /hour
	MnemonicSceneLambda float64 // default 0.02  /hour
	AgentProfileLambda  float64 // default 0.001 /hour
	NegativeEdgeLambda  float64 // default 0.05  /hour
	DefaultLambda       float64 // fallback for unknown document types; default 0.01 /hour
}

// DefaultDecayConfig returns the editorial defaults for temporal decay rates.
func DefaultDecayConfig() DecayConfig {
	return DecayConfig{
		MnemonicFactLambda:  0.005,
		MnemonicSceneLambda: 0.02,
		AgentProfileLambda:  0.001,
		NegativeEdgeLambda:  0.05,
		DefaultLambda:       0.01,
	}
}

// LambdaFor returns the appropriate decay rate for the given document type.
func (c DecayConfig) LambdaFor(docType string) float64 {
	switch docType {
	case DocTypeMnemonicFact:
		return c.MnemonicFactLambda
	case DocTypeMnemonicScene:
		return c.MnemonicSceneLambda
	case DocTypeAgentProfile:
		return c.AgentProfileLambda
	case DocTypeNegativeEdge:
		return c.NegativeEdgeLambda
	default:
		return c.DefaultLambda
	}
}

// ReRankWithTemporalDecay applies query-time temporal decay to a candidate set
// returned by pgvector ANN and re-ranks by:
//
//	score × (floorAlpha + (1-floorAlpha) × effectiveActivation)
//
// where effectiveActivation = TemporalDecay(base, lastAccessed, lambda, now).
// floorAlpha=0.2 matches the existing floor-multiplier contract (ADR-0015).
//
// The input slice is NOT modified; a sorted copy is returned.
func ReRankWithTemporalDecay(candidates []SearchResult, lambda float64, now time.Time) []SearchResult {
	const floorAlpha = 0.2

	out := make([]SearchResult, len(candidates))
	copy(out, candidates)

	for i := range out {
		effective := TemporalDecay(out[i].Document.ActivationStrength, out[i].Document.LastAccessedAt, lambda, now)
		out[i].Score = out[i].Score * (floorAlpha + (1-floorAlpha)*effective)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out
}
