package postgres

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0145 D12: migration 0036 admits the "composition" kind (a worker composition
// recorded for a plan step), and the store round-trips it.
func TestPg_LedgerCompositionKindRoundTrips(t *testing.T) {
	st, _, ctx := newLedgerStore(t)
	e := pgEntry("led_pgled_composition", domain.LedgerComposition, "run-c", "turn-c", 1, "", `{"runtime":"composed_worker"}`, time.Now().Add(time.Hour))
	if err := st.Put(ctx, e); err != nil {
		t.Fatalf("put composition entry: %v", err)
	}
	got, err := st.Get(ctx, e.ID)
	if err != nil || got.Kind != domain.LedgerComposition {
		t.Fatalf("get = %+v, %v", got, err)
	}
}
