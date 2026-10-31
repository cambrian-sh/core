package chat

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package when any test leaves a goroutine behind
// (AGENTS.md §3.3: every background worker package carries a goleak TestMain).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
