package config

import (
	"strings"
	"testing"
	"time"
)

// ADR-0137 D10: execution.erasure defaults off (operator-only when on), with the
// documented limits, and negative values fail validation.
func TestErasureConfig_DefaultsAndValidation(t *testing.T) {
	c := DefaultConfig().Execution.Erasure
	if c.Enabled || c.AgentEraseEnabled {
		t.Fatalf("erasure must default off and operator-only: %+v", c)
	}
	if c.MaxTargets() != 1000 || c.AsyncDeadline() != time.Hour || c.TombstoneRetention() != 0 ||
		c.WorkerPoll() != 30*time.Second || c.WorkerAttempts() != 5 {
		t.Fatalf("defaults: max=%d deadline=%v retention=%v poll=%v attempts=%d",
			c.MaxTargets(), c.AsyncDeadline(), c.TombstoneRetention(), c.WorkerPoll(), c.WorkerAttempts())
	}
	set := ErasureConfig{MaxTargetsPerRequest: 5, AsyncDeadlineSeconds: 60, TombstoneRetentionDays: 2, WorkerPollSeconds: 3, WorkerMaxAttempts: 7}
	if set.MaxTargets() != 5 || set.AsyncDeadline() != time.Minute || set.TombstoneRetention() != 48*time.Hour ||
		set.WorkerPoll() != 3*time.Second || set.WorkerAttempts() != 7 {
		t.Fatalf("set values not honoured: %+v", set)
	}
	errs := ErasureConfig{MaxTargetsPerRequest: -1, WorkerMaxAttempts: -2}.validate()
	if len(errs) != 2 || !strings.Contains(strings.Join(errs, ";"), "execution.erasure.max_targets_per_request") {
		t.Fatalf("validate = %v", errs)
	}
	cfg := DefaultConfig()
	cfg.Execution.Erasure.AsyncDeadlineSeconds = -1
	if err := cfg.Execution.Validate(); err == nil || !strings.Contains(err.Error(), "async_deadline_seconds") {
		t.Fatalf("ExecutionConfig.Validate must report the erasure block: %v", err)
	}
}

// ADR-0137 D2 (stage C): a forget preview is executable for preview_ttl_seconds,
// 600 by default; a negative value fails validation.
func TestErasureConfig_PreviewTTL(t *testing.T) {
	if got := DefaultConfig().Execution.Erasure.PreviewTTL(); got != 10*time.Minute {
		t.Fatalf("default preview TTL = %v, want 10m", got)
	}
	if got := (ErasureConfig{PreviewTTLSeconds: 30}).PreviewTTL(); got != 30*time.Second {
		t.Fatalf("set preview TTL = %v, want 30s", got)
	}
	if errs := (ErasureConfig{PreviewTTLSeconds: -1}).validate(); len(errs) != 1 || !strings.Contains(errs[0], "preview_ttl_seconds") {
		t.Fatalf("validate = %v", errs)
	}
}

// ENG-1948: an agent query preview clips snippets to preview_snippet_chars, 200 by
// default; a negative value fails validation.
func TestErasureConfig_PreviewSnippet(t *testing.T) {
	if got := DefaultConfig().Execution.Erasure.PreviewSnippet(); got != 200 {
		t.Fatalf("default snippet = %d, want 200", got)
	}
	if got := (ErasureConfig{PreviewSnippetChars: 40}).PreviewSnippet(); got != 40 {
		t.Fatalf("set snippet = %d, want 40", got)
	}
	if errs := (ErasureConfig{PreviewSnippetChars: -1}).validate(); len(errs) != 1 || !strings.Contains(errs[0], "preview_snippet_chars") {
		t.Fatalf("validate = %v", errs)
	}
}
