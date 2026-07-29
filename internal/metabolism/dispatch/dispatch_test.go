package dispatch

import (
	"context"
	"errors"
	"math/rand"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// --- fakes -------------------------------------------------------------------

type fakeGatekeeper struct {
	candidates []domain.ScoredCandidate
	err        error
	models     []domain.ScoredCandidate
	lastTask   *domain.AuctionTask
}

func (f *fakeGatekeeper) FindCandidates(_ context.Context, task *domain.AuctionTask) ([]domain.ScoredCandidate, error) {
	f.lastTask = task
	return f.candidates, f.err
}

func (f *fakeGatekeeper) FindModelCandidates(_ context.Context, _ []string) ([]domain.ScoredCandidate, error) {
	return f.models, nil
}

type fakeCaller struct {
	called   []string
	err      error
	response *domain.Handoff
}

func (f *fakeCaller) CallAgent(_ context.Context, agentID string, _ *domain.Handoff, _ string) (*domain.Handoff, error) {
	f.called = append(f.called, agentID)
	if f.err != nil {
		return nil, f.err
	}
	if f.response != nil {
		return f.response, nil
	}
	return &domain.Handoff{FromAgent: agentID}, nil
}

type fakeProfiles struct{ byID map[string]*domain.AgentProfile }

func (f *fakeProfiles) GetProfile(_ context.Context, agentID, _ string) (*domain.AgentProfile, error) {
	p, ok := f.byID[agentID]
	if !ok {
		return nil, errors.New("no profile")
	}
	return p, nil
}

type capturingBus struct{ events []domain.AuctionEventPayload }

func (c *capturingBus) Subscribe(string, domain.EventHandler) {}

func (c *capturingBus) Publish(ev domain.DomainEvent) error {
	if p, ok := ev.(domain.AuctionEventPayload); ok {
		c.events = append(c.events, p)
	}
	return nil
}

func cand(id string, score float64) domain.ScoredCandidate {
	return domain.ScoredCandidate{Agent: domain.AgentDefinition{ID: id}, Score: score}
}

func newDispatcher(gk *fakeGatekeeper, caller *fakeCaller) *Dispatcher {
	return &Dispatcher{
		Gatekeeper: gk,
		Caller:     caller,
		ExecCfg:    config.DefaultConfig().Execution,
	}
}

// --- tests -------------------------------------------------------------------

// The core claim of ADR-0100: selection costs ZERO agent round trips. Only the
// winner is ever called — the losers are never contacted, and therefore never booted.
func TestExecute_CallsOnlyTheWinner(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{
		cand("best", 0.9), cand("mid", 0.5), cand("worst", 0.1),
	}}
	caller := &fakeCaller{}
	d := newDispatcher(gk, caller)

	res, err := d.Execute(context.Background(), &domain.AuctionTask{ID: "t1"}, &domain.Handoff{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(caller.called) != 1 {
		t.Fatalf("expected exactly 1 agent call (the winner), got %d: %v", len(caller.called), caller.called)
	}
	if caller.called[0] != "best" {
		t.Errorf("winner = %q, want %q", caller.called[0], "best")
	}
	if res.Handoff.FromAgent != "best" {
		t.Errorf("result handoff from %q, want %q", res.Handoff.FromAgent, "best")
	}
}

func TestExecute_ArgmaxPicksHighestMerit(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("a", 0.8), cand("b", 0.3)}}
	caller := &fakeCaller{}
	d := newDispatcher(gk, caller)

	if _, err := d.Execute(context.Background(), &domain.AuctionTask{ID: "t"}, &domain.Handoff{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if caller.called[0] != "a" {
		t.Errorf("winner = %q, want a", caller.called[0])
	}
}

func TestExecute_NoEligibleCandidatesFailsLoudly(t *testing.T) {
	gk := &fakeGatekeeper{candidates: nil}
	caller := &fakeCaller{}
	bus := &capturingBus{}
	d := newDispatcher(gk, caller)
	d.EventBus = bus

	_, err := d.Execute(context.Background(),
		&domain.AuctionTask{ID: "t", RequiredCapabilities: []string{"pdf-extract"}}, &domain.Handoff{})
	if err == nil {
		t.Fatal("expected an error when no agent is eligible")
	}
	// D5: the failure must name the unmatched requirement, not fail silently.
	if got := err.Error(); got == "" || !contains(got, "pdf-extract") {
		t.Errorf("error should name the unsatisfied capability, got: %v", err)
	}
	if len(caller.called) != 0 {
		t.Errorf("no agent should be called when nothing is eligible, got %v", caller.called)
	}
	if len(bus.events) == 0 || bus.events[len(bus.events)-1].Status != "failed" {
		t.Errorf("expected a terminal 'failed' event, got %+v", bus.events)
	}
}

// The orchestration suite scores routing accuracy off this event. If dispatch
// stops emitting it, the P0 A/B cannot be measured at all.
func TestExecute_EmitsWinnerAndSlateForBenchmarking(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("a", 0.9), cand("b", 0.4)}}
	bus := &capturingBus{}
	d := newDispatcher(gk, &fakeCaller{})
	d.EventBus = bus

	if _, err := d.Execute(context.Background(), &domain.AuctionTask{ID: "t"}, &domain.Handoff{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var completed *domain.AuctionEventPayload
	for i := range bus.events {
		if bus.events[i].Status == "completed" {
			completed = &bus.events[i]
		}
	}
	if completed == nil {
		t.Fatal("no 'completed' auction event emitted — the benchmark suite would see nothing")
	}
	if completed.WinnerID != "a" {
		t.Errorf("WinnerID = %q, want a", completed.WinnerID)
	}
	if len(completed.Bids) != 2 {
		t.Errorf("expected the full candidate slate (2), got %d", len(completed.Bids))
	}
	if got := completed.WinnerMargin; got < 0.49 || got > 0.51 {
		t.Errorf("WinnerMargin = %v, want ~0.5", got)
	}
}

// D4: a cheap, verified step takes the cheapest competent agent, not the best one.
func TestSelectWinner_CheapestCompetentOnCheapVerifiedStep(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{
		cand("expensive-best", 0.95),
		cand("cheap-ok", 0.60),
	}}
	caller := &fakeCaller{}
	d := newDispatcher(gk, caller)
	d.Profiles = &fakeProfiles{byID: map[string]*domain.AgentProfile{
		"expensive-best": {ModelMetrics: &domain.ModelMetrics{AvgCostPerTask: 0.05}},
		"cheap-ok":       {ModelMetrics: &domain.ModelMetrics{AvgCostPerTask: 0.001}},
	}}
	d.ExecCfg.DispatchCheapEnergyMax = 10
	d.ExecCfg.DispatchMeritFloor = 0.5

	task := &domain.AuctionTask{ID: "t", MaxEnergy: 5, CheckpointAfter: true}
	if _, err := d.Execute(context.Background(), task, &domain.Handoff{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if caller.called[0] != "cheap-ok" {
		t.Errorf("winner = %q, want cheap-ok (cheap + verified ⇒ cheapest competent)", caller.called[0])
	}
}

// The same slate on an UNVERIFIED step must take argmax — without a checkpoint
// there is nothing to catch a bad cheap answer.
func TestSelectWinner_ArgmaxWhenStepIsNotVerified(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{
		cand("expensive-best", 0.95),
		cand("cheap-ok", 0.60),
	}}
	caller := &fakeCaller{}
	d := newDispatcher(gk, caller)
	d.Profiles = &fakeProfiles{byID: map[string]*domain.AgentProfile{
		"expensive-best": {ModelMetrics: &domain.ModelMetrics{AvgCostPerTask: 0.05}},
		"cheap-ok":       {ModelMetrics: &domain.ModelMetrics{AvgCostPerTask: 0.001}},
	}}
	d.ExecCfg.DispatchCheapEnergyMax = 10

	task := &domain.AuctionTask{ID: "t", MaxEnergy: 5, CheckpointAfter: false}
	if _, err := d.Execute(context.Background(), task, &domain.Handoff{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if caller.called[0] != "expensive-best" {
		t.Errorf("winner = %q, want expensive-best (unverified ⇒ argmax)", caller.called[0])
	}
}

// A step with no declared budget is not a claim of cheapness.
func TestSelectWinner_ZeroEnergyIsNotCheap(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("best", 0.95), cand("cheap", 0.60)}}
	caller := &fakeCaller{}
	d := newDispatcher(gk, caller)
	d.Profiles = &fakeProfiles{byID: map[string]*domain.AgentProfile{
		"best":  {ModelMetrics: &domain.ModelMetrics{AvgCostPerTask: 0.05}},
		"cheap": {ModelMetrics: &domain.ModelMetrics{AvgCostPerTask: 0.001}},
	}}
	d.ExecCfg.DispatchCheapEnergyMax = 10

	task := &domain.AuctionTask{ID: "t", MaxEnergy: 0, CheckpointAfter: true}
	if _, err := d.Execute(context.Background(), task, &domain.Handoff{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if caller.called[0] != "best" {
		t.Errorf("winner = %q, want best (no budget ⇒ argmax)", caller.called[0])
	}
}

// "Cheapest" must never mean "worst": a cheap agent below the merit floor loses.
func TestCheapestCompetent_RespectsMeritFloor(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("good", 0.9), cand("cheap-bad", 0.2)}}
	caller := &fakeCaller{}
	d := newDispatcher(gk, caller)
	d.Profiles = &fakeProfiles{byID: map[string]*domain.AgentProfile{
		"good":      {ModelMetrics: &domain.ModelMetrics{AvgCostPerTask: 0.05}},
		"cheap-bad": {ModelMetrics: &domain.ModelMetrics{AvgCostPerTask: 0.0001}},
	}}
	d.ExecCfg.DispatchCheapEnergyMax = 10
	d.ExecCfg.DispatchMeritFloor = 0.5

	task := &domain.AuctionTask{ID: "t", MaxEnergy: 1, CheckpointAfter: true}
	if _, err := d.Execute(context.Background(), task, &domain.Handoff{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if caller.called[0] != "good" {
		t.Errorf("winner = %q, want good (cheap-bad is below the merit floor)", caller.called[0])
	}
}

// Cold start: no profile data ⇒ fall back to argmax rather than guessing.
func TestCheapestCompetent_FallsBackToArgmaxWithoutProfiles(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("best", 0.9), cand("other", 0.6)}}
	caller := &fakeCaller{}
	d := newDispatcher(gk, caller)
	d.Profiles = nil
	d.ExecCfg.DispatchCheapEnergyMax = 10

	task := &domain.AuctionTask{ID: "t", MaxEnergy: 1, CheckpointAfter: true}
	if _, err := d.Execute(context.Background(), task, &domain.Handoff{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if caller.called[0] != "best" {
		t.Errorf("winner = %q, want best (no profiles ⇒ argmax)", caller.called[0])
	}
}

func TestExecute_ExplorationCanPickANonTopCandidate(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("a", 0.9), cand("b", 0.5), cand("c", 0.1)}}
	caller := &fakeCaller{}
	d := newDispatcher(gk, caller)
	d.ExplorationRate = 1.0 // always explore
	d.Rand = rand.New(rand.NewSource(7))

	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		caller.called = nil
		if _, err := d.Execute(context.Background(), &domain.AuctionTask{ID: "t"}, &domain.Handoff{}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		seen[caller.called[0]] = true
	}
	if len(seen) < 2 {
		t.Errorf("exploration never left the top candidate; saw only %v", seen)
	}
}

func TestExecute_RunnerUpsExcludeWinner(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("a", 0.9), cand("b", 0.5), cand("c", 0.1)}}
	d := newDispatcher(gk, &fakeCaller{})

	res, err := d.Execute(context.Background(), &domain.AuctionTask{ID: "t"}, &domain.Handoff{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.RunnerUps) != 2 {
		t.Fatalf("expected 2 runner-ups, got %d", len(res.RunnerUps))
	}
	for _, r := range res.RunnerUps {
		if r.Agent.ID == "a" {
			t.Error("winner must not appear in RunnerUps — the fallback loop would re-run it")
		}
	}
}

// A failed call still returns runner-ups so the inter-step fallback loop can act.
func TestExecute_CallFailureStillReturnsRunnerUps(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("a", 0.9), cand("b", 0.5)}}
	caller := &fakeCaller{err: errors.New("agent exploded")}
	d := newDispatcher(gk, caller)

	res, err := d.Execute(context.Background(), &domain.AuctionTask{ID: "t"}, &domain.Handoff{})
	if err == nil {
		t.Fatal("expected the call error to propagate")
	}
	if res == nil || len(res.RunnerUps) != 1 {
		t.Fatalf("expected runner-ups on failure so fallback can proceed, got %+v", res)
	}
}

func TestExecute_GatekeeperErrorPropagates(t *testing.T) {
	gk := &fakeGatekeeper{err: errors.New("registry down")}
	d := newDispatcher(gk, &fakeCaller{})

	if _, err := d.Execute(context.Background(), &domain.AuctionTask{ID: "t"}, &domain.Handoff{}); err == nil {
		t.Fatal("expected the gatekeeper error to propagate")
	}
}

// Dispatcher must remain a drop-in for the DAG call site.
func TestDispatcherSatisfiesAuctioneerInterface(t *testing.T) {
	var _ domain.Auctioneer = (*Dispatcher)(nil)
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
