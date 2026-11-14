package signal

import (
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261113-four-consumer-interfaces-restate-domain-port: ManagerAccess IS the domain
// port (an alias), not a restated copy of its method set that could drift
// (AGENTS.md §3.1).
func TestManagerAccessIsTheDomainPort(t *testing.T) {
	if reflect.TypeOf((*ManagerAccess)(nil)).Elem() != reflect.TypeOf((*domain.AgentManager)(nil)).Elem() {
		t.Fatal("ManagerAccess is a separate interface type; alias domain.AgentManager instead")
	}
}
