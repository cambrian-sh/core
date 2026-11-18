package ingress

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package when any test leaves a goroutine behind
// (AGENTS.md §3.3: every background worker package carries a goleak TestMain;
// this package runs the ConsentBridge render loop and the delivery redriver).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
