package app

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
)

// The chat turn's person-wait allowance is the longest window one blocking
// approval can wait (T-20261118-a-chat-turn-waiting-on-a-human-approval-is):
// derived from the settings that own those windows, never a tunable of its own.
func TestPersonWaitAllowance_IsTheLongestApprovalWindow(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Approvals.DefaultTimeoutSeconds = 60
	cfg.Execution.Schedules.ApprovalTimeoutSeconds = 300
	cfg.Execution.Skills.AccessRequestTimeoutSeconds = 120
	cfg.MCP.WorkerConsentTimeoutMs = 0
	if got := personWaitAllowance(cfg); got != 300*time.Second {
		t.Fatalf("allowance %s, want the 300 s schedule window", got)
	}
	cfg.Approvals.DefaultTimeoutSeconds = 900
	if got := personWaitAllowance(cfg); got != 900*time.Second {
		t.Fatalf("allowance %s, want the 900 s approvals default", got)
	}
	cfg.MCP.WorkerConsentTimeoutMs = 1_200_000
	if got := personWaitAllowance(cfg); got != 20*time.Minute {
		t.Fatalf("allowance %s, want the 20 min consent window", got)
	}
}
