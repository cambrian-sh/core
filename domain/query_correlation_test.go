package domain

import (
	"context"
	"sync"
	"testing"
)

func TestQueryCorrelation_AbsentIsTheOrdinaryCase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if id, ok := QueryCorrelationFromContext(ctx); ok || id != "" {
		t.Errorf("bare context reported a correlation %q", id)
	}
	if id, ok := NextQueryCorrelationID(ctx); ok || id != "" {
		t.Errorf("bare context handed out a hop id %q; the caller must be told to mint its own", id)
	}
	// An empty handle is a no-op rather than a stored empty string, so a transport
	// with nothing to offer does not have to branch.
	if _, ok := QueryCorrelationFromContext(WithQueryCorrelation(ctx, "")); ok {
		t.Error("an empty correlation was installed")
	}
}

// THE property: hops EXTEND the prefix. A 1:1 id would put hop 2 on hop 1's
// handle, and the chain would read incomplete without saying so.
func TestNextQueryCorrelationID_HopsExtendThePrefix(t *testing.T) {
	t.Parallel()
	ctx := WithQueryCorrelation(context.Background(), "mcp-7f")

	if got, _ := QueryCorrelationFromContext(ctx); got != "mcp-7f" {
		t.Errorf("prefix = %q, want the seeded handle unchanged", got)
	}
	for _, want := range []string{"mcp-7f-h1", "mcp-7f-h2", "mcp-7f-h3"} {
		got, ok := NextQueryCorrelationID(ctx)
		if !ok || got != want {
			t.Fatalf("hop id = %q (ok=%v), want %q", got, ok, want)
		}
	}
	// Reading the prefix must not consume a hop.
	if got, _ := QueryCorrelationFromContext(ctx); got != "mcp-7f" {
		t.Errorf("prefix = %q after three hops", got)
	}
}

// Two calls that (legally) chose the same handle number their own hops from 1
// rather than continuing each other's.
func TestWithQueryCorrelation_InstallsAFreshCounter(t *testing.T) {
	t.Parallel()
	first := WithQueryCorrelation(context.Background(), "same")
	if _, _ = NextQueryCorrelationID(first); true {
		_, _ = NextQueryCorrelationID(first)
	}
	second := WithQueryCorrelation(context.Background(), "same")
	if got, _ := NextQueryCorrelationID(second); got != "same-h1" {
		t.Errorf("second call's first hop = %q, want same-h1", got)
	}
}

// A child context inherits the SAME counter: hops descend through the call tree,
// and two of them must never claim one number.
func TestNextQueryCorrelationID_SharedAcrossDerivedContexts(t *testing.T) {
	t.Parallel()
	root := WithQueryCorrelation(context.Background(), "c")
	child, cancel := context.WithCancel(root)
	defer cancel()

	a, _ := NextQueryCorrelationID(root)
	b, _ := NextQueryCorrelationID(child)
	if a == b {
		t.Fatalf("a derived context restarted the hop count (both %q)", a)
	}
}

// Nothing forbids a future loop from retrieving two hops concurrently, so the
// counter is atomic and the -race build proves it.
func TestNextQueryCorrelationID_ConcurrentHopsAreDistinct(t *testing.T) {
	t.Parallel()
	ctx := WithQueryCorrelation(context.Background(), "c")
	const n = 32
	ids := make([]string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ids[i], _ = NextQueryCorrelationID(ctx)
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("hop id %q was handed out twice", id)
		}
		seen[id] = true
	}
}

// ENG-2068: the derived handle is a pure function of the request (plan step or
// session, principal, question) — same input, same handle; any part changed, a
// different one — and it never replaces a boundary's handle.
func TestDerivedQueryCorrelation_IsDeterministicPerRequest(t *testing.T) {
	t.Parallel()
	bg := context.Background()
	base := DerivedQueryCorrelation(bg, "p", "q")
	if base != DerivedQueryCorrelation(bg, "p", "q") || len(base) != 34 || base[:2] != "q-" {
		t.Fatalf("derived handle %q is not stable / q-<32 hex>", base)
	}
	sess := WithSessionID(bg, "s1")
	step := WithStepCall(sess, StepCall{RunID: "r1", StepIndex: 0})
	seen := map[string]string{"bare": base}
	for name, got := range map[string]string{
		"principal":   DerivedQueryCorrelation(bg, "p2", "q"),
		"question":    DerivedQueryCorrelation(bg, "p", "q2"),
		"session":     DerivedQueryCorrelation(sess, "p", "q"),
		"other sess":  DerivedQueryCorrelation(WithSessionID(bg, "s2"), "p", "q"),
		"plan step":   DerivedQueryCorrelation(step, "p", "q"),
		"other step":  DerivedQueryCorrelation(WithStepCall(sess, StepCall{RunID: "r1", StepIndex: 1}), "p", "q"),
		"split parts": DerivedQueryCorrelation(bg, "pq", ""),
	} {
		for other, prev := range seen {
			if got == prev {
				t.Errorf("%s collides with %s: %q", name, other, got)
			}
		}
		seen[name] = got
	}
	if DerivedQueryCorrelation(sess, "p", "q") != DerivedQueryCorrelation(WithSessionID(bg, "s1"), "p", "q") {
		t.Error("same session, principal and question must derive the same handle")
	}
}

func TestEnsureQueryCorrelation_KeepsTheBoundaryHandleAndNumbersHops(t *testing.T) {
	t.Parallel()
	seeded := WithQueryCorrelation(context.Background(), "mcp-1")
	if got, _ := QueryCorrelationFromContext(EnsureQueryCorrelation(seeded, "p", "q")); got != "mcp-1" {
		t.Fatalf("the boundary's handle must win, got %q", got)
	}
	ctx := EnsureQueryCorrelation(context.Background(), "p", "q")
	want := DerivedQueryCorrelation(context.Background(), "p", "q")
	h1, _ := NextQueryCorrelationID(ctx)
	h2, _ := NextQueryCorrelationID(ctx)
	if h1 != want+"-h1" || h2 != want+"-h2" {
		t.Fatalf("hops %q %q, want %s-h1/-h2", h1, h2, want)
	}
	// A retry is a fresh call: its hops start at 1 again, so it names the same ids.
	if r1, _ := NextQueryCorrelationID(EnsureQueryCorrelation(context.Background(), "p", "q")); r1 != h1 {
		t.Fatalf("retry hop 1 = %q, want %q", r1, h1)
	}
}
