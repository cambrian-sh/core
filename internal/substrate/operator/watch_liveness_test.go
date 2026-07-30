package operator

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

type stubStreams struct {
	state    map[string]string
	refcount map[string]int
}

func (s stubStreams) StreamState(id string) string {
	if v, ok := s.state[id]; ok {
		return v
	}
	return domain.StreamUnknown
}
func (s stubStreams) StreamRefcount(id string) int {
	if v, ok := s.refcount[id]; ok {
		return v
	}
	return -1
}

type stubFires struct{ fires []domain.WatchFire }

func (s stubFires) RecentFires(string, int) []domain.WatchFire { return s.fires }

type stubBudget struct{ b domain.ReactivePlaneBudget }

func (s stubBudget) PlaneBudget() domain.ReactivePlaneBudget { return s.b }

type stubDeadLetters struct{ n int }

func (s stubDeadLetters) ListDeadLetters(int) ([]domain.ReactiveDeadLetter, error) {
	return make([]domain.ReactiveDeadLetter, s.n), nil
}

// Without a stream registry the state must be "unknown", NOT "live".
//
// This is the whole point of the field. A watch whose feeding daemon has crashed
// still reads active, so a state that defaults to healthy would confirm the
// operator's wrong assumption instead of correcting it.
func TestWatchLiveness_NoRegistryReportsUnknownNotLive(t *testing.T) {
	op := toWatchConfigOp(domain.WatchConfig{ID: "w1", Source: domain.WatchSource{StreamID: "s1"}})
	(&Service{}).enrichWatchConfig(op)

	if op.GetSourceStreamState() != domain.StreamUnknown {
		t.Fatalf("state = %q, want %q", op.GetSourceStreamState(), domain.StreamUnknown)
	}
	if op.GetSourceStreamRefcount() != -1 {
		t.Fatalf("refcount = %d, want -1 (not counted), which is distinct from 0", op.GetSourceStreamRefcount())
	}
}

func TestWatchLiveness_ReportsUnavailableStreamAndRefcount(t *testing.T) {
	s := &Service{}
	s.SetWatchLiveness(stubStreams{
		state:    map[string]string{"s1": domain.StreamUnavailable},
		refcount: map[string]int{"s1": 2},
	}, nil, nil)

	op := toWatchConfigOp(domain.WatchConfig{ID: "w1", Source: domain.WatchSource{StreamID: "s1"}})
	s.enrichWatchConfig(op)

	if op.GetSourceStreamState() != domain.StreamUnavailable {
		t.Fatalf("state = %q, want %q", op.GetSourceStreamState(), domain.StreamUnavailable)
	}
	// The refcount is the follow-up question: fixing this daemon un-breaks 2
	// rules, not just the one being looked at.
	if op.GetSourceStreamRefcount() != 2 {
		t.Fatalf("refcount = %d, want 2", op.GetSourceStreamRefcount())
	}
}

// "suppressed" must survive as its own outcome. It is a rule WORKING, and a
// console that cannot distinguish it from a failure trains an operator to loosen
// a boundary that is doing its job.
func TestWatchLiveness_FireHistoryDistinguishesSuppressedFromFailed(t *testing.T) {
	now := time.Now()
	s := &Service{}
	s.SetWatchLiveness(nil, stubFires{fires: []domain.WatchFire{
		{At: now, Outcome: domain.FireSuppressed},
		{At: now, Outcome: domain.FireFailed, Error: "action timed out"},
		{At: now, Outcome: domain.FireFired, LatencyMs: 12},
	}}, nil)

	op := toWatchConfigOp(domain.WatchConfig{ID: "w1"})
	s.enrichWatchConfig(op)

	if len(op.GetLastFires()) != 3 {
		t.Fatalf("got %d fires, want 3", len(op.GetLastFires()))
	}
	got := op.GetLastFires()
	if got[0].GetOutcome() != domain.FireSuppressed {
		t.Fatalf("outcome[0] = %q, want %q", got[0].GetOutcome(), domain.FireSuppressed)
	}
	if got[1].GetOutcome() != domain.FireFailed || got[1].GetError() == "" {
		t.Fatalf("a failure must carry its reason: %+v", got[1])
	}
	if got[2].GetLatencyMs() != 12 {
		t.Fatalf("latency = %d, want 12", got[2].GetLatencyMs())
	}
}

// A REACT-06 schedule watch must round-trip its catch-up policy. It did not
// before contract 0074: toWatchConfigOp dropped the field, so a watch edited
// through the console silently reverted to "skip" and stopped catching up after
// a restart.
func TestWatchConfig_MissedFirePolicyRoundTrips(t *testing.T) {
	op := toWatchConfigOp(domain.WatchConfig{ID: "w1", MissedFirePolicy: "fire_once"})

	if op.GetMissedFirePolicy() != "fire_once" {
		t.Fatalf("missed_fire_policy = %q, want fire_once — a schedule watch silently loses its catch-up otherwise", op.GetMissedFirePolicy())
	}
	back := fromWatchConfigOp(op)
	if back.MissedFirePolicy != "fire_once" {
		t.Fatalf("round-trip lost the policy: %q", back.MissedFirePolicy)
	}
}

// ── plane budget ─────────────────────────────────────────────────────────────

func TestGetReactiveBudget_UnconfiguredIsUnimplemented(t *testing.T) {
	if _, err := (&Service{}).GetReactiveBudget(context.Background(), &pb.GetReactiveBudgetOpRequest{}); codeOf(err) != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", codeOf(err))
	}
}

// A kernel with dead letters but no budget reader must still serve the count —
// and report every budget counter as -1 rather than 0, so the console says "not
// reported" instead of drawing an idle plane.
func TestGetReactiveBudget_UntrackedCountersAreMinusOneNotZero(t *testing.T) {
	s := &Service{}
	s.SetDeadLetterReader(stubDeadLetters{n: 3})

	resp, err := s.GetReactiveBudget(context.Background(), &pb.GetReactiveBudgetOpRequest{})
	if err != nil {
		t.Fatalf("GetReactiveBudget: %v", err)
	}
	if resp.GetDeadLetterCount() != 3 {
		t.Fatalf("dead_letter_count = %d, want 3", resp.GetDeadLetterCount())
	}
	if resp.GetBudget().GetPlansStartedThisHour() != -1 {
		t.Fatalf("plans_started = %d, want -1 — 0 would claim the plane is idle", resp.GetBudget().GetPlansStartedThisHour())
	}
}

func TestGetReactiveBudget_ReportsTotalsAgainstCaps(t *testing.T) {
	started := time.Now().Add(-20 * time.Minute)
	s := &Service{}
	s.SetDeadLetterReader(stubDeadLetters{n: 0})
	s.SetWatchLiveness(nil, nil, stubBudget{b: domain.ReactivePlaneBudget{
		GateEvaluationsThisHour: 812, GateEvaluationsCap: 1000,
		PlansStartedThisHour: 4, PlansStartedCap: 10,
		SignalsShedThisHour: 2, WindowStarted: started,
	}})

	resp, err := s.GetReactiveBudget(context.Background(), &pb.GetReactiveBudgetOpRequest{})
	if err != nil {
		t.Fatalf("GetReactiveBudget: %v", err)
	}
	b := resp.GetBudget()
	if b.GetGateEvaluationsThisHour() != 812 || b.GetGateEvaluationsCap() != 1000 {
		t.Fatalf("gate evals = %d/%d, want 812/1000", b.GetGateEvaluationsThisHour(), b.GetGateEvaluationsCap())
	}
	// The window start is what lets a console say "resets in 12 minutes" rather
	// than leaving an operator to guess whether 812/1000 is about to reset or
	// about to bite.
	if b.GetWindowStartedUnixMs() != started.UnixMilli() {
		t.Fatalf("window_started = %d, want %d", b.GetWindowStartedUnixMs(), started.UnixMilli())
	}
}

// ── multi-action watches (contract 0076) ─────────────────────────────────────

// A watch stored before 0076 has only the singular Action. EffectiveActions must
// keep returning it, or every existing watch stops firing on upgrade.
func TestEffectiveActions_SingularStillWorks(t *testing.T) {
	w := domain.WatchConfig{Action: domain.WatchAction{Type: "emit_event"}}

	arms := w.EffectiveActions()
	if len(arms) != 1 || arms[0].Type != "emit_event" {
		t.Fatalf("arms = %+v, want the single stored action", arms)
	}
}

func TestEffectiveActions_ArmsRunFirstArmFirst(t *testing.T) {
	w := domain.WatchConfig{
		Action:  domain.WatchAction{Type: "emit_event"},
		Actions: []domain.WatchAction{{Type: "dispatch_agent"}, {Type: "start_plan"}},
	}

	arms := w.EffectiveActions()
	if len(arms) != 3 {
		t.Fatalf("got %d arms, want 3", len(arms))
	}
	// Order is the contract: a builder draws arms top-down and an operator expects
	// them to run that way.
	want := []string{"emit_event", "dispatch_agent", "start_plan"}
	for i, w := range want {
		if arms[i].Type != w {
			t.Fatalf("arm %d = %q, want %q", i, arms[i].Type, w)
		}
	}
}

// A client that only knows the plural form sends an empty Action. That must not
// silently drop every arm.
func TestEffectiveActions_PluralOnlyIsHonoured(t *testing.T) {
	w := domain.WatchConfig{Actions: []domain.WatchAction{{Type: "emit_event"}}}

	arms := w.EffectiveActions()
	if len(arms) != 1 || arms[0].Type != "emit_event" {
		t.Fatalf("arms = %+v, want the plural-only action", arms)
	}
}

// Arms must survive the wire in both directions, or the builder draws arms the
// engine never receives — the exact defect the singular field caused.
func TestWatchConfig_ExtraArmsRoundTrip(t *testing.T) {
	in := domain.WatchConfig{
		ID:      "w1",
		Action:  domain.WatchAction{Type: "emit_event", Target: "alerts"},
		Actions: []domain.WatchAction{{Type: "dispatch_agent", TargetType: "agent_id", Target: "oncall"}},
	}

	op := toWatchConfigOp(in)
	if len(op.GetActions()) != 1 {
		t.Fatalf("outbound arms = %d, want 1", len(op.GetActions()))
	}
	back := fromWatchConfigOp(op)
	if len(back.EffectiveActions()) != 2 {
		t.Fatalf("round-tripped arms = %d, want 2", len(back.EffectiveActions()))
	}
	if back.Actions[0].Target != "oncall" || back.Actions[0].TargetType != "agent_id" {
		t.Fatalf("arm 2 lost its target: %+v", back.Actions[0])
	}
}
