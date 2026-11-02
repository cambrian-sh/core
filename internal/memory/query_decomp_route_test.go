package memory

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// routedPlanner is decompPlanner plus the ADR-0135 route.
type routedPlanner struct {
	*decompPlanner
	route       string
	routedCalls atomic.Int32
}

func (p *routedPlanner) DecomposeRouted(context.Context, string) (DecompositionPlan, error) {
	p.routedCalls.Add(1)
	return DecompositionPlan{Route: p.route, SubQuestions: p.subqs, Refs: p.refs}, nil
}

var _ RoutedDecomposePlanner = (*routedPlanner)(nil)

// newRoutedFixture is newDecompFixture with a planner that returns route, and
// adaptive routes set to adaptive.
func newRoutedFixture(route string, adaptive bool, subqs, refs []string, answers map[string]string, byQuery map[string][]string, concurrency int) *decompFixture {
	f, _ := newRoutedFixtureP(route, adaptive, subqs, refs, answers, byQuery, concurrency)
	return f
}

func newRoutedFixtureP(route string, adaptive bool, subqs, refs []string, answers map[string]string, byQuery map[string][]string, concurrency int) (*decompFixture, *routedPlanner) {
	f := newDecompFixture(subqs, refs, answers, byQuery, concurrency)
	rp := &routedPlanner{decompPlanner: f.plan, route: route}
	f.q.EnableAgenticRetrieval(rp, 4)
	f.q.SetAdaptiveRoutesEnabled(adaptive)
	return f, rp
}

func controlMeta(t *testing.T, rs []domain.SearchResult) map[string]interface{} {
	t.Helper()
	controlTrace(t, rs)
	return rs[0].Document.Metadata
}

func TestResolveDecompRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name               string
		plan               DecompositionPlan
		wantRoute, wantFbk string
	}{
		{"direct, no sub-question", DecompositionPlan{Route: "direct"}, DecompRouteDirect, ""},
		{"direct, one sub-question", DecompositionPlan{Route: " Direct ", SubQuestions: []string{"q?"}}, DecompRouteDirect, ""},
		{"direct, two sub-questions", DecompositionPlan{Route: "direct", SubQuestions: []string{"a?", "b?"}}, DecompRouteChain, routeFallbackDirectMultiSQ},
		{"facets", DecompositionPlan{Route: "facets", SubQuestions: []string{"a?", "b?"}, Refs: []string{"a", "b"}}, DecompRouteFacets, ""},
		{"facets, {n} in a sub-question", DecompositionPlan{Route: "facets", SubQuestions: []string{"a?", "b of {1}?"}}, DecompRouteChain, routeFallbackFacetsPlaceh},
		{"facets, {n} in a ref", DecompositionPlan{Route: "facets", SubQuestions: []string{"a?", "b?"}, Refs: []string{"a", "b of {1}"}}, DecompRouteChain, routeFallbackFacetsPlaceh},
		{"chain", DecompositionPlan{Route: "CHAIN", SubQuestions: []string{"a?", "b of {1}?"}}, DecompRouteChain, ""},
		{"chain without placeholders", DecompositionPlan{Route: "chain", SubQuestions: []string{"a?", "b?"}}, DecompRouteChain, ""},
		{"missing", DecompositionPlan{SubQuestions: []string{"a?"}}, DecompRouteChain, routeFallbackMissing},
		{"unknown", DecompositionPlan{Route: "parallel", SubQuestions: []string{"a?"}}, DecompRouteChain, routeFallbackUnknown},
	}
	for _, c := range cases {
		got := resolveDecompRoute(c.plan, true)
		if got.Route != c.wantRoute || got.Fallback != c.wantFbk {
			t.Errorf("%s: got route %q fallback %q, want %q %q", c.name, got.Route, got.Fallback, c.wantRoute, c.wantFbk)
		}
	}
}

// D2 direct: ONE pass on the original question (never the planner's rewrite), no
// extraction call, no synthesis for a caller that did not opt in — the single
// pass's rows.
func TestDecompSearch_DirectRouteIsTheSinglePass(t *testing.T) {
	t.Parallel()
	const query = "Where do I take yoga classes?"
	const rewrite = "Which studio hosts the user's yoga classes?"
	byQuery := map[string][]string{query: {"yoga-studio", "yoga-schedule"}, rewrite: {"other"}}
	f := newRoutedFixture(DecompRouteDirect, true, []string{rewrite}, []string{"the studio"}, nil, byQuery, 4)

	got, err := f.q.Search(context.Background(), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if qs := f.store.queries(); !reflect.DeepEqual(qs, []string{query}) {
		t.Fatalf("direct must run one pass on the original question, ran %q", qs)
	}
	if len(f.plan.answered()) != 0 || f.plan.synthCalls != 0 {
		t.Fatalf("direct: want 0 extraction and 0 synthesis, got %d and %d", len(f.plan.answered()), f.plan.synthCalls)
	}
	single := NewQueryService(f.store.emb, f.store, nil)
	want, _ := single.Search(context.Background(), query, "agent-a")
	if !reflect.DeepEqual(resultIDs(got), resultIDs(want)) {
		t.Fatalf("direct rows %v differ from the single pass %v", resultIDs(got), resultIDs(want))
	}

	// Opted in: the control row names the route; one synthesis, still no extraction.
	g := newRoutedFixture(DecompRouteDirect, true, []string{rewrite}, nil, nil, byQuery, 4)
	opted, err := g.q.Search(WithAgenticSynthesis(context.Background()), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	md := controlMeta(t, opted)
	if md[AgenticRouteKey] != DecompRouteDirect || md[AgenticRoutePlannedKey] != DecompRouteDirect || md[AgenticRouteFallbackKey] != "" {
		t.Fatalf("control row route keys: %v %v %v", md[AgenticRouteKey], md[AgenticRoutePlannedKey], md[AgenticRouteFallbackKey])
	}
	tr := controlTrace(t, opted)
	if tr.Route != DecompRouteDirect || len(tr.Hops) != 1 || tr.Hops[0].Decision != "direct_single_pass" {
		t.Fatalf("direct trace: %+v", tr)
	}
	if g.plan.synthCalls != 1 || len(g.plan.answered()) != 0 {
		t.Fatalf("opted-in direct: want 1 synthesis, 0 extraction; got %d, %d", g.plan.synthCalls, len(g.plan.answered()))
	}
}

// D2 facets: floor + every sub-question, each extracted, all in ONE wave (they
// overlap), ranked exactly as the same plan without routing.
func TestDecompSearch_FacetsRunInOneParallelWave(t *testing.T) {
	t.Parallel()
	const query = "How many graduation ceremonies have I attended in the past three months?"
	subqs := []string{"Did I attend a graduation ceremony in August?", "Did I attend a graduation ceremony in September?", "Did I attend a graduation ceremony in October?"}
	f := newRoutedFixture(DecompRouteFacets, true, subqs, nil, nil, nil, 4)
	f.store.delay = 20 * time.Millisecond
	got, err := f.q.Search(WithAgenticSynthesis(context.Background()), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.store.queries()); n != 4 {
		t.Fatalf("facets: want floor + 3 passes, ran %d", n)
	}
	if n := len(f.plan.answered()); n != 3 {
		t.Fatalf("facets: want 3 extraction calls, got %d", n)
	}
	f.store.mu.Lock()
	maxIn := f.store.maxIn
	f.store.mu.Unlock()
	if maxIn < 4 {
		t.Fatalf("facets should form one wave with the floor (4 in flight), max in flight was %d", maxIn)
	}
	tr := controlTrace(t, got)
	if tr.Route != DecompRouteFacets || controlMeta(t, got)[AgenticRouteKey] != DecompRouteFacets {
		t.Fatalf("facets route not recorded: %+v", tr)
	}

	// Same plan, routing off: identical evidence rows and hops.
	base := newDecompFixture(subqs, nil, nil, nil, 4)
	want, err := base.q.Search(WithAgenticSynthesis(context.Background()), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[1:], want[1:]) || !reflect.DeepEqual(tr.Hops, controlTrace(t, want).Hops) {
		t.Fatalf("facets must rank as the unrouted plan: %v vs %v", resultIDs(got[1:]), resultIDs(want[1:]))
	}
}

// D2 chain: today's grounded chain, with the route recorded.
func TestDecompSearch_ChainRouteIsToday(t *testing.T) {
	t.Parallel()
	query, subqs, refs, answers, byQuery := mixedPlan()
	f := newRoutedFixture(DecompRouteChain, true, subqs, refs, answers, byQuery, 4)
	got, err := f.q.Search(WithAgenticSynthesis(context.Background()), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	base := newDecompFixture(subqs, refs, answers, byQuery, 4)
	want, err := base.q.Search(WithAgenticSynthesis(context.Background()), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	tr, wtr := controlTrace(t, got), controlTrace(t, want)
	if !reflect.DeepEqual(got[1:], want[1:]) || !reflect.DeepEqual(tr.Hops, wtr.Hops) {
		t.Fatal("chain route must execute exactly the unrouted decomposition")
	}
	if tr.Route != DecompRouteChain || tr.RouteFallback != "" {
		t.Fatalf("chain trace route: %q fallback %q", tr.Route, tr.RouteFallback)
	}
}

// D3: every unusable route executes as chain and says why.
func TestDecompSearch_FallbacksRunAsChain(t *testing.T) {
	t.Parallel()
	query, subqs, refs, answers, byQuery := mixedPlan()
	base := newDecompFixture(subqs, refs, answers, byQuery, 4)
	want, err := base.q.Search(WithAgenticSynthesis(context.Background()), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ route, planned, reason string }{
		{"facets", "facets", routeFallbackFacetsPlaceh}, // mixedPlan carries {n}
		{"direct", "direct", routeFallbackDirectMultiSQ},
		{"sideways", "sideways", routeFallbackUnknown},
		{"", "", routeFallbackMissing},
	}
	for _, c := range cases {
		f := newRoutedFixture(c.route, true, subqs, refs, answers, byQuery, 4)
		got, err := f.q.Search(WithAgenticSynthesis(context.Background()), query, "agent-a")
		if err != nil {
			t.Fatal(err)
		}
		md := controlMeta(t, got)
		if md[AgenticRouteKey] != DecompRouteChain || md[AgenticRoutePlannedKey] != c.planned || md[AgenticRouteFallbackKey] != c.reason {
			t.Errorf("route %q: got %v/%v/%v", c.route, md[AgenticRouteKey], md[AgenticRoutePlannedKey], md[AgenticRouteFallbackKey])
		}
		if !reflect.DeepEqual(got[1:], want[1:]) || !reflect.DeepEqual(controlTrace(t, got).Hops, controlTrace(t, want).Hops) {
			t.Errorf("route %q: fallback did not execute as chain", c.route)
		}
	}

	// A planner without DecomposeRouted (an old agent / plain DecomposePlanner)
	// has no route: chain, "missing_route".
	plain := newDecompFixture(subqs, refs, answers, byQuery, 4)
	plain.q.SetAdaptiveRoutesEnabled(true)
	got, err := plain.q.Search(WithAgenticSynthesis(context.Background()), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if md := controlMeta(t, got); md[AgenticRouteKey] != DecompRouteChain || md[AgenticRouteFallbackKey] != routeFallbackMissing {
		t.Fatalf("plain DecomposePlanner: %v / %v", md[AgenticRouteKey], md[AgenticRouteFallbackKey])
	}
}

// D4: with the flag off, a routed planner changes nothing — rows, control row,
// trace and the calls made are those of today's unrouted decomposition, for
// every route the planner might choose.
func TestDecompSearch_FlagOffIsToday(t *testing.T) {
	t.Parallel()
	query, subqs, refs, answers, byQuery := mixedPlan()
	const yoga = "Where do I take yoga classes?"
	plans := []struct {
		query       string
		subqs, refs []string
	}{
		{query, subqs, refs},
		{yoga, []string{yoga}, nil},                                  // echo
		{yoga, []string{"Which studio hosts yoga?"}, nil},            // rewrite
		{yoga, []string{"Q1 yoga?", "Q2 yoga?"}, []string{"a", "b"}}, // independent
	}
	for _, route := range []string{"direct", "facets", "chain", "", "bogus"} {
		for _, p := range plans {
			for _, opt := range []bool{false, true} {
				ctx := context.Background()
				if opt {
					ctx = WithAgenticSynthesis(ctx)
				}
				off, rp := newRoutedFixtureP(route, false, p.subqs, p.refs, answers, byQuery, 4)
				got, err := off.q.Search(ctx, p.query, "agent-a")
				if err != nil {
					t.Fatal(err)
				}
				if n := rp.routedCalls.Load(); n != 0 {
					t.Fatalf("flag off must send today's planner request, not the routed one (%d routed calls)", n)
				}
				base := newDecompFixture(p.subqs, p.refs, answers, byQuery, 4)
				want, err := base.q.Search(ctx, p.query, "agent-a")
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("route %q plan %q opt %v: flag-off results differ from today", route, p.subqs, opt)
				}
				if len(off.store.queries()) != len(base.store.queries()) || len(off.plan.answered()) != len(base.plan.answered()) || off.plan.synthCalls != base.plan.synthCalls {
					t.Fatalf("route %q plan %q opt %v: flag-off made different calls", route, p.subqs, opt)
				}
				if opt {
					if _, ok := got[0].Document.Metadata[AgenticRouteKey]; ok {
						t.Fatalf("route %q: flag-off control row carries a route key", route)
					}
				}
			}
		}
	}
}

// Owner decision 3: the operator Answer path routes too — a direct question
// answers from one pass with no extraction call.
func TestAnswer_RoutesWhenAdaptive(t *testing.T) {
	t.Parallel()
	const query = "Where do I take yoga classes?"
	f := newRoutedFixture(DecompRouteDirect, true, []string{"Which studio hosts yoga?"}, nil, nil, map[string][]string{query: {"yoga-studio"}}, 4)
	status, answer, evidence, err := f.q.Answer(context.Background(), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if qs := f.store.queries(); !reflect.DeepEqual(qs, []string{query}) || len(f.plan.answered()) != 0 {
		t.Fatalf("answer lane direct: ran %q, %d extractions", qs, len(f.plan.answered()))
	}
	if status != "answer" || answer != "cited [1]" || !reflect.DeepEqual(resultIDs(evidence), []string{"yoga-studio"}) {
		t.Fatalf("answer lane result: %q %q %v", status, answer, resultIDs(evidence))
	}
}
