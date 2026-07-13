package network

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

type fakeScoutAuctioneer struct {
	resp        *domain.Handoff
	err         error
	calledAgent string
	gotHandoff  *domain.Handoff
}

func (f *fakeScoutAuctioneer) Execute(_ context.Context, _ *domain.AuctionTask, _ *domain.Handoff) (*domain.AuctionResult, error) {
	return nil, nil
}

func (f *fakeScoutAuctioneer) CallAgent(_ context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	f.calledAgent = agentID
	f.gotHandoff = h
	return f.resp, f.err
}

// fakeScoutGateway captures the StepAllocation it was asked to acquire.
type fakeScoutGateway struct {
	acquiredSA domain.StepAllocation
	tokenID    string
	completed  string
}

func (g *fakeScoutGateway) Acquire(_ context.Context, sa domain.StepAllocation, _ int, _ time.Duration) (string, error) {
	g.acquiredSA = sa
	return g.tokenID, nil
}
func (g *fakeScoutGateway) Complete(_ context.Context, id string) (llm.TokenUsage, error) {
	g.completed = id
	return llm.TokenUsage{}, nil
}
func (g *fakeScoutGateway) EvictExpired() {}
func (g *fakeScoutGateway) StreamChunks(_ context.Context, _, _ string, _ domain.GenerateOptions, _ chan<- domain.StreamChunk) error {
	return nil
}

// ADR-0051: the dispatcher deliberately allocates Scout's model via a managed session and
// threads the token into the handoff — not the gateway's default-model fallback.
func TestAgentScoutDispatcher_AllocatesModelSession(t *testing.T) {
	gw := &fakeScoutGateway{tokenID: "sess-scout-1"}
	f := &fakeScoutAuctioneer{resp: &domain.Handoff{Payload: &domain.Payload{Data: []byte(`{"entities":[]}`)}}}
	d := &AgentScoutDispatcher{Auctioneer: f, ScoutAgentID: "scout_agent", Gateway: gw, ScoutModel: "llm:cheap"}

	d.Discover(context.Background(), "continue the helicopter folder")

	if gw.acquiredSA.Winner.ID != "llm:cheap" {
		t.Errorf("Scout must allocate the configured model; got winner %q", gw.acquiredSA.Winner.ID)
	}
	if f.gotHandoff == nil || f.gotHandoff.Context["_session_token_id"] != "sess-scout-1" {
		t.Errorf("the acquired session token must be threaded into the handoff; got %v", f.gotHandoff.Context)
	}
	if gw.completed != "sess-scout-1" {
		t.Errorf("the session must be released (Complete) after dispatch; got %q", gw.completed)
	}
}

// ADR-0051: the dispatcher invokes the scout_agent, parses its structured report, and always
// merges deterministic env grounding.
func TestAgentScoutDispatcher_ParsesReportAndGroundsEnv(t *testing.T) {
	reportJSON := `{"entities":[{"kind":"dir","id":"helicopter","exists":true,"summary":"7 missing"}],"interpretation":"7 remain","unobserved":["api:x.com"]}`
	f := &fakeScoutAuctioneer{resp: &domain.Handoff{Payload: &domain.Payload{Data: []byte(reportJSON)}}}
	d := &AgentScoutDispatcher{Auctioneer: f, ScoutAgentID: "scout_agent"}

	rep := d.Discover(context.Background(), "continue the helicopter folder")
	if f.calledAgent != "scout_agent" {
		t.Errorf("must invoke scout_agent; got %q", f.calledAgent)
	}
	if len(rep.Entities) != 1 || rep.Entities[0].ID != "helicopter" || !rep.Entities[0].Exists {
		t.Errorf("entities not parsed: %+v", rep.Entities)
	}
	if rep.Interpretation != "7 remain" || len(rep.Unobserved) != 1 {
		t.Errorf("interpretation/unobserved not parsed: %q %v", rep.Interpretation, rep.Unobserved)
	}
	if rep.Environment == nil {
		t.Error("env grounding must always be present")
	}
}

// A dispatch failure must still ground the environment (an env-only report is non-empty, so
// the kernel attaches it and the Planner still gets correct host paths).
func TestAgentScoutDispatcher_DispatchFailureStillGroundsEnv(t *testing.T) {
	d := &AgentScoutDispatcher{Auctioneer: &fakeScoutAuctioneer{err: errors.New("agent down")}, ScoutAgentID: "scout_agent"}
	rep := d.Discover(context.Background(), "anything")
	if len(rep.Entities) != 0 {
		t.Error("a failed dispatch must yield no entities")
	}
	if rep.Environment == nil || rep.IsEmpty() {
		t.Error("env grounding must still flow (env-only report is non-empty)")
	}
}
