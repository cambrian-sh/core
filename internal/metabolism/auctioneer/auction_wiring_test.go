package auctioneer

import (
	"context"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// ─── Static Bidder Tests (Issue #0008-04) ────────────────────────────────────
// ADR-0023 Routing Fix: Tool agents no longer bypass the proposal RPC.
// They call their Python on_proposal handler via the normal gRPC path.
// Tests register mock clients so ConductAuction can request proposals.

func TestConductAuction_TraitTool_StaticBid_Confidence(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "tool-agent-1",
		Description: "A deterministic calculator tool",
		Trait:       domain.TraitTool,
	}

	task := &domain.AuctionTask{
		ID:          "task-tool-001",
		Description: "compute something",
		Context:     "{}",
		Deadline:    time.Now().Add(5 * time.Second),
	}

	mockClient := &mockAgentClient{
		proposal: &pb.ProposalResponse{
			Confidence:         1.0,
			Rationale:          "A deterministic calculator tool",
			EstimatedLatencyMs: 5,
		},
	}

	a := &Auctioneer{
		agentClients:         make(map[string]pb.AgentServiceClient),
		MinAuctionConfidence: 0.3,
	}
	a.agentClients[agent.ID] = &pbClientWrapper{m: mockClient}

	winner, err := a.ConductAuction(context.Background(), task, []domain.AgentDefinition{agent})
	if err != nil {
		t.Fatalf("ConductAuction failed: %v", err)
	}
	if winner == nil {
		t.Fatal("expected a winner, got nil")
	}
	if winner.Confidence != 1.0 {
		t.Errorf("expected Confidence=1.0 for TraitTool agent, got %f", winner.Confidence)
	}
}

func TestConductAuction_TraitTool_StaticBid_Rationale(t *testing.T) {
	const wantDesc = "Converts units of measurement deterministically"
	agent := domain.AgentDefinition{
		ID:          "tool-agent-2",
		Description: wantDesc,
		Trait:       domain.TraitTool,
	}

	task := &domain.AuctionTask{
		ID:       "task-tool-002",
		Deadline: time.Now().Add(5 * time.Second),
	}

	mockClient := &mockAgentClient{
		proposal: &pb.ProposalResponse{
			Confidence:         1.0,
			Rationale:          wantDesc,
			EstimatedLatencyMs: 5,
		},
	}

	a := &Auctioneer{
		agentClients:         make(map[string]pb.AgentServiceClient),
		MinAuctionConfidence: 0.3,
	}
	a.agentClients[agent.ID] = &pbClientWrapper{m: mockClient}

	winner, err := a.ConductAuction(context.Background(), task, []domain.AgentDefinition{agent})
	if err != nil {
		t.Fatalf("ConductAuction failed: %v", err)
	}
	if winner.Rationale != wantDesc {
		t.Errorf("expected Rationale=%q, got %q", wantDesc, winner.Rationale)
	}
}

func TestConductAuction_TraitTool_StaticBid_Latency(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:    "tool-agent-3",
		Trait: domain.TraitTool,
	}

	task := &domain.AuctionTask{
		ID:       "task-tool-003",
		Deadline: time.Now().Add(5 * time.Second),
	}

	mockClient := &mockAgentClient{
		proposal: &pb.ProposalResponse{
			Confidence:         1.0,
			Rationale:          "fast tool",
			EstimatedLatencyMs: 5,
		},
	}

	a := &Auctioneer{
		agentClients:         make(map[string]pb.AgentServiceClient),
		MinAuctionConfidence: 0.3,
	}
	a.agentClients[agent.ID] = &pbClientWrapper{m: mockClient}

	winner, err := a.ConductAuction(context.Background(), task, []domain.AgentDefinition{agent})
	if err != nil {
		t.Fatalf("ConductAuction failed: %v", err)
	}
	if winner.Latency != 5 {
		t.Errorf("expected Latency=5 for TraitTool agent, got %d", winner.Latency)
	}
}

// Test SB-5: ConductAuction emits a "completed" AuctionEvent whose BidEntry for
// the TraitTool candidate has IsTool=true, confirming the wire from the agent's
// proposal response → AgentProposal.IsTool → pb.BidEntry.IsTool → EventSink.
func TestConductAuction_TraitTool_BidEntry_IsTool(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:    "tool-agent-5",
		Trait: domain.TraitTool,
	}
	task := &domain.AuctionTask{
		ID:       "task-tool-005",
		Deadline: time.Now().Add(5 * time.Second),
	}

	mockClient := &mockAgentClient{
		proposal: &pb.ProposalResponse{
			Confidence:         1.0,
			Rationale:          "tool",
			EstimatedLatencyMs: 5,
		},
	}

	bus := &capturingEventBus{}
	a := &Auctioneer{
		agentClients:         make(map[string]pb.AgentServiceClient),
		MinAuctionConfidence: 0.3,
		EventBus:             bus,
	}
	a.agentClients[agent.ID] = &pbClientWrapper{m: mockClient}

	if _, err := a.ConductAuction(context.Background(), task, []domain.AgentDefinition{agent}); err != nil {
		t.Fatalf("ConductAuction failed: %v", err)
	}

	var completedEv *domain.AuctionEventPayload
	for i := range bus.events {
		if bus.events[i].Status == "completed" {
			completedEv = &bus.events[i]
			break
		}
	}
	if completedEv == nil {
		t.Fatal("no 'completed' AuctionEvent was emitted")
	}
	if len(completedEv.Bids) == 0 {
		t.Fatal("expected at least one BidEntry in the completed event")
	}
	if !completedEv.Bids[0].IsTool {
		t.Errorf("expected BidEntry.IsTool=true for TraitTool agent, got false")
	}
}

func TestConductAuction_TraitTool_BeatsLowerConfidence(t *testing.T) {
	toolAgent := domain.AgentDefinition{
		ID:          "tool-agent-4",
		Description: "fast tool",
		Trait:       domain.TraitTool,
	}
	cogAgent := domain.AgentDefinition{
		ID: "cog-agent-4",
	}

	task := &domain.AuctionTask{
		ID:       "task-tool-004",
		Deadline: time.Now().Add(5 * time.Second),
	}

	toolMock := &mockAgentClient{
		proposal: &pb.ProposalResponse{
			Confidence:         1.0,
			Rationale:          "tool",
			EstimatedLatencyMs: 5,
		},
	}
	cogMock := &mockAgentClient{
		proposal: &pb.ProposalResponse{
			Confidence:         0.5,
			Rationale:          "cog",
			EstimatedLatencyMs: 100,
		},
	}

	a := &Auctioneer{
		agentClients:         make(map[string]pb.AgentServiceClient),
		MinAuctionConfidence: 0.3,
	}
	a.agentClients[toolAgent.ID] = &pbClientWrapper{m: toolMock}
	a.agentClients[cogAgent.ID] = &pbClientWrapper{m: cogMock}

	winner, err := a.ConductAuction(context.Background(), task, []domain.AgentDefinition{toolAgent, cogAgent})
	if err != nil {
		t.Fatalf("ConductAuction failed: %v", err)
	}
	if winner.AgentID != toolAgent.ID {
		t.Errorf("expected tool agent to win, but winner was %q", winner.AgentID)
	}
	if winner.Confidence != 1.0 {
		t.Errorf("expected winning Confidence=1.0, got %f", winner.Confidence)
	}
}

// ROUTE-02: the completed AuctionEvent carries the winner margin (winner conf
// minus best losing bid) and passes the Gatekeeper funnel through from the task.
func TestConductAuction_EmitsWinnerMarginAndFunnel(t *testing.T) {
	winnerAgent := domain.AgentDefinition{ID: "win-agent"}
	loserAgent := domain.AgentDefinition{ID: "lose-agent"}

	task := &domain.AuctionTask{
		ID:       "task-margin-1",
		Deadline: time.Now().Add(5 * time.Second),
		// Funnel is normally written by the Gatekeeper; set it directly here to
		// assert ConductAuction reads it off the same task pointer.
		Funnel: &domain.GatekeeperFunnel{
			L1: []domain.DeclarationResult{{AgentID: "win-agent", Passed: true}},
			L3: []domain.MeritResult{{AgentID: "win-agent", Score: 0.9}},
		},
	}

	winMock := &mockAgentClient{proposal: &pb.ProposalResponse{Confidence: 0.9, Rationale: "win"}}
	loseMock := &mockAgentClient{proposal: &pb.ProposalResponse{Confidence: 0.6, Rationale: "lose"}}

	bus := &capturingEventBus{}
	a := &Auctioneer{
		agentClients:         make(map[string]pb.AgentServiceClient),
		MinAuctionConfidence: 0.3,
		EventBus:             bus,
	}
	a.agentClients[winnerAgent.ID] = &pbClientWrapper{m: winMock}
	a.agentClients[loserAgent.ID] = &pbClientWrapper{m: loseMock}

	if _, err := a.ConductAuction(context.Background(), task, []domain.AgentDefinition{winnerAgent, loserAgent}); err != nil {
		t.Fatalf("ConductAuction failed: %v", err)
	}

	var completedEv *domain.AuctionEventPayload
	for i := range bus.events {
		if bus.events[i].Status == "completed" {
			completedEv = &bus.events[i]
			break
		}
	}
	if completedEv == nil {
		t.Fatal("no 'completed' AuctionEvent was emitted")
	}
	// 0.9 winner minus 0.6 runner-up = 0.3 (float32 tolerance).
	if diff := completedEv.WinnerMargin - 0.3; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("expected winner margin ~0.3, got %f", completedEv.WinnerMargin)
	}
	if completedEv.Funnel == nil {
		t.Fatal("expected the Gatekeeper funnel to pass through to the completed event")
	}
	if len(completedEv.Funnel.L3) != 1 || completedEv.Funnel.L3[0].AgentID != "win-agent" {
		t.Errorf("funnel L3 not carried through: %+v", completedEv.Funnel.L3)
	}
}
