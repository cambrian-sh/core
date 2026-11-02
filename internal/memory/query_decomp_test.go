package memory

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ── fakes ────────────────────────────────────────────────────────────────────

// queryEmbedder gives every distinct query text its own one-dimensional vector,
// so queryStore can answer per query. Safe for concurrent use.
type queryEmbedder struct {
	mu    sync.Mutex
	byQ   map[string]int
	texts []string
}

func (e *queryEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.byQ == nil {
		e.byQ = map[string]int{}
	}
	idx, ok := e.byQ[text]
	if !ok {
		idx = len(e.texts)
		e.byQ[text] = idx
		e.texts = append(e.texts, text)
	}
	return []float32{float32(idx)}, nil
}

func (e *queryEmbedder) textOf(v []float32) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.texts[int(v[0])]
}

type ctxKeyTestMarker struct{}

// storeCall is what one vector search saw.
type storeCall struct {
	query    string
	session  domain.SessionID
	marker   any
	hasScope bool
	seq      int64
}

// queryStore returns a fixed ranked list per query text, records every call and
// its context values, and tracks how many searches were in flight at once.
type queryStore struct {
	scopeApplyingStore // the unused VectorStore methods
	emb                *queryEmbedder
	byQuery            map[string][]string // query → ranked doc ids
	errQueries         map[string]bool
	delay              time.Duration
	seq                *atomic.Int64

	mu       sync.Mutex
	calls    []storeCall
	inFlight int
	maxIn    int
}

func (s *queryStore) Search(ctx context.Context, v []float32, _ domain.SearchOptions) ([]domain.SearchResult, error) {
	q := s.emb.textOf(v)
	sid, _ := domain.SessionIDFromContext(ctx)
	sc, ok := domain.ScopeFromContext(ctx)
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.maxIn {
		s.maxIn = s.inFlight
	}
	s.calls = append(s.calls, storeCall{query: q, session: sid, marker: ctx.Value(ctxKeyTestMarker{}), hasScope: ok && sc != nil, seq: s.seq.Add(1)})
	s.mu.Unlock()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	if s.errQueries[q] {
		return nil, errors.New("store down for " + q)
	}
	ids, ok := s.byQuery[q]
	if !ok {
		ids = []string{"generic-1", "generic-2"}
	}
	out := make([]domain.SearchResult, 0, len(ids))
	for r, id := range ids {
		sc := 0.9 - 0.01*float64(r)
		out = append(out, domain.SearchResult{Score: sc, RawScore: sc,
			Document: domain.Document{ID: id, Text: id + " text", Metadata: map[string]any{}}})
	}
	return out, nil
}

func (s *queryStore) queries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	for i, c := range s.calls {
		out[i] = c.query
	}
	return out
}

func (s *queryStore) seqOf(query string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c.query == query {
			return c.seq
		}
	}
	return -1
}

type answerCall struct {
	subq    string
	session domain.SessionID
	marker  any
	seq     int64
}

// decompPlanner is a thread-safe Planner + DecomposePlanner + CitedSynthesizer.
type decompPlanner struct {
	subqs, refs []string
	answers     map[string]string // resolved sub-question → grounded answer
	seq         *atomic.Int64

	mu          sync.Mutex
	answerCalls []answerCall
	synthCalls  int
	citedCalls  int
}

func (p *decompPlanner) PlanQuery(context.Context, string, string, []string, int) (string, error) {
	return "", nil
}
func (p *decompPlanner) DecideContinue(context.Context, string, []string, []string) (bool, string, error) {
	return true, "", nil
}
func (p *decompPlanner) Synthesize(context.Context, string, []string) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.synthCalls++
	return "answer", "synthesized", nil
}
func (p *decompPlanner) SynthesizeCited(context.Context, string, []string) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.citedCalls++
	return "answer", "cited [1]", nil
}
func (p *decompPlanner) Decompose(context.Context, string) ([]string, []string, error) {
	return p.subqs, p.refs, nil
}
func (p *decompPlanner) AnswerSubQuestion(ctx context.Context, subq string, _ []string) (string, error) {
	sid, _ := domain.SessionIDFromContext(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answerCalls = append(p.answerCalls, answerCall{subq: subq, session: sid, marker: ctx.Value(ctxKeyTestMarker{}), seq: p.seq.Add(1)})
	return p.answers[subq], nil
}

func (p *decompPlanner) answered() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.answerCalls))
	for i, c := range p.answerCalls {
		out[i] = c.subq
	}
	return out
}

type decompFixture struct {
	q     *QueryService
	store *queryStore
	plan  *decompPlanner
}

func newDecompFixture(subqs, refs []string, answers map[string]string, byQuery map[string][]string, concurrency int) *decompFixture {
	seq := &atomic.Int64{}
	emb := &queryEmbedder{}
	store := &queryStore{emb: emb, byQuery: byQuery, seq: seq, errQueries: map[string]bool{}}
	plan := &decompPlanner{subqs: subqs, refs: refs, answers: answers, seq: seq}
	q := NewQueryService(emb, store, nil)
	q.EnableAgenticRetrieval(plan, 4)
	q.SetDecomposeEnabled(true)
	q.SetSubQuestionConcurrency(concurrency)
	return &decompFixture{q: q, store: store, plan: plan}
}

func controlTrace(t *testing.T, rs []domain.SearchResult) agenticTrace {
	t.Helper()
	if len(rs) == 0 || rs[0].Document.ID != AgenticControlID {
		t.Fatalf("expected a control row first, got %v", resultIDs(rs))
	}
	tr, ok := rs[0].Document.Metadata[AgenticTraceKey].(agenticTrace)
	if !ok {
		t.Fatal("control row carries no trace")
	}
	return tr
}

// ── echo detection ──────────────────────────────────────────────────────────

func TestIsEchoPlan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
		subqs []string
		want  bool
	}{
		{"identical", "Where do I take yoga classes?", []string{"Where do I take yoga classes?"}, true},
		{"case, punctuation and spacing", "Where do I take yoga classes?", []string{"  where do i take YOGA classes "}, true},
		{"curly vs straight apostrophe", "What’s my dog’s name?", []string{"What's my dog's name?"}, true},
		// Near-misses taken from the 2026-11-02 LongMemEval arm1 log: each is a
		// rewrite the planner chose and must keep its plan.
		{"grammar fix gave->give", "What did my dad gave me as a birthday gift?", []string{"What did my dad give me as a birthday gift?"}, false},
		{"dropped article", "What percentage of leadership positions do women hold in the my company?", []string{"What percentage of leadership positions do women hold in my company?"}, false},
		{"person rewrite", "What is the order of airlines I flew with from earliest to latest before today?", []string{"What is the order of airlines the user flew with from earliest to latest before today?"}, false},
		{"paraphrase", "I received a piece of jewelry last Saturday from whom?", []string{"Who gave me a piece of jewelry last Saturday?"}, false},
		{"entity swap", "Where does Alice work?", []string{"Where does Alicia work?"}, false},
		{"two sub-questions, first echoes", "Where does Alice work?", []string{"Where does Alice work?", "Who runs {1}?"}, false},
		{"placeholder", "Where does {1} work?", []string{"Where does {1} work?"}, false},
		{"empty", "", []string{""}, false},
		{"no plan", "Where does Alice work?", nil, false},
	}
	for _, c := range cases {
		if got := isEchoPlan(c.query, c.subqs); got != c.want {
			t.Errorf("%s: isEchoPlan(%q, %q) = %v, want %v", c.name, c.query, c.subqs, got, c.want)
		}
	}
}

// An echo plan runs ONE retrieval pass and no extraction call, and its rows are
// exactly the single pass's.
func TestDecompSearch_EchoPlanIsTheSinglePass(t *testing.T) {
	t.Parallel()
	const query = "Where do I take yoga classes?"
	byQuery := map[string][]string{query: {"yoga-studio", "yoga-schedule", "unrelated"}}
	f := newDecompFixture([]string{"where do I take yoga classes"}, []string{"the yoga place"}, nil, byQuery, 4)

	got, err := f.q.Search(context.Background(), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if qs := f.store.queries(); len(qs) != 1 || qs[0] != query {
		t.Fatalf("echo plan must run exactly one pass on the original question, ran %q", qs)
	}
	if a := f.plan.answered(); len(a) != 0 {
		t.Fatalf("echo plan must not extract a sub-answer, got calls %q", a)
	}
	if f.plan.synthCalls != 0 {
		t.Fatalf("no opt-in ⇒ no synthesis call, got %d", f.plan.synthCalls)
	}

	// The single pass, from a non-agentic service over the same store.
	single := NewQueryService(f.store.emb, f.store, nil)
	want, err := single.Search(context.Background(), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resultIDs(got), resultIDs(want)) {
		t.Fatalf("echo rows %v differ from the single pass %v", resultIDs(got), resultIDs(want))
	}
}

// An opted-in caller still gets its control row on an echo, and the trace says why
// only one pass ran.
func TestDecompSearch_EchoPlanTraceForOptedInCaller(t *testing.T) {
	t.Parallel()
	const query = "Where do I take yoga classes?"
	f := newDecompFixture([]string{query}, nil, nil, map[string][]string{query: {"yoga-studio"}}, 1)

	got, err := f.q.Search(WithAgenticSynthesis(context.Background()), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	tr := controlTrace(t, got)
	if len(tr.Hops) != 1 || tr.Hops[0].Decision != "echo_single_pass" {
		t.Fatalf("trace should record the echo, got %+v", tr.Hops)
	}
	if f.plan.synthCalls != 1 || len(f.plan.answered()) != 0 || len(f.store.queries()) != 1 {
		t.Fatalf("opt-in echo: want 1 synth, 0 extract, 1 pass; got %d, %d, %d",
			f.plan.synthCalls, len(f.plan.answered()), len(f.store.queries()))
	}
	if !reflect.DeepEqual(resultIDs(got[1:]), []string{"yoga-studio"}) {
		t.Fatalf("rows after the control should be the single pass, got %v", resultIDs(got[1:]))
	}
}

// A near-miss rewrite is a real plan: coverage-floor pass + sub-question pass +
// one extraction call.
func TestDecompSearch_NearMissRewriteKeepsItsPlan(t *testing.T) {
	t.Parallel()
	const query = "What did my dad gave me as a birthday gift?"
	const subq = "What did my dad give me as a birthday gift?"
	f := newDecompFixture([]string{subq}, nil, nil, nil, 1)

	if _, err := f.q.Search(context.Background(), query, "agent-a"); err != nil {
		t.Fatal(err)
	}
	if qs := f.store.queries(); !reflect.DeepEqual(qs, []string{query, subq}) {
		t.Fatalf("rewrite should run floor + sub-question passes, ran %q", qs)
	}
	if a := f.plan.answered(); !reflect.DeepEqual(a, []string{subq}) {
		t.Fatalf("rewrite should extract its sub-answer, got %q", a)
	}
}

// ── synthesis opt-in ────────────────────────────────────────────────────────

func TestDecompSearch_SynthesisOnlyForOptedInCallers(t *testing.T) {
	t.Parallel()
	const query = "Which award did the author of Tales receive?"
	subqs := []string{"Who wrote Tales?", "Which award did {1} receive?"}
	answers := map[string]string{"Who wrote Tales?": "Shaun Tan"}

	// Not opted in: no LLM synthesis, no control row, retrieved evidence only.
	f := newDecompFixture(subqs, nil, answers, nil, 1)
	got, err := f.q.Search(context.Background(), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if f.plan.synthCalls != 0 {
		t.Fatalf("synthesis ran for a caller that did not opt in (%d calls)", f.plan.synthCalls)
	}
	for _, r := range got {
		if r.Document.ID == AgenticControlID {
			t.Fatal("control row built for a caller that did not opt in")
		}
	}

	// Opted in: one synthesis call, control row first, rows after it identical.
	g := newDecompFixture(subqs, nil, answers, nil, 1)
	opted, err := g.q.Search(WithAgenticSynthesis(context.Background()), query, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if g.plan.synthCalls != 1 {
		t.Fatalf("opted-in caller: want 1 synthesis call, got %d", g.plan.synthCalls)
	}
	tr := controlTrace(t, opted)
	if tr.FinalAnswer != "synthesized" || len(tr.Hops) != 3 {
		t.Fatalf("control trace incomplete: %+v", tr)
	}
	if !reflect.DeepEqual(resultIDs(opted[1:]), resultIDs(got)) {
		t.Fatalf("opt-in must not change the evidence rows: %v vs %v", resultIDs(opted[1:]), resultIDs(got))
	}
}

// The answer lane reads the control row's status (abstention), so it keeps the
// loop's synthesis without any caller opt-in.
func TestAnswer_KeepsAgenticSynthesis(t *testing.T) {
	t.Parallel()
	subqs := []string{"Who wrote Tales?", "Which award did {1} receive?"}
	f := newDecompFixture(subqs, nil, map[string]string{"Who wrote Tales?": "Shaun Tan"}, nil, 1)
	status, answer, evidence, err := f.q.Answer(context.Background(), "Which award did the author of Tales receive?", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if f.plan.synthCalls != 1 || f.plan.citedCalls != 1 {
		t.Fatalf("answer lane: want loop synthesis 1 + cited 1, got %d + %d", f.plan.synthCalls, f.plan.citedCalls)
	}
	if status != "answer" || answer != "cited [1]" || len(evidence) == 0 {
		t.Fatalf("answer lane result: %q %q %d", status, answer, len(evidence))
	}
}

// ── parallel sub-questions ──────────────────────────────────────────────────

// A plan mixing independent and dependent sub-questions:
//
//	0: independent           2: independent
//	1: depends on {1}        3: depends on {2} (in its text) and {3} (in its ref)
func mixedPlan() (query string, subqs, refs []string, answers map[string]string, byQuery map[string][]string) {
	query = "Did the director of Alpha and the composer of Beta work together?"
	subqs = []string{
		"Who directed Alpha?",
		"What else did {1} direct?",
		"Who composed Beta?",
		"Did {2} score a film with {3}?",
	}
	refs = []string{"the director of Alpha", "the other films of {1}", "the composer of Beta", "the collaboration of {2} and {3}"}
	answers = map[string]string{
		"Who directed Alpha?":                               "Ann Lee",
		"What else did Ann Lee direct?":                     "Gamma",
		"Who composed Beta?":                                "", // unfound ⇒ ref fallback "the composer of Beta"
		"Did Gamma score a film with the composer of Beta?": "yes",
	}
	byQuery = map[string][]string{
		query:                           {"surface-1", "shared", "surface-2"},
		"Who directed Alpha?":           {"alpha-credits", "shared", "alpha-2"},
		"What else did Ann Lee direct?": {"lee-filmography", "alpha-credits", "lee-2"},
		"Who composed Beta?":            {"beta-credits", "beta-2", "shared"},
		"Did Gamma score a film with the composer of Beta?": {"gamma-score", "beta-credits", "gamma-2"},
	}
	return
}

// The ranking-equivalence proof: for the same plan, the parallel scheduler
// returns the same rows in the same order, the same trace and the same
// sub-answers as the sequential loop — repeated, so a scheduling-dependent order
// would show.
func TestDecompSearch_ParallelEqualsSequential(t *testing.T) {
	t.Parallel()
	query, subqs, refs, answers, byQuery := mixedPlan()
	run := func(conc int, delay time.Duration) ([]domain.SearchResult, agenticTrace, []string) {
		f := newDecompFixture(subqs, refs, answers, byQuery, conc)
		f.store.delay = delay
		got, err := f.q.Search(WithAgenticSynthesis(context.Background()), query, "agent-a")
		if err != nil {
			t.Fatal(err)
		}
		tr := controlTrace(t, got)
		return got[1:], tr, f.store.queries()
	}
	seqRows, seqTrace, seqQueries := run(1, 0)
	if len(seqQueries) != 5 {
		t.Fatalf("sequential should run floor + 4 passes, ran %q", seqQueries)
	}
	for i := 0; i < 20; i++ {
		parRows, parTrace, parQueries := run(4, time.Duration(i%3)*time.Millisecond)
		if !reflect.DeepEqual(parRows, seqRows) {
			t.Fatalf("iteration %d: parallel rows %v != sequential %v", i, resultIDs(parRows), resultIDs(seqRows))
		}
		if !reflect.DeepEqual(parTrace, seqTrace) {
			t.Fatalf("iteration %d: parallel trace differs:\n par %+v\n seq %+v", i, parTrace.Hops, seqTrace.Hops)
		}
		if len(parQueries) != len(seqQueries) {
			t.Fatalf("iteration %d: parallel ran %d passes, sequential %d", i, len(parQueries), len(seqQueries))
		}
	}
	// The substitution chain resolved as the sequential loop resolves it,
	// including the ref fallback for the unfound answer.
	want := []string{"Who directed Alpha?", "What else did Ann Lee direct?", "Who composed Beta?", "Did Gamma score a film with the composer of Beta?"}
	for i, h := range seqTrace.Hops[1:] {
		if h.PlannedQuery != want[i] {
			t.Fatalf("hop %d planned %q, want %q", i, h.PlannedQuery, want[i])
		}
	}
}

// Dependent sub-questions start only after the answers they substitute exist;
// independent ones overlap, bounded by the configured concurrency.
func TestDecompSearch_DependentOrderingAndBoundedOverlap(t *testing.T) {
	t.Parallel()
	query, subqs, refs, answers, byQuery := mixedPlan()
	f := newDecompFixture(subqs, refs, answers, byQuery, 2)
	f.store.delay = 20 * time.Millisecond
	if _, err := f.q.Search(context.Background(), query, "agent-a"); err != nil {
		t.Fatal(err)
	}
	ansSeq := map[string]int64{}
	for _, c := range f.plan.answerCalls {
		ansSeq[c.subq] = c.seq
	}
	// 1 needs 0's answer; 3 needs 1's ({2}) and 2's ({3}, unfound ⇒ ref fallback).
	if f.store.seqOf("What else did Ann Lee direct?") < ansSeq["Who directed Alpha?"] {
		t.Fatal("sub-question 1 was retrieved before sub-question 0 was answered")
	}
	s3 := f.store.seqOf("Did Gamma score a film with the composer of Beta?")
	if s3 < ansSeq["What else did Ann Lee direct?"] || s3 < ansSeq["Who composed Beta?"] {
		t.Fatal("sub-question 3 was retrieved before its dependencies were answered")
	}
	if f.store.maxIn < 2 {
		t.Fatalf("independent passes never overlapped (max in flight %d)", f.store.maxIn)
	}
	if f.store.maxIn > 2 {
		t.Fatalf("concurrency bound 2 exceeded: %d searches in flight", f.store.maxIn)
	}
}

// Every worker sees the request's context values: session, an arbitrary request
// value (stands in for lease / trace / spend metering), and the read scope.
func TestDecompSearch_WorkersCarryRequestContext(t *testing.T) {
	t.Parallel()
	query, subqs, refs, answers, byQuery := mixedPlan()
	f := newDecompFixture(subqs, refs, answers, byQuery, 4)
	ctx := domain.WithSessionID(context.Background(), "sess-42")
	ctx = context.WithValue(ctx, ctxKeyTestMarker{}, "lease-7")
	if _, err := f.q.Search(ctx, query, "agent-a"); err != nil {
		t.Fatal(err)
	}
	if len(f.store.calls) != 5 || len(f.plan.answerCalls) != 4 {
		t.Fatalf("want 5 passes and 4 extractions, got %d and %d", len(f.store.calls), len(f.plan.answerCalls))
	}
	for _, c := range f.store.calls {
		if c.session != "sess-42" || c.marker != "lease-7" || !c.hasScope {
			t.Fatalf("search %q lost request context: %+v", c.query, c)
		}
	}
	for _, c := range f.plan.answerCalls {
		if c.session != "sess-42" || c.marker != "lease-7" {
			t.Fatalf("extraction %q lost request context: %+v", c.subq, c)
		}
	}
}

// A failed sub-question pass truncates the plan exactly where the sequential loop
// broke, in both modes.
func TestDecompSearch_FailedPassMatchesSequentialBreak(t *testing.T) {
	t.Parallel()
	query, subqs, refs, answers, byQuery := mixedPlan()
	run := func(conc int) ([]string, []hopTrace) {
		f := newDecompFixture(subqs, refs, answers, byQuery, conc)
		f.store.errQueries["Who composed Beta?"] = true
		got, err := f.q.Search(WithAgenticSynthesis(context.Background()), query, "agent-a")
		if err != nil {
			t.Fatal(err)
		}
		return resultIDs(got[1:]), controlTrace(t, got).Hops
	}
	seqIDs, seqHops := run(1)
	parIDs, parHops := run(4)
	if len(seqHops) != 3 { // floor + sub-questions 0 and 1
		t.Fatalf("sequential should stop before sub-question 2, trace %+v", seqHops)
	}
	if !reflect.DeepEqual(seqIDs, parIDs) || !reflect.DeepEqual(seqHops, parHops) {
		t.Fatalf("failed-pass outcome differs:\n seq %v %+v\n par %v %+v", seqIDs, seqHops, parIDs, parHops)
	}
}

// Nothing retrieved and the first sub-question failed: the error surfaces in both
// modes (the sequential contract).
func TestDecompSearch_AllPassesFailedReturnsError(t *testing.T) {
	t.Parallel()
	for _, conc := range []int{1, 4} {
		f := newDecompFixture([]string{"Who wrote Tales?", "Which award did {1} receive?"}, nil, nil, nil, conc)
		f.store.errQueries["Which award did the author of Tales receive?"] = true
		f.store.errQueries["Who wrote Tales?"] = true
		if _, err := f.q.Search(context.Background(), "Which award did the author of Tales receive?", "agent-a"); err == nil ||
			!strings.Contains(err.Error(), "store down") {
			t.Fatalf("concurrency %d: want the pass error, got %v", conc, err)
		}
	}
}

// A cancelled request still joins every worker (the package goleak TestMain
// fails the run on a leaked goroutine).
func TestDecompSearch_CancelledRequestLeavesNoGoroutines(t *testing.T) {
	t.Parallel()
	query, subqs, refs, answers, byQuery := mixedPlan()
	f := newDecompFixture(subqs, refs, answers, byQuery, 4)
	f.store.delay = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = f.q.Search(ctx, query, "agent-a")
}

func TestSubqDeps(t *testing.T) {
	t.Parallel()
	got := subqDeps(
		[]string{"a", "b {1}", "c {1} {1}", "d {4} {3}", "e {9}"},
		[]string{"", "", "", "ref {2}"},
	)
	want := [][]int{nil, {0}, {0}, {2, 1}, nil}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("subqDeps = %v, want %v", got, want)
	}
}
