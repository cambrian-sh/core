package app

import (
	"testing"
	"time"

	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/config"
)

// ENG-2146: execution.chat.restart_notice_window_seconds reaches the chat lane: 0 is the
// package default, a positive value is used as given, -1 turns ingress delivery of the
// boot interruption notice off; validation refuses anything below -1.
func TestRestartNoticeWindow_ConfigReachesTheChatLane(t *testing.T) {
	for secs, want := range map[int]time.Duration{
		0:   corechat.DefaultRestartNoticeWindow,
		120: 2 * time.Minute,
		-1:  0,
	} {
		if got := restartNoticeWindow(secs); got != want {
			t.Errorf("restartNoticeWindow(%d) = %v, want %v", secs, got, want)
		}
	}
	cfg := config.DefaultConfig()
	cfg.Execution.Chat.RestartNoticeWindowSeconds = -2
	if err := cfg.Execution.Validate(); err == nil {
		t.Fatal("restart_notice_window_seconds = -2 passed validation")
	}
	cfg.Execution.Chat.RestartNoticeWindowSeconds = -1
	if err := cfg.Execution.Validate(); err != nil {
		t.Fatalf("-1 (off) refused: %v", err)
	}
}
