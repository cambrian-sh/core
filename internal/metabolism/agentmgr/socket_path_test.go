package agentmgr

import (
	"strings"
	"testing"
)

// The bug: temp dir + agent id + a full UUID + ".sock" runs to 109 characters
// for "telegram_ingress_support". sockaddr_un allows 107, so the agent died
// during startup with a gRPC message about PORTS — and the default ingress
// happened to land on exactly 107 and worked, so the one everybody tests with
// was fine while every per-bot agent failed.
func TestSocketPath_StaysWithinTheSockaddrLimit(t *testing.T) {
	instance := "e1f87c26-2bca-4ae4-b1e9-427923e46c51"
	for _, agentID := range []string{
		"telegram_ingress_agent",
		"telegram_ingress_support",
		"telegram_ingress_customer_facing_escalations",
		strings.Repeat("a", 200),
		"x",
	} {
		got := socketPath(agentID, instance)
		if len(got) > maxSocketPathLen {
			t.Errorf("agent %q: socket path is %d characters, over the %d limit: %s",
				agentID, len(got), maxSocketPathLen, got)
		}
	}
}

// Truncation must not merge two agents onto one socket — they would fight over
// the same address and one would fail to bind, intermittently.
func TestSocketPath_LongSharedPrefixesDoNotCollide(t *testing.T) {
	instance := "e1f87c26-2bca-4ae4-b1e9-427923e46c51"
	long := strings.Repeat("telegram_ingress_", 6)
	a := socketPath(long+"one", instance)
	b := socketPath(long+"two", instance)
	if a == b {
		t.Fatalf("two agents share a socket path: %s", a)
	}
}

// Two instances of the SAME agent must not share a socket either.
func TestSocketPath_DistinctInstancesDistinctSockets(t *testing.T) {
	a := socketPath("telegram_ingress_support", "e1f87c26-2bca-4ae4-b1e9-427923e46c51")
	b := socketPath("telegram_ingress_support", "ffffffff-2bca-4ae4-b1e9-427923e46c51")
	if a == b {
		t.Fatalf("two instances share a socket path: %s", a)
	}
}

// A readable name is worth keeping when it fits: this path appears in logs and
// in `ss -x`, and a hash there costs real debugging time.
func TestSocketPath_KeepsTheAgentNameWhenItFits(t *testing.T) {
	got := socketPath("telegram_ingress_support", "e1f87c26-2bca-4ae4-b1e9-427923e46c51")
	if !strings.Contains(got, "telegram_ingress_support") {
		t.Fatalf("the agent name should survive when there is room: %s", got)
	}
}
