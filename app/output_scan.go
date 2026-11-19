package app

import (
	"fmt"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/contentscan"
)

// contentScanner adapts the one shared deterministic scanner (internal/contentscan,
// also used for skills and memory notes) to the domain.ContentScanner port.
// Findings carry the rule, class, severity and line, never the matched excerpt:
// they travel beside the fence, so they must hold nothing the source wrote.
type contentScanner struct{ s *contentscan.Scanner }

func (c contentScanner) ScanContent(text string) domain.ContentScan {
	r := c.s.Scan(text)
	out := domain.ContentScan{Verdict: domain.ContentScanVerdict(r.Verdict)}
	for _, f := range r.Findings {
		out.Findings = append(out.Findings, fmt.Sprintf("%s %s/%s line %d", f.Rule, f.Class, f.Severity, f.Line))
	}
	return out
}

// untrustedOutputGuard reads execution.tools.output_scan into the tool executor's
// UntrustedOutput control (ADR-0043 D4; owner decision 2026-11-19, ENG-117). Off
// ⇒ nil: MCP responses are still fenced, marked not_scanned. A rule set that does
// not build (unknown disabled rule, bad pattern) fails the boot.
func untrustedOutputGuard(cfg config.ToolOutputScanConfig) (*domain.UntrustedOutputGuard, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	extra := make([]contentscan.RuleSpec, 0, len(cfg.ExtraRules))
	for _, r := range cfg.ExtraRules {
		extra = append(extra, contentscan.RuleSpec{ID: r.ID, Class: r.Class, Severity: r.Severity, Pattern: r.Pattern})
	}
	s, err := contentscan.New(contentscan.Config{DisabledRules: cfg.DisabledRules, ExtraRules: extra})
	if err != nil {
		return nil, fmt.Errorf("execution.tools.output_scan: %w", err)
	}
	return domain.NewUntrustedOutputGuard(contentScanner{s},
		domain.ContentScanVerdict(cfg.HitAtOrDefault()), domain.UntrustedHitAction(cfg.OnHitOrDefault()))
}
