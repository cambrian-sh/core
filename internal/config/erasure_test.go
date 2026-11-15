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
