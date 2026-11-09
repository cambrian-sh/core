package network

import (
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// stepCacheVocabulary is the effect vocabulary the DAG executor's step cache reads
// to decide which steps observe only (ADR-0145 D9, executer/step_cache_guard.go).
// The vocabulary is validated at config load, so an error here means a config that
// bypassed validation; nil then makes every action step uncacheable (fail closed).
func stepCacheVocabulary(c config.CapabilityConfig) *domain.StepEffectVocabulary {
	v, err := c.StepEffectVocabulary()
	if err != nil {
		slog.Warn("step cache: effect vocabulary invalid, caching thought steps only", "err", err)
		return nil
	}
	return v
}

// stepCacheDefaultTTL resolves execution.plan.step_cache_default_ttl_minutes; 0 ⇒ 60.
func stepCacheDefaultTTL(p config.PlanConfig) time.Duration {
	if p.StepCacheDefaultTTLMinutes <= 0 {
		return time.Hour
	}
	return time.Duration(p.StepCacheDefaultTTLMinutes) * time.Minute
}
