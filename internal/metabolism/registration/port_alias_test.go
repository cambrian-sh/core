package registration

import (
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261113-four-consumer-interfaces-restate-domain-port: ProfileStore IS the domain
// port (an alias), not a restated copy of its method set that could drift
// (AGENTS.md §3.1).
func TestProfileStoreIsTheDomainPort(t *testing.T) {
	if reflect.TypeOf((*ProfileStore)(nil)).Elem() != reflect.TypeOf((*domain.VerifierProfileStore)(nil)).Elem() {
		t.Fatal("ProfileStore is a separate interface type; alias domain.VerifierProfileStore instead")
	}
}
