package app

import (
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ruleOfTwoGate reads execution.tools.rule_of_two into the tool executor's
// RuleOfTwo control (owner decision 2026-11-19, ENG-531,
// T-20261104-no-content-provenance-reaches-tool): a write/egress call from a
// session that has consumed untrusted content and holds sensitive access goes to
// approval through the one approval path. Off ⇒ nil. The gate owns the
// per-session provenance tracker the transport reports recalls and tool results
// to (ToolExecutor.ObserveContent).
func ruleOfTwoGate(cfg config.RuleOfTwoConfig) (*domain.RuleOfTwoGate, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	ttl := time.Duration(cfg.TaintTTLMinutes) * time.Minute
	return domain.NewRuleOfTwoGate(domain.RuleOfTwoPolicy{
		UntrustedSources: cfg.Sources(),
		SensitiveTags:    cfg.SensitiveTags,
		GatedEffects:     cfg.Effects(),
		// ENG-2247: the dedicated kernel write calls (memory notes, schedules,
		// skill/knowledge/ontology proposals, forget, agent ingest) are decided
		// by this same gate, each under its configured effects.
		KernelWriteEffects: cfg.VerbEffects(),
	}, domain.NewProvenanceTracker(ttl, nil))
}
