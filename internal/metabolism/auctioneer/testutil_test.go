package auctioneer

import (
	"context"
	"fmt"
	"sort"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc"
)

// ─── Mock gRPC clients ───────────────────────────────────────────────────────

// capturingAgentClient implements pb.AgentServiceClient and records the last
// ProposalRequest so tests can assert on its fields.
type capturingAgentClient struct {
	lastReq *pb.ProposalRequest
}

func (c *capturingAgentClient) RequestProposal(_ context.Context, req *pb.ProposalRequest, _ ...grpc.CallOption) (*pb.ProposalResponse, error) {
	c.lastReq = req
	return &pb.ProposalResponse{
		Confidence:         0.9,
		Rationale:          "mock rationale",
		EstimatedLatencyMs: 100,
	}, nil
}

func (c *capturingAgentClient) Execute(_ context.Context, _ *pb.Handoff, _ ...grpc.CallOption) (*pb.Handoff, error) {
	return nil, nil
}

func (c *capturingAgentClient) VerifyOutput(_ context.Context, _ *pb.VerifyRequest, _ ...grpc.CallOption) (*pb.VerifyResponse, error) {
	return nil, nil
}

// mockAgentClient is a configurable client for Execute-level tests.
type mockAgentClient struct {
	pb.AgentServiceServer
	proposal *pb.ProposalResponse
	execute  *pb.Handoff
	err      error

	lastProposalReq *pb.ProposalRequest
	lastExecuteReq  *pb.Handoff
}

func (m *mockAgentClient) RequestProposal(_ context.Context, req *pb.ProposalRequest) (*pb.ProposalResponse, error) {
	m.lastProposalReq = req
	return m.proposal, m.err
}

func (m *mockAgentClient) Execute(_ context.Context, req *pb.Handoff) (*pb.Handoff, error) {
	m.lastExecuteReq = req
	return m.execute, m.err
}

// pbClientWrapper adapts the mock server interface to the gRPC client interface.
type pbClientWrapper struct {
	pb.AgentServiceClient
	m *mockAgentClient
}

func (w *pbClientWrapper) RequestProposal(ctx context.Context, in *pb.ProposalRequest, opts ...grpc.CallOption) (*pb.ProposalResponse, error) {
	return w.m.RequestProposal(ctx, in)
}

func (w *pbClientWrapper) Execute(ctx context.Context, in *pb.Handoff, opts ...grpc.CallOption) (*pb.Handoff, error) {
	return w.m.Execute(ctx, in)
}

// ─── Mock Profile Reader ─────────────────────────────────────────────────────

type auctioneerProfileReader struct {
	profiles map[string]*domain.AgentProfile
}

func (r *auctioneerProfileReader) GetProfile(_ context.Context, agentID, sourceHash string) (*domain.AgentProfile, error) {
	if r.profiles == nil {
		return nil, nil
	}
	return r.profiles[agentID+":"+sourceHash], nil
}

// ─── Mock EventBus ───────────────────────────────────────────────────────────

type capturingEventBus struct {
	events []domain.AuctionEventPayload
}

func (b *capturingEventBus) Subscribe(_ string, _ domain.EventHandler) {}
func (b *capturingEventBus) Publish(ev domain.DomainEvent) error {
	if auctionEv, ok := ev.(domain.AuctionEventPayload); ok {
		b.events = append(b.events, auctionEv)
	}
	return nil
}

// ─── Mock AgentDialer ────────────────────────────────────────────────────────

type mockDialer struct {
	agents map[string]*domain.AgentDefinition
}

func (m *mockDialer) GetOrBootInstance(_ context.Context, _ *domain.AgentDefinition, _ string) *domain.Instance {
	return nil
}

func (m *mockDialer) DialAgent(_ string) (*grpc.ClientConn, error) {
	return nil, fmt.Errorf("DialAgent not available in test")
}

func (m *mockDialer) GetAgentByName(_ context.Context, name string) (*domain.AgentDefinition, error) {
	if m == nil || m.agents == nil {
		return nil, fmt.Errorf("agent not found: %s", name)
	}
	a, ok := m.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent not found: %s", name)
	}
	return a, nil
}

func (m *mockDialer) GetManifest(_ context.Context, agentID string) (*domain.AgentManifest, error) {
	return nil, nil
}

// ─── Mock Gatekeeper ─────────────────────────────────────────────────────────

// testGatekeeper implements domain.Gatekeeper with manifest-aware Declaration
// filtering so recursive-bidding tests behave correctly.
type testGatekeeper struct {
	agents    map[string]domain.AgentDefinition
	manifests map[string]*domain.AgentManifest
}

func (g *testGatekeeper) FindCandidates(_ context.Context, task *domain.AuctionTask) ([]domain.ScoredCandidate, error) {
	var candidates []domain.ScoredCandidate
	for _, agent := range g.agents {
		manifest := g.manifests[agent.ID]
		if !gatekeeperPassesFormats(manifest, task.RequiredFormats) {
			continue
		}
		score := 0.5
		if agent.Provisional {
			score = 0.1
		}
		candidates = append(candidates, domain.ScoredCandidate{Agent: agent, Score: score})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	return candidates, nil
}

func (g *testGatekeeper) FindModelCandidates(_ context.Context, requiredCapabilities []string) ([]domain.ScoredCandidate, error) {
	var candidates []domain.ScoredCandidate
	for _, agent := range g.agents {
		if agent.Trait != domain.TraitModel {
			continue
		}
		manifest := g.manifests[agent.ID]
		if len(requiredCapabilities) > 0 && manifest != nil {
			hasCap := make(map[string]bool, len(manifest.Capabilities))
			for _, c := range manifest.Capabilities {
				hasCap[c] = true
			}
			ok := true
			for _, req := range requiredCapabilities {
				if !hasCap[req] {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
		}
		candidates = append(candidates, domain.ScoredCandidate{Agent: agent, Score: 0.9})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	return candidates, nil
}

func gatekeeperPassesFormats(manifest *domain.AgentManifest, required []string) bool {
	if len(required) == 0 {
		return true
	}
	if manifest == nil {
		return true
	}
	supported := make(map[string]struct{}, len(manifest.SupportedFormats))
	for _, f := range manifest.SupportedFormats {
		supported[f] = struct{}{}
	}
	for _, req := range required {
		if _, ok := supported[req]; !ok {
			return false
		}
	}
	return true
}

// ─── Test helpers ────────────────────────────────────────────────────────────

func buildTestAuctioneer(agentID string, client pb.AgentServiceClient, profiles GatekeeperProfileReader) *Auctioneer {
	a := &Auctioneer{
		agentClients:         make(map[string]pb.AgentServiceClient),
		Profiles:             profiles,
		MinAuctionConfidence: 0.3,
	}
	a.agentClients[agentID] = client
	return a
}

func testAuctionTask() *domain.AuctionTask {
	return &domain.AuctionTask{
		ID:          "task-001",
		Description: "test description",
		Context:     "test context",
		Deadline:    time.Now().Add(5 * time.Second),
	}
}
