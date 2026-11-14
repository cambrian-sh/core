package aggregator

import (
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261113-four-consumer-interfaces-restate-domain-port: AggregatorProfileStore IS the domain
// port (an alias), not a restated copy of its method set that could drift
// (AGENTS.md §3.1).
func TestAggregatorProfileStoreIsTheDomainPort(t *testing.T) {
	if reflect.TypeOf((*AggregatorProfileStore)(nil)).Elem() != reflect.TypeOf((*domain.VerifierProfileStore)(nil)).Elem() {
		t.Fatal("AggregatorProfileStore is a separate interface type; alias domain.VerifierProfileStore instead")
	}
}
