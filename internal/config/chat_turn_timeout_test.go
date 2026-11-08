package config

import (
	"strings"
	"testing"
)

// The chat turn and stall bounds were constants in internal/chat (240 s / 90 s)
// that no operator could raise (T-20261107-chat-turn-timeout-is-hardcoded-at-240-s).
// They are execution.chat keys now, defaulted to those values, and the plan's
// caller-deadline reserve (T-20261107-a-delegated-plan-stopped-by-its-caller-s) is
// an execution.plan key. A v2 file and a CAMBRIAN_* env var both set them; there is
// no v1 spelling to migrate (the keys are new).
func TestChatTurnBoundsAndCallerReserve_DefaultsAndOverrides(t *testing.T) {
	cfg, _ := loadFrom(t, baseBundle(t), nil)
	if got := cfg.Execution.Chat.ChatTurnTimeoutSeconds; got != 240 {
		t.Errorf("chat_turn_timeout_seconds default = %d, want 240", got)
	}
	if got := cfg.Execution.Chat.ChatStallTimeoutSeconds; got != 90 {
		t.Errorf("chat_stall_timeout_seconds default = %d, want 90", got)
	}
	if got := cfg.Execution.Plan.CallerDeadlineReserveMs; got != DefaultCallerDeadlineReserveMs {
		t.Errorf("caller_deadline_reserve_ms default = %d, want %d", got, DefaultCallerDeadlineReserveMs)
	}

	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"schema_version": 2, "execution": {
		"chat": {"chat_turn_timeout_seconds": 600, "chat_stall_timeout_seconds": 120},
		"plan": {"caller_deadline_reserve_ms": 8000}}}`)
	cfg, _ = loadFrom(t, dir, nil)
	if cfg.Execution.Chat.ChatTurnTimeoutSeconds != 600 || cfg.Execution.Chat.ChatStallTimeoutSeconds != 120 ||
		cfg.Execution.Plan.CallerDeadlineReserveMs != 8000 {
		t.Errorf("file overrides not applied: chat=%+v reserve=%d", cfg.Execution.Chat, cfg.Execution.Plan.CallerDeadlineReserveMs)
	}
}

func TestChatTurnBoundsAndCallerReserve_RejectNegative(t *testing.T) {
	c := DefaultConfig().Execution
	c.Chat.ChatTurnTimeoutSeconds = -1
	c.Chat.ChatStallTimeoutSeconds = -1
	c.Plan.CallerDeadlineReserveMs = -1
	err := c.Validate()
	if err == nil {
		t.Fatal("negative bounds validated")
	}
	for _, k := range []string{"chat_turn_timeout_seconds", "chat_stall_timeout_seconds", "caller_deadline_reserve_ms"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("validation error does not name %s: %v", k, err)
		}
	}
}
