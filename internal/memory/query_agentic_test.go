package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// fakePlanner is a test Planner for the loop helpers.
type fakePlanner struct {
	rewrite        string
	planErr        error
	stop           bool
	bridge         string
	decErr         error
	synthStatus    string
	synthText      string
	synthErr       error
	planCalls      int
	decCalls       int
	synthCalls     int
	lastScratchpad string
}

func (f *fakePlanner) PlanQuery(_ context.Context, _ string, scratchpad string, _ []string, _ int) (string, error) {
	f.planCalls++
	f.lastScratchpad = scratchpad
	if f.planErr != nil {
		return "", f.planErr
	}
	return f.rewrite, nil
}

func (f *fakePlanner) DecideContinue(_ context.Context, _ string, _ []string, _ []string) (bool, string, error) {
	f.decCalls++
	if f.decErr != nil {
		return true, "", f.decErr
	}
	return f.stop, f.bridge, nil
}

func (f *fakePlanner) Synthesize(_ context.Context, _ string, _ []string) (string, string, error) {
	f.synthCalls++
	if f.synthErr != nil {
		return "answer", "", f.synthErr
	}
	if f.synthStatus == "" {
		return "answer", f.synthText, nil
	}
	return f.synthStatus, f.synthText, nil
}

func doc(id, text string) domain.SearchResult {
	return domain.SearchResult{Document: domain.Document{ID: id, Text: text}}
}

// --- planQuery --------------------------------------------------------------

func TestPlanQuery_DisabledIsIdentity(t *testing.T) {
	q := &QueryService{}
	if got := q.planQuery(context.Background(), "original", "", nil, 0); got != "original" {
		t.Fatalf("disabled planQuery should be identity, got %q", got)
	}
}

func TestPlanQuery_NilPlannerFailsOpen(t *testing.T) {
	q := &QueryService{}
	q.EnableAgenticRetrieval(nil, 1)
	if got := q.planQuery(context.Background(), "original", "", nil, 0); got != "original" {
		t.Fatalf("nil planner should fail open to original, got %q", got)
	}
}

func TestPlanQuery_RewritesWhenEnabled(t *testing.T) {
	p := &fakePlanner{rewrite: `"Titan migration" bob chen`}
	q := &QueryService{}
	q.EnableAgenticRetrieval(p, 1)
	got := q.planQuery(context.Background(), "what did Alice's manager approve?", "", nil, 0)
	if got != `"Titan migration" bob chen` {
		t.Fatalf("expected rewrite, got %q", got)
	}
}

func TestPlanQuery_ErrorFailsOpenToScratchpad(t *testing.T) {
	// On a later hop (scratchpad set), a planner error falls back to the BRIDGE,
	// not the raw query — so hop-2 still searches for the bridge.
	p := &fakePlanner{planErr: errors.New("dispatch failed")}
	q := &QueryService{}
	q.EnableAgenticRetrieval(p, 2)
	if got := q.planQuery(context.Background(), "orig question", "Bob Chen", nil, 0); got != "Bob Chen" {
		t.Fatalf("hop error should fall back to bridge, got %q", got)
	}
}

func TestPlanQuery_EmptyRewriteFallsBackToScratchpad(t *testing.T) {
	p := &fakePlanner{rewrite: "   "}
	q := &QueryService{}
	q.EnableAgenticRetrieval(p, 2)
	if got := q.planQuery(context.Background(), "orig", "Bob Chen", nil, 0); got != "Bob Chen" {
		t.Fatalf("empty rewrite should fall back to bridge, got %q", got)
	}
}

// --- decideContinue ---------------------------------------------------------

func TestDecideContinue_NilPlannerStops(t *testing.T) {
	q := &QueryService{}
	if stop, _ := q.decideContinue(context.Background(), "q", nil, nil); !stop {
		t.Fatal("nil planner should stop")
	}
}

func TestDecideContinue_ErrorStops(t *testing.T) {
	p := &fakePlanner{decErr: errors.New("boom")}
	q := &QueryService{}
	q.EnableAgenticRetrieval(p, 2)
	if stop, _ := q.decideContinue(context.Background(), "q", nil, []domain.SearchResult{doc("a", "x")}); !stop {
		t.Fatal("decide error should fail open to stop")
	}
}

func TestDecideContinue_ReturnsBridge(t *testing.T) {
	p := &fakePlanner{stop: false, bridge: "Bob Chen"}
	q := &QueryService{}
	q.EnableAgenticRetrieval(p, 2)
	stop, bridge := q.decideContinue(context.Background(), "q", nil, []domain.SearchResult{doc("a", "Alice reports to Bob Chen")})
	if stop || bridge != "Bob Chen" {
		t.Fatalf("expected continue+bridge, got stop=%v bridge=%q", stop, bridge)
	}
}

// --- interleaveDedup --------------------------------------------------------

func TestInterleaveDedup_SingleHopUnchanged(t *testing.T) {
	hop := []domain.SearchResult{doc("a", ""), doc("b", "")}
	got := interleaveDedup([][]domain.SearchResult{hop})
	if len(got) != 2 || got[0].Document.ID != "a" {
		t.Fatalf("single hop should pass through, got %+v", resultIDs(got))
	}
}

func TestInterleaveDedup_RoundRobinAndDedup(t *testing.T) {
	hop1 := []domain.SearchResult{doc("seed", ""), doc("shared", "")}
	hop2 := []domain.SearchResult{doc("answer", ""), doc("shared", "")}
	got := resultIDs(interleaveDedup([][]domain.SearchResult{hop1, hop2}))
	// position 0 = hop1 rank1 (seed), position 1 = hop2 rank1 (answer) → both
	// survive top-k; "shared" appears once.
	want := []string{"seed", "answer", "shared"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order mismatch: got %v want %v", got, want)
		}
	}
}

// --- finalizeAgentic (typed status control result) --------------------------

func TestFinalizeAgentic_PrependsControlWithStatus(t *testing.T) {
	p := &fakePlanner{synthStatus: "abstention", synthText: "not found in memory"}
	q := &QueryService{}
	q.EnableAgenticRetrieval(p, 1)
	acc := []domain.SearchResult{doc("chunk1", "some text")}
	hops := []hopTrace{{Hop: 0, PlannedQuery: "q", Decision: "max_hops"}}
	got := q.finalizeAgentic(context.Background(), "q", acc, hops, nil)
	if len(got) != 2 {
		t.Fatalf("expected control + 1 chunk, got %d", len(got))
	}
	ctl := got[0]
	if ctl.Document.ID != AgenticControlID {
		t.Fatalf("result[0] should be the control, got id %q", ctl.Document.ID)
	}
	if ctl.Document.Metadata[AgenticStatusKey] != "abstention" {
		t.Fatalf("control status = %v, want abstention", ctl.Document.Metadata[AgenticStatusKey])
	}
	tr, ok := ctl.Document.Metadata[AgenticTraceKey].(agenticTrace)
	if !ok || len(tr.Hops) != 1 || tr.FinalStatus != "abstention" {
		t.Fatalf("control should carry the loop trace, got %v", ctl.Document.Metadata[AgenticTraceKey])
	}
	if got[1].Document.ID != "chunk1" {
		t.Fatal("original chunk should follow the control")
	}
}

func TestFinalizeAgentic_SynthErrorFailsOpenButKeepsTrace(t *testing.T) {
	p := &fakePlanner{synthErr: errors.New("boom")}
	q := &QueryService{}
	q.EnableAgenticRetrieval(p, 1)
	acc := []domain.SearchResult{doc("chunk1", "x")}
	hops := []hopTrace{{Hop: 0, PlannedQuery: "q", Decision: "max_hops"}}
	got := q.finalizeAgentic(context.Background(), "q", acc, hops, nil)
	// Synth error still emits a control (status "answer", empty text) so the
	// loop trace is preserved; the original chunk follows it.
	if len(got) != 2 {
		t.Fatalf("expected control + 1 chunk on synth-error fail-open, got %d", len(got))
	}
	if got[0].Document.ID != AgenticControlID || got[0].Document.Metadata[AgenticStatusKey] != "answer" {
		t.Fatalf("fail-open control should default to status answer, got %v", got[0].Document.Metadata)
	}
	if _, ok := got[0].Document.Metadata[AgenticTraceKey].(agenticTrace); !ok {
		t.Fatal("fail-open control should still carry the loop trace")
	}
	if got[1].Document.ID != "chunk1" {
		t.Fatal("original chunk should follow the control")
	}
}

func TestEnableAgenticRetrieval_ClampsMaxHops(t *testing.T) {
	q := &QueryService{}
	q.EnableAgenticRetrieval(&fakePlanner{}, 0)
	if q.agenticMaxHops != 1 {
		t.Fatalf("maxHops < 1 should clamp to 1, got %d", q.agenticMaxHops)
	}
}

func resultIDs(rs []domain.SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Document.ID
	}
	return out
}
