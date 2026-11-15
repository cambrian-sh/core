package kernel

import (
	"strings"
	"testing"
)

// ENG-263/ENG-494: no agent may take a service principal's writer id.
func TestRefuseReservedAgentID(t *testing.T) {
	for _, id := range []string{"system:kernel", "source:telegram:bot1"} {
		if err := refuseReservedAgentID(id); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("%q: %v", id, err)
		}
	}
	if err := refuseReservedAgentID("kernel"); err != nil {
		t.Fatalf("a bare agent id stays allowed: %v", err)
	}
}
