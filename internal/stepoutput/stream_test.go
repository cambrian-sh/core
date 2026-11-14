package stepoutput

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.uber.org/goleak"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// The stream starts no goroutine; this holds it to that (AGENTS.md §3.3).
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// leases is a fixed LeaseResolver.
type leases map[domain.LeaseID]domain.LeaseBinding

func (l leases) BindLease(domain.LeaseID, domain.LeaseBinding) {}
func (l leases) ResolveLease(id domain.LeaseID) (domain.LeaseBinding, bool) {
	b, ok := l[id]
	return b, ok
}

type recorder struct {
	mu     sync.Mutex
	chunks []domain.StepOutputChunk
}

func (r *recorder) sink(c domain.StepOutputChunk) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chunks = append(r.chunks, c)
}

func (r *recorder) all() []domain.StepOutputChunk {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.StepOutputChunk(nil), r.chunks...)
}

var testLeases = leases{
	"L1": {SessionID: "S1", StepIndex: 2, AgentID: "writer"},
	"L2": {SessionID: "S1", StepIndex: 3, AgentID: "pusher"},
	"L3": {SessionID: "S2", StepIndex: 0, AgentID: "writer"},
}

func newTestStream(t *testing.T, cfg config.StepOutputConfig, hooks *domain.HookChain, streams ...string) (*Stream, *recorder) {
	t.Helper()
	rec := &recorder{}
	s := New(Options{
		Config: cfg,
		Hooks:  hooks,
		Leases: testLeases,
		StreamsOutput: func(_ context.Context, agent string) bool {
			for _, a := range streams {
				if a == agent {
					return true
				}
			}
			return false
		},
		Sinks: []Sink{rec.sink},
	})
	return s, rec
}

func TestStream_GatewayChunksCarryTheBindingAndOneSequence(t *testing.T) {
	s, rec := newTestStream(t, config.StepOutputConfig{}, nil)
	s.EmitGateway("L1", domain.StepOutputText, "Hel")
	s.EmitGateway("L1", domain.StepOutputText, "")
	s.EmitGateway("L1", domain.StepOutputText, "lo")
	if got := s.EmitAgent("L1", AgentChunk{Seq: 1, Kind: domain.StepOutputProgress, Data: "half way"}); got != AgentAccepted {
		t.Fatalf("agent chunk: %v", got)
	}
	got := rec.all()
	if len(got) != 3 {
		t.Fatalf("chunks = %+v", got)
	}
	for i, c := range got {
		if c.LeaseID != "L1" || c.SessionID != "S1" || c.StepIndex != 2 || c.AgentID != "writer" {
			t.Errorf("chunk %d binding = %+v", i, c)
		}
		if c.Seq != uint64(i+1) {
			t.Errorf("chunk %d seq = %d: one kernel sequence per lease across producers", i, c.Seq)
		}
	}
	if got[0].Source != domain.StepOutputFromGateway || got[2].Source != domain.StepOutputFromAgent || got[2].Kind != domain.StepOutputProgress {
		t.Errorf("sources/kinds = %+v", got)
	}
}

func TestStream_AgentDuplicatesAndAfterFinalAreDropped(t *testing.T) {
	s, rec := newTestStream(t, config.StepOutputConfig{}, nil)
	steps := []struct {
		c    AgentChunk
		want AgentOutcome
	}{
		{AgentChunk{Seq: 1, Data: "a"}, AgentAccepted},
		{AgentChunk{Seq: 2, Data: "b"}, AgentAccepted},
		{AgentChunk{Seq: 2, Data: "b"}, AgentDuplicate}, // a retried stream
		{AgentChunk{Seq: 1, Data: "a"}, AgentDuplicate},
		{AgentChunk{Seq: 5, Data: "c", Final: true}, AgentAccepted}, // gaps are fine
		{AgentChunk{Seq: 6, Data: "late"}, AgentClosed},
	}
	for i, st := range steps {
		if got := s.EmitAgent("L2", st.c); got != st.want {
			t.Fatalf("step %d: %v, want %v", i, got, st.want)
		}
	}
	var texts []string
	for _, c := range rec.all() {
		texts = append(texts, c.Data)
	}
	if strings.Join(texts, "") != "abc" {
		t.Fatalf("delivered %q", texts)
	}
	if last := rec.all()[2]; !last.Final {
		t.Error("the final chunk must carry final")
	}
	// After the agent's final, a gateway delta for the same lease is dropped too.
	s.EmitGateway("L2", domain.StepOutputText, "x")
	if n := len(rec.all()); n != 3 {
		t.Errorf("gateway delta after final delivered (%d chunks)", n)
	}
}

func TestStream_ChunkAndStepCaps(t *testing.T) {
	s, rec := newTestStream(t, config.StepOutputConfig{MaxChunkBytes: 4, MaxStepBytes: 10}, nil)
	s.EmitGateway("L1", domain.StepOutputText, "abcdefgh") // cut to "abcd"
	s.EmitGateway("L1", domain.StepOutputText, "é中")       // 5 bytes, cut at 4 on a rune boundary => "é"
	s.EmitGateway("L1", domain.StepOutputText, "wxyz")     // 6 used, 4 left: fits exactly
	s.EmitGateway("L1", domain.StepOutputText, "more")     // nothing left: the cap is said once
	s.EmitGateway("L1", domain.StepOutputText, "again")    // dropped silently
	var data []string
	statuses := 0
	for _, c := range rec.all() {
		if c.Kind == domain.StepOutputStatus {
			statuses++
			continue
		}
		data = append(data, c.Data)
	}
	if strings.Join(data, "|") != "abcd|é|wxyz" {
		t.Fatalf("data = %q", data)
	}
	if statuses != 1 {
		t.Fatalf("the cap should be said exactly once, got %d", statuses)
	}
	// A cut never splits a rune.
	for _, d := range data {
		if !utf8.ValidString(d) {
			t.Fatalf("a cut split a rune: %q", d)
		}
	}
}

func TestStream_CapMidChunkSaysSoAndAgentFinalStillGoesOut(t *testing.T) {
	s, rec := newTestStream(t, config.StepOutputConfig{MaxChunkBytes: 8, MaxStepBytes: 8}, nil)
	if got := s.EmitAgent("L2", AgentChunk{Seq: 1, Data: "12345"}); got != AgentAccepted {
		t.Fatal(got)
	}
	if got := s.EmitAgent("L2", AgentChunk{Seq: 2, Data: "6789"}); got != AgentAccepted { // "678" fits, cap hit
		t.Fatal(got)
	}
	if got := s.EmitAgent("L2", AgentChunk{Seq: 3, Data: "x"}); got != AgentOverCap {
		t.Fatalf("over cap: %v", got)
	}
	if got := s.EmitAgent("L2", AgentChunk{Seq: 4, Data: "y", Final: true}); got != AgentOverCap {
		t.Fatalf("final over cap: %v", got)
	}
	all := rec.all()
	last := all[len(all)-1]
	if !last.Final || last.Data != "" {
		t.Fatalf("the final marker must still go out, without data: %+v", last)
	}
	var b strings.Builder
	for _, c := range all {
		if c.Kind == domain.StepOutputText {
			b.WriteString(c.Data)
		}
	}
	if b.String() != "12345678" {
		t.Fatalf("text = %q", b.String())
	}
}

func TestStream_StreamsOutputAgentSuppressesGatewayDeltas(t *testing.T) {
	s, _ := newTestStream(t, config.StepOutputConfig{}, nil, "pusher")
	if !s.GatewayForwarding(context.Background(), "L1", "") {
		t.Error("writer does not stream its own output: gateway deltas flow")
	}
	if s.GatewayForwarding(context.Background(), "L2", "") {
		t.Error("pusher declares streams_output: gateway deltas must not flow")
	}
	// An unbound lease falls back to the caller's header id.
	if s.GatewayForwarding(context.Background(), "unbound", "pusher") {
		t.Error("caller header names a streams_output agent")
	}
}

// postModelHook is a D11 hook at post_model.
type postModelHook struct{ rewrite bool }

func (h postModelHook) Spec() domain.HookSpec {
	return domain.HookSpec{Name: "redactor", Points: []domain.HookPoint{domain.HookPostModel}, MayRewrite: h.rewrite}
}
func (postModelHook) Run(context.Context, domain.HookEvent) (domain.HookResult, error) {
	return domain.HookResult{}, nil
}

// preToolHook attaches elsewhere; it must not suppress anything.
type preToolHook struct{}

func (preToolHook) Spec() domain.HookSpec {
	return domain.HookSpec{Name: "guard", Points: []domain.HookPoint{domain.HookPreTool}}
}
func (preToolHook) Run(context.Context, domain.HookEvent) (domain.HookResult, error) {
	return domain.HookResult{}, nil
}

func TestStream_PostModelHookSuppressesGatewayDeltasFailClosed(t *testing.T) {
	for _, rewrite := range []bool{true, false} {
		chain, err := domain.NewHookChain([]domain.Hook{postModelHook{rewrite: rewrite}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		s, rec := newTestStream(t, config.StepOutputConfig{}, chain)
		if s.GatewayForwarding(context.Background(), "L1", "") {
			t.Errorf("may_rewrite=%v: a post_model hook must suppress gateway deltas (it sees only the complete reply)", rewrite)
		}
		// Agent-pushed output is not model output the hook could see: unaffected.
		if got := s.EmitAgent("L2", AgentChunk{Seq: 1, Data: "pushed"}); got != AgentAccepted || len(rec.all()) != 1 {
			t.Errorf("agent push under a post_model hook: %v", got)
		}
	}
	other, err := domain.NewHookChain([]domain.Hook{preToolHook{}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := newTestStream(t, config.StepOutputConfig{}, other); !s.GatewayForwarding(context.Background(), "L1", "") {
		t.Error("a pre_tool hook must not suppress model deltas")
	}
	if s, _ := newTestStream(t, config.StepOutputConfig{GatewayDeltasDisabled: true}, nil); s.GatewayForwarding(context.Background(), "L1", "") {
		t.Error("gateway_deltas_disabled must suppress")
	}
}

// scopedPostModelHook is a post_model hook with a static scope.
type scopedPostModelHook struct {
	name             string
	agents, purposes []string
}

func (h scopedPostModelHook) Spec() domain.HookSpec {
	return domain.HookSpec{Name: h.name, Points: []domain.HookPoint{domain.HookPostModel}, MayRewrite: true,
		Agents: h.agents, Purposes: h.purposes}
}
func (scopedPostModelHook) Run(context.Context, domain.HookEvent) (domain.HookResult, error) {
	return domain.HookResult{}, nil
}

// T-20261106-post-model-hooks-cannot-be-scoped-by-agent-or: suppression honours the
// hook's static scope. A planner-only hook does not suppress an agent step's
// deltas; an agent-scoped hook suppresses only that agent's steps; an unscoped one
// still suppresses every step (fail-closed).
func TestStream_SuppressionHonoursHookScope(t *testing.T) {
	cases := []struct {
		name                string
		hook                scopedPostModelHook
		writerFwd, otherFwd bool
	}{
		{"planner only", scopedPostModelHook{name: "p", purposes: []string{"planner"}}, true, true},
		{"router glob", scopedPostModelHook{name: "r", purposes: []string{"rout*", "verifier"}}, true, true},
		{"agent steps", scopedPostModelHook{name: "s", purposes: []string{"agent_step"}}, false, false},
		{"writer only", scopedPostModelHook{name: "w", agents: []string{"writ*"}}, false, true},
		{"unscoped", scopedPostModelHook{name: "u"}, false, false},
	}
	for _, c := range cases {
		chain, err := domain.NewHookChain([]domain.Hook{c.hook}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		s, _ := newTestStream(t, config.StepOutputConfig{}, chain)
		if got := s.GatewayForwarding(context.Background(), "L1", "writer"); got != c.writerFwd {
			t.Errorf("%s: writer step forwarding = %v, want %v", c.name, got, c.writerFwd)
		}
		if got := s.GatewayForwarding(context.Background(), "unbound", "other"); got != c.otherFwd {
			t.Errorf("%s: other agent forwarding = %v, want %v", c.name, got, c.otherFwd)
		}
	}
	// The lease's bound agent counts even when the header names another: matching
	// either suppresses (fail closed).
	chain, err := domain.NewHookChain([]domain.Hook{scopedPostModelHook{name: "w", agents: []string{"writer"}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := newTestStream(t, config.StepOutputConfig{}, chain)
	if s.GatewayForwarding(context.Background(), "L1", "impostor") {
		t.Error("the lease is bound to writer: its scoped hook must suppress")
	}
}

func TestStream_SessionSubscribersSeeOnlyTheirSession(t *testing.T) {
	s, _ := newTestStream(t, config.StepOutputConfig{}, nil)
	var mu sync.Mutex
	var got []string
	cancel := s.SubscribeSession("S1", func(c domain.StepOutputChunk) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, fmt.Sprintf("%s:%s", c.LeaseID, c.Data))
	})
	s.EmitGateway("L1", domain.StepOutputText, "one")   // S1
	s.EmitGateway("L3", domain.StepOutputText, "other") // S2
	s.EmitAgent("L2", AgentChunk{Seq: 1, Data: "two"})  // S1
	cancel()
	s.EmitGateway("L1", domain.StepOutputText, "after")
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != "L1:one,L2:two" {
		t.Fatalf("subscriber saw %v", got)
	}
}

func TestStream_EndLeaseForgetsAndPruneBoundsTheMap(t *testing.T) {
	now := time.Unix(0, 0)
	s := New(Options{Now: func() time.Time { return now }})
	s.EmitGateway("A", domain.StepOutputText, "x")
	s.EndLease("A")
	if s.Tracked() != 0 {
		t.Fatalf("EndLease kept state")
	}
	// A new lease after EndLease starts its sequence afresh.
	for i := 0; i < maxTrackedLeases+10; i++ {
		s.EmitGateway(domain.LeaseID(fmt.Sprintf("l%d", i)), domain.StepOutputText, "x")
		now = now.Add(time.Millisecond)
	}
	if n := s.Tracked(); n > maxTrackedLeases {
		t.Fatalf("tracked %d leases, bound is %d", n, maxTrackedLeases)
	}
}

func TestStream_NilStreamIsInert(t *testing.T) {
	var s *Stream
	s.EmitGateway("L", domain.StepOutputText, "x")
	s.EndLease("L")
	s.SubscribeSession("S", func(domain.StepOutputChunk) {})()
	if s.GatewayForwarding(context.Background(), "L", "a") {
		t.Error("a nil stream forwards nothing")
	}
}

// ENG-1467 item 7: the streams_output lookup runs on the gateway call's ctx
// (it used to read the registry under context.Background()).
func TestStream_StreamsOutputLookupGetsTheCallersContext(t *testing.T) {
	type key struct{}
	var got context.Context
	s := New(Options{
		Config: config.StepOutputConfig{},
		Leases: testLeases,
		StreamsOutput: func(ctx context.Context, _ string) bool {
			got = ctx
			return false
		},
	})
	s.GatewayForwarding(context.WithValue(context.Background(), key{}, "caller"), "unbound", "agent-x")
	if got == nil || got.Value(key{}) != "caller" {
		t.Fatal("streams_output lookup did not receive the caller's ctx")
	}
}
