package auctioneer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
)

// ─── Cycle 1: requestProposalFromAgent sets ConfidenceHint from profile ──────

func TestRequestProposalFromAgent_SetsConfidenceHintFromProfile(t *testing.T) {
	agentID := "agent-hint-test"
	sourceHash := "hash-001"

	agent := domain.AgentDefinition{
		ID:         agentID,
		SourceHash: sourceHash,
	}

	client := &capturingAgentClient{}
	profiles := &auctioneerProfileReader{
		profiles: map[string]*domain.AgentProfile{
			agentID + ":" + sourceHash: {
				AgentID:    agentID,
				SourceHash: sourceHash,
				TrustScore: 0.75,
			},
		},
	}

	a := buildTestAuctioneer(agentID, client, profiles)

	ctx := context.Background()
	hint := computeConfidenceHint(ctx, a.Profiles, agent)

	_, _ = a.requestProposalFromAgent(ctx, agent, testAuctionTask(), hint)

	if client.lastReq == nil {
		t.Fatal("expected RequestProposal to be called, but it was not")
	}
	if client.lastReq.ConfidenceHint != float32(0.75) {
		t.Errorf("expected ConfidenceHint=0.75, got %f", client.lastReq.ConfidenceHint)
	}
}

// ─── Cycle 2: Zero hint for cold-start (no profile) ──────────────────────────

func TestRequestProposalFromAgent_ZeroHintForColdStart(t *testing.T) {
	agentID := "agent-cold-start"
	sourceHash := "hash-002"

	agent := domain.AgentDefinition{
		ID:         agentID,
		SourceHash: sourceHash,
	}

	client := &capturingAgentClient{}
	profiles := &auctioneerProfileReader{
		profiles: map[string]*domain.AgentProfile{},
	}

	a := buildTestAuctioneer(agentID, client, profiles)

	ctx := context.Background()
	hint := computeConfidenceHint(ctx, a.Profiles, agent)

	_, _ = a.requestProposalFromAgent(ctx, agent, testAuctionTask(), hint)

	if client.lastReq == nil {
		t.Fatal("expected RequestProposal to be called, but it was not")
	}
	if client.lastReq.ConfidenceHint != float32(0.0) {
		t.Errorf("expected ConfidenceHint=0.0 for cold-start, got %f", client.lastReq.ConfidenceHint)
	}
}

func TestRequestProposalFromAgent_NilProfilesReader_ZeroHint(t *testing.T) {
	agentID := "agent-no-reader"
	agent := domain.AgentDefinition{ID: agentID, SourceHash: "hash-003"}

	client := &capturingAgentClient{}
	a := buildTestAuctioneer(agentID, client, nil)

	ctx := context.Background()
	hint := computeConfidenceHint(ctx, a.Profiles, agent)

	_, _ = a.requestProposalFromAgent(ctx, agent, testAuctionTask(), hint)

	if client.lastReq == nil {
		t.Fatal("expected RequestProposal to be called, but it was not")
	}
	if client.lastReq.ConfidenceHint != float32(0.0) {
		t.Errorf("expected ConfidenceHint=0.0 when Profiles is nil, got %f", client.lastReq.ConfidenceHint)
	}
}

func TestComputeConfidenceHint_ClampsAboveOne(t *testing.T) {
	agentID := "agent-clamp"
	sourceHash := "hash-004"
	agent := domain.AgentDefinition{ID: agentID, SourceHash: sourceHash}

	profiles := &auctioneerProfileReader{
		profiles: map[string]*domain.AgentProfile{
			agentID + ":" + sourceHash: {TrustScore: 1.5},
		},
	}

	hint := computeConfidenceHint(context.Background(), profiles, agent)
	if hint != float32(1.0) {
		t.Errorf("expected clamped hint=1.0, got %f", hint)
	}
}

func TestComputeConfidenceHint_ClampsNegative(t *testing.T) {
	agentID := "agent-clamp-neg"
	sourceHash := "hash-005"
	agent := domain.AgentDefinition{ID: agentID, SourceHash: sourceHash}

	profiles := &auctioneerProfileReader{
		profiles: map[string]*domain.AgentProfile{
			agentID + ":" + sourceHash: {TrustScore: -0.3},
		},
	}

	hint := computeConfidenceHint(context.Background(), profiles, agent)
	if hint != float32(0.0) {
		t.Errorf("expected clamped hint=0.0 for negative TrustScore, got %f", hint)
	}
}

// ─── Tool-Agent tests ──────────────────────────────────────────────────────

func TestRequestProposalFromAgent_ToolAgent_SetsIsTool(t *testing.T) {
	mockClient := &mockAgentClient{
		proposal: &pb.ProposalResponse{
			Confidence:         1.0,
			Rationale:          "tool",
			EstimatedLatencyMs: 5,
		},
	}
	a := &Auctioneer{
		agentClients: make(map[string]pb.AgentServiceClient),
	}
	a.agentClients["tool-agent"] = &pbClientWrapper{m: mockClient}

	agent := domain.AgentDefinition{
		ID:          "tool-agent",
		Trait:       domain.TraitTool,
		Description: "I am a tool",
	}
	task := &domain.AuctionTask{ID: "task-1"}

	prop, err := a.requestProposalFromAgent(context.Background(), agent, task, 0.0)
	if err != nil {
		t.Fatalf("requestProposalFromAgent failed: %v", err)
	}

	if !prop.IsTool {
		t.Error("expected IsTool=true for TraitTool agent")
	}
}

// ─── Recursive Bidding tests ───────────────────────────────────────────────

func TestAuctioneer_Execute_SingleRequirement(t *testing.T) {
	coordID := "coordinator"
	calcID := "calculator"

	agents := map[string]domain.AgentDefinition{
		coordID: {ID: coordID},
		calcID:  {ID: calcID},
	}
	manifests := map[string]*domain.AgentManifest{
		coordID: {Tools: []string{"coordinator"}, SupportedFormats: []string{"coordinator"}},
		calcID:  {Tools: []string{"calculator"}, SupportedFormats: []string{"calculator"}},
	}

	coordMock := &mockAgentClient{
		proposal: &pb.ProposalResponse{
			Confidence:   1.0,
			Requirements: []string{"calculator"},
		},
		execute: &pb.Handoff{
			Payload: &pb.Object{Data: []byte("final result")},
		},
	}
	calcMock := &mockAgentClient{
		proposal: &pb.ProposalResponse{Confidence: 0.9},
		execute:  &pb.Handoff{Payload: &pb.Object{Data: []byte("42")}},
	}

	agentPtrs := map[string]*domain.AgentDefinition{}
	for id, a := range agents {
		a := a
		agentPtrs[id] = &a
	}
	manager := &mockDialer{agents: agentPtrs}
	gk := &testGatekeeper{agents: agents, manifests: manifests}
	cfg := config.ExecutionConfig{MinAuctionConfidence: 0.3, MaxRecursionDepth: 3}
	auc := New(manager, gk, cfg)

	auc.RegisterAgentClient(coordID, &pbClientWrapper{m: coordMock}, nil)
	auc.RegisterAgentClient(calcID, &pbClientWrapper{m: calcMock}, nil)

	task := &domain.AuctionTask{
		ID:          "task-parent",
		Description: "coordinate something",
	}
	handoff := &domain.Handoff{Payload: &domain.Payload{Data: []byte("input")}}

	result, err := auc.Execute(t.Context(), task, handoff)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if string(result.Handoff.Payload.Data) != "final result" {
		t.Errorf("expected final result, got %q", string(result.Handoff.Payload.Data))
	}

	if coordMock.lastExecuteReq == nil {
		t.Fatal("coordinator was never called")
	}

	calcResult, ok := coordMock.lastExecuteReq.Metadata["requirements.calculator.result"]
	if !ok {
		t.Errorf("calculator result not found in coordinator context. Context: %v", coordMock.lastExecuteReq.Metadata)
	} else if calcResult != "42" {
		t.Errorf("expected requirement result '42', got %q", calcResult)
	}
}

func TestAuctioneer_Execute_RecursionLimit(t *testing.T) {
	idA := "agentA"
	idB := "agentB"

	agents := map[string]domain.AgentDefinition{
		idA: {ID: idA},
		idB: {ID: idB},
	}
	manifests := map[string]*domain.AgentManifest{
		idA: {Tools: []string{idA}, SupportedFormats: []string{idA}},
		idB: {Tools: []string{idB}, SupportedFormats: []string{idB}},
	}

	mockA := &mockAgentClient{
		proposal: &pb.ProposalResponse{
			Confidence:   1.0,
			Requirements: []string{idB},
		},
	}
	mockB := &mockAgentClient{
		proposal: &pb.ProposalResponse{
			Confidence:   1.0,
			Requirements: []string{idA},
		},
	}

	agentPtrs := map[string]*domain.AgentDefinition{}
	for id, a := range agents {
		a := a
		agentPtrs[id] = &a
	}
	manager := &mockDialer{agents: agentPtrs}
	gk := &testGatekeeper{agents: agents, manifests: manifests}
	cfg := config.ExecutionConfig{MinAuctionConfidence: 0.3, MaxRecursionDepth: 1}
	auc := New(manager, gk, cfg)

	auc.RegisterAgentClient(idA, &pbClientWrapper{m: mockA}, nil)
	auc.RegisterAgentClient(idB, &pbClientWrapper{m: mockB}, nil)

	task := &domain.AuctionTask{ID: "task-limit", Description: "start recursion"}
	handoff := &domain.Handoff{Payload: &domain.Payload{Data: []byte("start")}}

	_, err := auc.Execute(t.Context(), task, handoff)
	if err == nil {
		t.Fatal("expected error due to recursion limit, got nil")
	}

	if !strings.Contains(err.Error(), "max recursion depth reached") {
		t.Errorf("expected max recursion depth error, got: %v", err)
	}
}

// ─── Cycle 3: Execute returns runner-up ScoredCandidate list ──────────────

func TestExecute_ReturnsRunnerUpCandidates(t *testing.T) {
	agentDefs := []struct {
		id      string
		conf    float64
		latency int
	}{
		{"winner", 0.95, 50},
		{"runner1", 0.80, 60},
		{"runner2", 0.70, 70},
	}

	agents := make(map[string]domain.AgentDefinition)
	manifests := make(map[string]*domain.AgentManifest)
	for _, a := range agentDefs {
		agents[a.id] = domain.AgentDefinition{ID: a.id}
		manifests[a.id] = &domain.AgentManifest{Tools: []string{a.id}, SupportedFormats: []string{a.id}}
	}

	mocks := make(map[string]*mockAgentClient)
	for _, a := range agentDefs {
		mocks[a.id] = &mockAgentClient{
			proposal: &pb.ProposalResponse{
				Confidence:         float32(a.conf),
				EstimatedLatencyMs: int32(a.latency),
			},
			execute: &pb.Handoff{
				Payload: &pb.Object{Data: []byte("result-" + a.id)},
			},
		}
	}

	agentPtrs := map[string]*domain.AgentDefinition{}
	for id, a := range agents {
		a := a
		agentPtrs[id] = &a
	}
	manager := &mockDialer{agents: agentPtrs}
	gk := &testGatekeeper{agents: agents, manifests: manifests}
	cfg := config.ExecutionConfig{
		MinAuctionConfidence:    0.3,
		MaxRecursionDepth:       3,
		GatekeeperMaxCandidates: 3,
		GatekeeperW1:            0.4,
		GatekeeperW2:            0.4,
		GatekeeperW3:            0.2,
	}
	auc := New(manager, gk, cfg)

	for _, a := range agentDefs {
		auc.RegisterAgentClient(a.id, &pbClientWrapper{m: mocks[a.id]}, nil)
	}

	task := &domain.AuctionTask{ID: "task-runnerup", Description: "test runner-up"}
	handoff := &domain.Handoff{Payload: &domain.Payload{Data: []byte("input")}}

	result, err := auc.Execute(t.Context(), task, handoff)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if string(result.Handoff.Payload.Data) != "result-winner" {
		t.Errorf("expected winner response 'result-winner', got %q", string(result.Handoff.Payload.Data))
	}

	if result.Confidence < 0.94 || result.Confidence > 0.96 {
		t.Errorf("expected confidence ~0.95, got %f", result.Confidence)
	}

	if len(result.RunnerUps) != 2 {
		t.Fatalf("expected 2 runner-ups, got %d", len(result.RunnerUps))
	}

	for _, ru := range result.RunnerUps {
		if ru.Agent.ID == "winner" {
			t.Errorf("runner-up list contains winner %q", ru.Agent.ID)
		}
	}

	for _, ru := range result.RunnerUps {
		if ru.Score == 0 {
			t.Errorf("runner-up %q has zero score", ru.Agent.ID)
		}
	}
}

func TestExecute_ReturnsEmptyRunnerUpsWithSingleCandidate(t *testing.T) {
	agentID := "solo"
	agents := map[string]domain.AgentDefinition{
		agentID: {ID: agentID},
	}
	manifests := map[string]*domain.AgentManifest{
		agentID: {Tools: []string{agentID}, SupportedFormats: []string{agentID}},
	}

	mock := &mockAgentClient{
		proposal: &pb.ProposalResponse{Confidence: 0.9},
		execute:  &pb.Handoff{Payload: &pb.Object{Data: []byte("solo result")}},
	}

	a := agents[agentID]
	manager := &mockDialer{agents: map[string]*domain.AgentDefinition{agentID: &a}}
	gk := &testGatekeeper{agents: agents, manifests: manifests}
	cfg := config.ExecutionConfig{
		MinAuctionConfidence: 0.3,
		MaxRecursionDepth:    3,
		GatekeeperW1:         0.4,
		GatekeeperW2:         0.4,
		GatekeeperW3:         0.2,
	}
	auc := New(manager, gk, cfg)
	auc.RegisterAgentClient(agentID, &pbClientWrapper{m: mock}, nil)

	task := &domain.AuctionTask{ID: "task-solo", Description: "solo"}
	handoff := &domain.Handoff{Payload: &domain.Payload{Data: []byte("x")}}

	result, err := auc.Execute(t.Context(), task, handoff)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if len(result.RunnerUps) != 0 {
		t.Errorf("expected 0 runner-ups for single candidate, got %d", len(result.RunnerUps))
	}
}

func TestConductAuction_PropagatesIsTool(t *testing.T) {
	bus := &capturingEventBus{}
	a := &Auctioneer{
		agentClients:         make(map[string]pb.AgentServiceClient),
		EventBus:             bus,
		MinAuctionConfidence: 0.1,
	}

	task := &domain.AuctionTask{ID: "task-1", Description: "test task"}
	candidates := []domain.AgentDefinition{
		{ID: "tool-agent-1", Trait: domain.TraitTool, Description: "tool 1"},
		{ID: "tool-agent-2", Trait: domain.TraitTool, Description: "tool 2"},
	}

	for _, c := range candidates {
		mock := &mockAgentClient{
			proposal: &pb.ProposalResponse{
				Confidence:         1.0,
				Rationale:          c.Description,
				EstimatedLatencyMs: 5,
			},
		}
		a.agentClients[c.ID] = &pbClientWrapper{m: mock}
	}

	_, err := a.ConductAuction(context.Background(), task, candidates)
	if err != nil {
		t.Fatalf("ConductAuction failed: %v", err)
	}

	if len(bus.events) == 0 {
		t.Fatal("no events emitted")
	}

	var completedEv *domain.AuctionEventPayload
	for i := range bus.events {
		if bus.events[i].Status == "completed" {
			completedEv = &bus.events[i]
			break
		}
	}

	if completedEv == nil {
		t.Fatal("completed event not found")
	}

	if len(completedEv.Bids) != 2 {
		t.Errorf("expected 2 bids, got %d", len(completedEv.Bids))
	}

	for _, bid := range completedEv.Bids {
		if !bid.IsTool {
			t.Errorf("expected IsTool=true for bid from agent %s", bid.AgentID)
		}
	}
}

func TestAuctioneer_ExplorationRunsWithoutCrash(t *testing.T) {
	n := 5
	agents := make(map[string]domain.AgentDefinition)
	manifests := make(map[string]*domain.AgentManifest)
	agentPtrs := make(map[string]*domain.AgentDefinition)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("agent-%d", i)
		agents[id] = domain.AgentDefinition{ID: id}
		manifests[id] = &domain.AgentManifest{Tools: []string{id}, SupportedFormats: []string{id}}
		a := agents[id]
		agentPtrs[id] = &a
	}

	manager := &mockDialer{agents: agentPtrs}
	gk := &testGatekeeper{agents: agents, manifests: manifests}
	cfg := config.ExecutionConfig{MinAuctionConfidence: 0.3, MaxRecursionDepth: 3}
	auc := New(manager, gk, cfg)
	auc.ExplorationRate = 1.0

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("agent-%d", i)
		mock := &mockAgentClient{
			proposal: &pb.ProposalResponse{Confidence: float32(1.0 - float64(i)*0.1)},
			execute:  &pb.Handoff{Payload: &pb.Object{Data: []byte(id)}},
		}
		auc.RegisterAgentClient(id, &pbClientWrapper{m: mock}, nil)
	}

	task := &domain.AuctionTask{ID: "t-explore", Description: "test"}
	result, err := auc.Execute(t.Context(), task, &domain.Handoff{Payload: &domain.Payload{Data: []byte("x")}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result == nil {
		t.Fatal("nil response")
	}
	winners := make(map[string]int)
	for i := 0; i < 10; i++ {
		result, err := auc.Execute(t.Context(), task, &domain.Handoff{Payload: &domain.Payload{Data: []byte("x")}})
		if err != nil {
			t.Fatalf("Execute run %d: %v", i, err)
		}
		winners[string(result.Handoff.Payload.Data)]++
	}
	if len(winners) <= 1 {
		t.Error("exploration did not produce variation in winners across 10 runs")
	}
	t.Logf("winners: %v", winners)
}

func TestAuctioneer_ExplorationDisabled_SameWinner(t *testing.T) {
	n := 5
	agents := make(map[string]domain.AgentDefinition)
	manifests := make(map[string]*domain.AgentManifest)
	agentPtrs := make(map[string]*domain.AgentDefinition)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("agent-%d", i)
		agents[id] = domain.AgentDefinition{ID: id}
		manifests[id] = &domain.AgentManifest{Tools: []string{id}, SupportedFormats: []string{id}}
		a := agents[id]
		agentPtrs[id] = &a
	}

	manager := &mockDialer{agents: agentPtrs}
	gk := &testGatekeeper{agents: agents, manifests: manifests}
	cfg := config.ExecutionConfig{MinAuctionConfidence: 0.3, MaxRecursionDepth: 3}
	auc := New(manager, gk, cfg)
	auc.ExplorationRate = 0.0

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("agent-%d", i)
		mock := &mockAgentClient{
			proposal: &pb.ProposalResponse{Confidence: float32(1.0 - float64(i)*0.1)},
			execute:  &pb.Handoff{Payload: &pb.Object{Data: []byte(id)}},
		}
		auc.RegisterAgentClient(id, &pbClientWrapper{m: mock}, nil)
	}

	task := &domain.AuctionTask{ID: "t-noexplore", Description: "test"}
	result, err := auc.Execute(t.Context(), task, &domain.Handoff{Payload: &domain.Payload{Data: []byte("x")}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result == nil {
		t.Fatal("nil response")
	}

	// testGatekeeper returns agents with uniform score 0.5; exploration=0 means
	// the top scored agent wins. With uniform scores, any agent may be "first" in
	// the map — we just check that the same one always wins across 5 runs.
	firstWinner := string(result.Handoff.Payload.Data)
	for i := 0; i < 5; i++ {
		r, err := auc.Execute(t.Context(), task, &domain.Handoff{Payload: &domain.Payload{Data: []byte("x")}})
		if err != nil {
			t.Fatalf("Execute run %d: %v", i, err)
		}
		if string(r.Handoff.Payload.Data) != firstWinner {
			t.Errorf("expected consistent winner %s, got %s on run %d", firstWinner, string(r.Handoff.Payload.Data), i)
		}
	}
	t.Logf("consistent winner: %s", firstWinner)
}

// ─── Unused import guard ─────────────────────────────────────────────────────
var _ = time.Second
