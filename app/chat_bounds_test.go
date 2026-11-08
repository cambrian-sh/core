package app

import (
	"testing"
	"time"

	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/config"
)

// The chat lane's turn and stall bounds come from execution.chat, and a zero keeps
// the package default (T-20261107-chat-turn-timeout-is-hardcoded-at-240-s).
func TestSecondsOr_ChatBounds(t *testing.T) {
	def := config.DefaultConfig().Execution.Chat
	if got := secondsOr(def.ChatTurnTimeoutSeconds, corechat.DefaultTurnTimeout); got != corechat.DefaultTurnTimeout {
		t.Errorf("default config turn timeout = %s, want the package default %s", got, corechat.DefaultTurnTimeout)
	}
	if got := secondsOr(def.ChatStallTimeoutSeconds, corechat.DefaultStallTimeout); got != corechat.DefaultStallTimeout {
		t.Errorf("default config stall timeout = %s, want the package default %s", got, corechat.DefaultStallTimeout)
	}
	if got := secondsOr(600, corechat.DefaultTurnTimeout); got != 600*time.Second {
		t.Errorf("secondsOr(600) = %s", got)
	}
	if got := secondsOr(0, corechat.DefaultTurnTimeout); got != corechat.DefaultTurnTimeout {
		t.Errorf("secondsOr(0) = %s, want the default", got)
	}
}
