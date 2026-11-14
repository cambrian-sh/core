package domain

import (
	"math"
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
// λ comes from the caller: SearchOptions.DecayLambda in the adapter's re-rank
// (pgvector_adapter.go) and the memory re-ranker (internal/memory/rerank.go). A
// per-document-type DecayConfig and a second domain-level re-ranker
// (ReRankWithTemporalDecay) once lived here with no production caller; they were
// deleted 2026-11-14 (T-20261113-deadcode-cmd-finds-60-unreachable-core).
func TemporalDecay(baseStrength float64, lastAccessed time.Time, lambda float64, now time.Time) float64 {
	age := now.Sub(lastAccessed).Hours()
	if age < 0 {
		age = 0
	}
	return baseStrength * math.Exp(-lambda*age)
}
