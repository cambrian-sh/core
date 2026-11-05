package executer

import (
	"context"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/dispatch"
	"github.com/cambrian-sh/core/internal/supervision/aggregator"
)

// --- fakes for the dispatch → TaskEvent → aggregator → profile loop ----------

type oneCandidateGatekeeper struct{ agent domain.AgentDefinition }

func (g oneCandidateGatekeeper) FindCandidates(context.Context, *domain.DispatchTask) ([]domain.ScoredCandidate, error) {
	return []domain.ScoredCandidate{{Agent: g.agent, Score: 0.7}}, nil
}

func (g oneCandidateGatekeeper) FindModelCandidates(context.Context, []string) ([]domain.ScoredCandidate, error) {
	return nil, nil
}

// echoingCaller answers like an agent, and tries to spoof the winner stamp — the
// Dispatcher must overwrite it.
type echoingCaller struct{}

func (echoingCaller) CallAgent(_ context.Context, agentID string, _ *domain.Handoff, _ string) (*domain.Handoff, error) {
	return &domain.Handoff{
		FromAgent: agentID,
		Payload:   &domain.Payload{Data: []byte("answer")},
		Context:   map[string]string{domain.HandoffKeyWinnerSourceHash: "spoofed"},
	}, nil
}

// eventStore is a TaskEventWriter + aggregator.TaskEventReader keyed like the bbolt
// adapter: agentID:sourceHash.
type eventStore struct {
	mu     sync.Mutex
	events map[string]domain.TaskEvent
}

func (s *eventStore) WriteTaskEvent(e domain.TaskEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events == nil {
		s.events = map[string]domain.TaskEvent{}
	}
	s.events[e.TaskID] = e
	return nil
}

func (s *eventStore) ReadTaskEvents(agentID, sourceHash string) ([]domain.TaskEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.TaskEvent
	for _, e := range s.events {
		if e.AgentID == agentID && e.SourceHash == sourceHash {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *eventStore) ReadAllAgentIDs() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var keys []string
	for _, e := range s.events {
		k := e.AgentID + ":" + e.SourceHash
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// markVerified is the VerificationWorker's in-place update (read, set score, write).
func (s *eventStore) markVerified(taskID string, score float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.events[taskID]
	e.VerifierScore = score
	e.Verified = true
	s.events[taskID] = e
}

type profileMap struct {
	mu sync.Mutex
	m  map[string]domain.AgentProfile
}

func (p *profileMap) SaveProfile(_ context.Context, agentID, sourceHash string, _ []float32, profile domain.AgentProfile) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]domain.AgentProfile{}
	}
	p.m[agentID+":"+sourceHash] = profile
	return nil
}

func (p *profileMap) GetProfile(_ context.Context, agentID, sourceHash string) (*domain.AgentProfile, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.m[agentID+":"+sourceHash]; ok {
		return &v, nil
	}
	return nil, nil
}

// T-20261104-routing-never-learns-from-real-outcomes-dag / ADR-0139 D6: a step the
// Dispatcher routed to a hashed agent must move the profile the Gatekeeper reads —
// GetProfile(agent.ID, agent.SourceHash) — not the orphan `profile:<agent>:` row.
func TestDispatchedStepOutcomeMovesProfileUnderRealSourceHash(t *testing.T) {
	agent := domain.AgentDefinition{ID: "coder", SourceHash: "h-abc123"}
	d := &dispatch.Dispatcher{
		Gatekeeper: oneCandidateGatekeeper{agent: agent},
		Caller:     echoingCaller{},
		ExecCfg:    config.DefaultConfig().Execution,
	}
	events := &eventStore{}
	exec := &DAGExecutor{EventWriter: events}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "write the function"}}}
	stepFn := func(ctx context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		res, err := d.Execute(ctx, &domain.DispatchTask{ID: "task-0", Description: plan.Steps[i].Query}, h)
		if err != nil {
			return nil, err
		}
		return res.Handoff, nil
	}

	final, err := exec.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, leaked := final[domain.HandoffKeyWinnerSourceHash]; leaked {
		t.Fatal("winner stamp must be consumed, not merged into the plan context")
	}

	events.mu.Lock()
	if len(events.events) != 1 {
		events.mu.Unlock()
		t.Fatalf("want 1 TaskEvent, got %d", len(events.events))
	}
	var taskID string
	for id, e := range events.events {
		taskID = id
		if e.SourceHash != agent.SourceHash {
			events.mu.Unlock()
			t.Fatalf("TaskEvent SourceHash = %q, want the dispatched definition's %q", e.SourceHash, agent.SourceHash)
		}
	}
	events.mu.Unlock()

	profiles := &profileMap{}
	agg := aggregator.New(events, profiles, aggregator.AggregatorConfig{EWMAAlpha: 0.3, LatencyWindow: 10})

	// Unverified: the outcome is neutral, not a success (ADR-0139 D6).
	if err := agg.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	p, _ := profiles.GetProfile(context.Background(), agent.ID, agent.SourceHash)
	if p == nil {
		t.Fatal("no profile under the agent's real hash: the outcome went to an orphan row")
	}
	if p.SuccessRate != 0.5 {
		t.Fatalf("unverified-only SuccessRate = %v, want neutral 0.5", p.SuccessRate)
	}
	if orphan, _ := profiles.GetProfile(context.Background(), agent.ID, ""); orphan != nil {
		t.Fatalf("outcome also wrote the empty-hash orphan profile: %+v", orphan)
	}

	// Verified failure moves the same profile down.
	events.markVerified(taskID, 0.1)
	if err := agg.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	p, _ = profiles.GetProfile(context.Background(), agent.ID, agent.SourceHash)
	if p == nil || p.SuccessRate != 0 {
		t.Fatalf("verified-failure profile = %+v, want SuccessRate 0 under %q", p, agent.SourceHash)
	}
}

// A response whose stamp names a different agent than the one that answered (a
// runner-up fallback that bypassed the Dispatcher) must not borrow that hash.
func TestDAGExecutor_WinnerHashIgnoredWhenAgentDiffers(t *testing.T) {
	mw := &mockEventWriter{}
	d := &DAGExecutor{EventWriter: mw}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "q"}}}
	stepFn := func(context.Context, int, *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{FromAgent: "runner-up", Payload: &domain.Payload{Data: []byte("x")}, Context: map[string]string{
			domain.HandoffKeyWinnerAgentID:    "winner",
			domain.HandoffKeyWinnerSourceHash: "h-winner",
		}}, nil
	}
	if _, err := d.Execute(t.Context(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	mw.mu.Lock()
	defer mw.mu.Unlock()
	if len(mw.events) != 1 || mw.events[0].SourceHash != "" {
		t.Fatalf("events = %+v, want one event with empty SourceHash", mw.events)
	}
}
