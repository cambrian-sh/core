package llm

import (
	"context"
	"net/http"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-453 (owner decision 2026-11-14): every result records the model that
// answered. The spend meter counts calls per generator id the Provider SERVED the
// call on — after any fallback — on the session total and the per-request tally.
func TestSpendMeter_RecordsTheAnsweringGenerator(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusTooManyRequests, quota429)
	b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
	p := policyProvider(t, a, b, false, [][]string{{"a", "b"}})
	meter := NewSpendMeter(0, p.Ledger(), nil)
	p.SetSpendMeter(meter)

	ctx := domain.WithSessionID(context.Background(), "ses-m")
	ctx, tally := domain.WithLLMUsageTally(ctx)
	base := meter.Session("ses-m")
	if out, err := p.GeneratorFor(domain.PurposePlanner).Generate(ctx, "plan"); err != nil || out != "from b" {
		t.Fatalf("Generate = %q, %v", out, err)
	}
	got := meter.Session("ses-m").Sub(base)
	if len(got.Models) != 1 || got.Models["b"] != 1 {
		t.Fatalf("Models = %v, want b=1 (b answered after a's 429)", got.Models)
	}
	if g := tally.ByGenerator(); len(g) != 1 || g["b"] != 1 {
		t.Fatalf("tally generators = %v, want b=1", g)
	}

	// A snapshot is a copy: later calls do not change it.
	snap := meter.Session("ses-m")
	if _, err := p.GeneratorFor(domain.PurposePlanner).Generate(ctx, "plan"); err != nil {
		t.Fatal(err)
	}
	if snap.Models["b"] != 1 {
		t.Fatalf("snapshot mutated by a later call: %v", snap.Models)
	}
	if d := meter.Session("ses-m").Sub(snap); d.Models["b"] != 1 || len(d.Models) != 1 {
		t.Fatalf("delta = %v, want b=1", d.Models)
	}
}
