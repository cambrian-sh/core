package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0126 D6: the correlation handle a boundary seeds, and the per-hop suffixes
// that keep a multi-hop call's receipts from landing on top of one another.

// recordingObserver captures every decision the retrieval path emits.
type recordingObserver struct{ queryIDs []string }

func (r *recordingObserver) ObserveRetrieval(d domain.RetrievalDecision) {
	r.queryIDs = append(r.queryIDs, d.QueryID)
}

// recordingAuthorizer is a decision point that allows everything and REMEMBERS
// which principal it was asked about — which is the only way to prove a caller id
// reached the read chokepoint rather than being dropped on the way.
type recordingAuthorizer struct {
	domain.AllowAllAuthorizer
	principals []string
}

func (a *recordingAuthorizer) ReadFilter(_ context.Context, p domain.PrincipalRef, _ domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	a.principals = append(a.principals, p.ID)
	return &domain.TagPredicate{}, domain.AccessDecision{Allowed: true, Principal: p, Reason: domain.ReasonAllowed}
}

// citedPlanner is a fakePlanner that can also answer with citations, which is
// what AnswerSystem/Answer require of a planner (CitedSynthesizer).
type citedPlanner struct {
	fakePlanner
	citedText string
}

func (c *citedPlanner) SynthesizeCited(_ context.Context, _ string, _ []string) (string, string, error) {
	return "answer", c.citedText, nil
}

func corpusOfOne() *scopeApplyingStore {
	return &scopeApplyingStore{docs: []domain.Document{
		{ID: "kb", Text: "the answer", Metadata: map[string]interface{}{"tags": []string{"public_kb"}}},
	}}
}

// THE property the receipt chain depends on. A multi-hop retrieval emits one
// decision per hop, so a 1:1 correlation id would put hop 2 on hop 1's handle: one
// row wins, the rest are unfindable, and the chain a caller fetches reads
// incomplete without ever saying so.
func TestEmitDecision_HopsExtendTheCorrelationPrefix(t *testing.T) {
	t.Parallel()
	obs := &recordingObserver{}
	q := NewQueryService(&fakeEmbedder{}, corpusOfOne(), &recordingAuthorizer{})
	q.SetDecisionObserver(obs, "fp-1")
	// Two hops: the planner never stops and always names a fresh bridge, and the
	// loop is capped at 2.
	q.EnableAgenticRetrieval(&fakePlanner{rewrite: "planned", bridge: "bridge-1"}, 2)

	ctx := domain.WithQueryCorrelation(context.Background(), "mcp-abc")
	if _, err := q.Search(ctx, "a bridge question", "ci-bot"); err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(obs.queryIDs) != 2 {
		t.Fatalf("query ids = %v, want one per hop (2)", obs.queryIDs)
	}
	if obs.queryIDs[0] != "mcp-abc-h1" || obs.queryIDs[1] != "mcp-abc-h2" {
		t.Fatalf("query ids = %v, want [mcp-abc-h1 mcp-abc-h2]", obs.queryIDs)
	}
}

// ENG-2068 (AGENTS.md §2.6): without a seeded handle the entry point derives
// one from the request — no clock, no process-local counter — so the same
// request asked again (a retry, or a second QueryService after a restart) names
// the same receipts, hops still get their own ids, and a different question,
// principal, session or plan step gets different ones.
func TestEmitDecision_NoCorrelationDerivesStableIDsFromTheRequest(t *testing.T) {
	t.Parallel()
	run := func(ctx context.Context, query, caller string) []string {
		t.Helper()
		obs := &recordingObserver{}
		q := NewQueryService(&fakeEmbedder{}, corpusOfOne(), &recordingAuthorizer{})
		q.SetDecisionObserver(obs, "fp-1")
		q.EnableAgenticRetrieval(&fakePlanner{rewrite: "planned", bridge: "bridge-1"}, 2)
		if _, err := q.Search(ctx, query, caller); err != nil {
			t.Fatalf("Search: %v", err)
		}
		return obs.queryIDs
	}
	bg := context.Background()
	first := run(bg, "a bridge question", "ci-bot")
	if len(first) != 2 {
		t.Fatalf("query ids = %v, want one per hop (2)", first)
	}
	prefix := domain.DerivedQueryCorrelation(bg, "ci-bot", "a bridge question")
	if first[0] != prefix+"-h1" || first[1] != prefix+"-h2" {
		t.Fatalf("query ids = %v, want [%s-h1 %s-h2]", first, prefix, prefix)
	}
	if again := run(bg, "a bridge question", "ci-bot"); again[0] != first[0] || again[1] != first[1] {
		t.Fatalf("a retry of the same request must name the same receipts: %v vs %v", again, first)
	}
	sess := domain.WithSessionID(bg, "conv-1")
	for name, ids := range map[string][]string{
		"other question":  run(bg, "another question", "ci-bot"),
		"other principal": run(bg, "a bridge question", "other-bot"),
		"other session":   run(sess, "a bridge question", "ci-bot"),
		"plan step":       run(domain.WithStepCall(sess, domain.StepCall{RunID: "run-9", StepIndex: 2}), "a bridge question", "ci-bot"),
	} {
		if ids[0] == first[0] || !strings.HasSuffix(ids[0], "-h1") {
			t.Errorf("%s: id %q must differ from %q and keep the hop shape", name, ids[0], first[0])
		}
	}
	// The single-pass lanes derive the same way.
	obs := &recordingObserver{}
	q := NewQueryService(&fakeEmbedder{}, corpusOfOne(), &recordingAuthorizer{})
	q.SetDecisionObserver(obs, "fp-1")
	if _, err := q.SearchActions(bg, "what did I do", "ci-bot"); err != nil {
		t.Fatal(err)
	}
	if want := domain.DerivedQueryCorrelation(bg, "ci-bot", "what did I do") + "-h1"; len(obs.queryIDs) != 1 || obs.queryIDs[0] != want {
		t.Fatalf("single-pass ids = %v, want [%s]", obs.queryIDs, want)
	}
}

// ADR-0126 E6: the scoped answer variant. AnswerSystem reads at ScopeSystem by
// design; the published ask_memory tool must not, or a premium PDP never bites on
// the one tool that returns prose.
func TestAnswer_ScopesRetrievalByTheCaller(t *testing.T) {
	t.Parallel()
	authz := &recordingAuthorizer{}
	q := NewQueryService(&fakeEmbedder{}, corpusOfOne(), authz)
	q.EnableAgenticRetrieval(&citedPlanner{citedText: "grounded [1]"}, 1)

	status, answer, evidence, err := q.Answer(context.Background(), "who?", "mcp:ci-bot")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if status != "answer" || answer != "grounded [1]" || len(evidence) == 0 {
		t.Fatalf("status=%q answer=%q evidence=%d", status, answer, len(evidence))
	}
	if len(authz.principals) == 0 {
		t.Fatal("the read chokepoint was never asked about a principal")
	}
	for _, p := range authz.principals {
		if p != "mcp:ci-bot" {
			t.Errorf("read chokepoint saw principal %q, want the caller mcp:ci-bot", p)
		}
	}
}

// The system lane is unchanged: it still runs at ScopeSystem under the kernel's
// own caller id, so the operator answer panel keeps full visibility.
func TestAnswerSystem_StillReadsAtSystemScope(t *testing.T) {
	t.Parallel()
	authz := &recordingAuthorizer{}
	q := NewQueryService(&fakeEmbedder{}, corpusOfOne(), authz)
	q.EnableAgenticRetrieval(&citedPlanner{citedText: "grounded [1]"}, 1)

	if _, _, _, err := q.AnswerSystem(context.Background(), "who?"); err != nil {
		t.Fatalf("AnswerSystem: %v", err)
	}
	for _, p := range authz.principals {
		if p != "operator:system" {
			t.Errorf("system lane saw principal %q, want operator:system", p)
		}
	}
}
