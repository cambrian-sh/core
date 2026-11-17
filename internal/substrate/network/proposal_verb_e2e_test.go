package network_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/identity"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/knowledgecommit"
	"github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// End to end through the real Orchestrator gRPC surface and the SAME interceptor
// chain app.go installs (surface stamp → agent credential → operator bearer):
// the kernel enforces the agent's opt-in to propose_knowledge (owner option A,
// T-20261113-agent-proposal-rpcs-proposeknowledge). The agent authenticates with
// its own credential; restricted tool grants; no grant → PermissionDenied; an
// operator grant of the verb → the proposal reaches the proposer.

type countingProposer struct{ calls int }

func (c *countingProposer) Propose(context.Context, domain.PrincipalRef, domain.PrincipalRef, knowledgecommit.Proposal, bool, string) (knowledgecommit.Result, error) {
	c.calls++
	return knowledgecommit.Result{CandidateID: "ki_e2e", Inserted: true}, nil
}

func (c *countingProposer) ProposeLink(context.Context, domain.PrincipalRef, knowledgecommit.LinkProposal) (bool, error) {
	c.calls++
	return true, nil
}

func TestE2E_ProposeKnowledgeNeedsTheVerbGrantThroughTheAgentPlane(t *testing.T) {
	t.Parallel()
	gw := network.NewLLMGateway(config.ExecutionConfig{
		Session: config.SessionConfig{SessionTokenTTLMultiplier: 5},
		LLM:     config.LLMConfig{LLMGatewayMaxConcurrency: 4},
	}, llm.NewSpendMeter(0, nil, nil))
	grants := domain.NewInMemoryGrantsStore()
	prop := &countingProposer{}
	srvImpl := &network.Server{LLMGateway: gw, Knowledge: prop, ToolExecutor: &domain.ToolExecutor{Grants: grants}}

	secrets := map[string]string{authz.AgentTokenSecretName("planner"): "tok-planner"}
	verifier, err := authz.NewAgentCredentialVerifier(func(n string) (string, bool) { v, ok := secrets[n]; return v, ok },
		authz.NewInstanceTokens(), func(context.Context, string) bool { return false }, false)
	if err != nil {
		t.Fatal(err)
	}
	idp := identity.NewChain(identity.NewLocalAccounts(identity.NewMemRegistry(), identity.NewMemRegistry()), nil)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(
		authz.UnarySurfaceInterceptor(nil),
		authz.UnaryAgentAuthInterceptor(verifier, nil),
		operator.UnaryAuthInterceptor(idp, nil, nil)))
	pb.RegisterOrchestratorServer(srv, srvImpl)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := pb.NewOrchestratorClient(conn)

	lease, err := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gw.BindLease(lease, domain.LeaseBinding{RunID: "r", AgentID: "planner"})
	req := &pb.ProposeKnowledgeRequest{LeaseId: string(lease),
		Proposal: &pb.ProposeKnowledgeRequest_Item{Item: &pb.ProposedKnowledgeItem{Kind: "case", EntityId: "c1",
			Values: []*pb.KnowledgeValue{{Predicate: "status", Type: "text", Text: "closed"}}}}}
	asPlanner := withMD("authorization", "Bearer tok-planner", "x-agent-id", "planner")

	// Without its credential the agent never reaches the gate.
	if _, err := c.ProposeKnowledge(withMD("x-agent-id", "planner"), req); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no credential: %v", err)
	}
	// Authenticated, restricted, no grant: refused, naming the verb.
	_, err = c.ProposeKnowledge(asPlanner, req)
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), domain.VerbProposeKnowledge) || prop.calls != 0 {
		t.Fatalf("no grant: %v (proposer calls %d)", err, prop.calls)
	}
	// An operator grants the verb: allowed.
	grants.Set("planner", []domain.ToolGrant{{Tool: domain.VerbProposeKnowledge}})
	resp, err := c.ProposeKnowledge(asPlanner, req)
	if err != nil || !resp.GetAccepted() || resp.GetCandidateId() != "ki_e2e" || prop.calls != 1 {
		t.Fatalf("with grant: %+v %v", resp, err)
	}
}
