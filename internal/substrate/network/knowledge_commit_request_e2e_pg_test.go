package network_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/identity"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/knowledgecommit"
	"github.com/cambrian-sh/core/internal/ontology"
	"github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// End to end, ENG-1768 (owner decision 2026-11-20, option A): an agent acting for
// a person asks, through the real Orchestrator gRPC surface and the interceptor
// chain app.go installs, for an EXISTING candidate to be committed. The real
// knowledgecommit bridge reads the candidate from the Postgres knowledge store
// under the live ontology and checks it is readable by the agent and by the
// person (the OSS default authorizer's owner fence). A candidate the person can
// read raises a knowledge_commit approval, and the person's approval commits it
// as theirs; another person's private candidate is refused exactly like an
// unknown id, and nothing is raised.
func TestPg_E2E_RequestKnowledgeCommitOnlyForACandidateThePersonCanRead(t *testing.T) {
	pool, ctx := pgscratch.Pool(t, "net_kc_request_test", pgscratch.Options{Shadow: true,
		Migrations: []string{"0040_ontology_declarations.sql", "0041_ontology_drafts.sql"}})
	const ns = "eng1768test"
	clean := func() {
		bg := context.Background()
		for _, q := range []string{`DELETE FROM resolutions WHERE namespace_id = $1`, `DELETE FROM knowledge_items WHERE namespace_id = $1`} {
			_, _ = pool.Exec(bg, q, ns) // best effort: a scratch namespace
		}
	}
	clean()
	t.Cleanup(clean)
	caseKind := domain.KindSpec{
		Kind: "rccase", Policy: domain.ResolutionPolicyDesignatedActor, ResolutionScope: domain.ResolutionScopeEntity,
		Designated: &domain.DesignatedActors{ApprovedViaCommit: true},
		Predicates: map[string]domain.ValueSpec{"status": {Type: "text", Enum: []string{"open", "closed"}}},
	}
	onto, err := ontology.Load(ctx, pool, domain.OntologyDoc{Kinds: []domain.KindSpec{caseKind}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewPgKnowledgeStore(pool, onto)
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{})
	bridge, err := knowledgecommit.New(store, onto, nil, hub, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := func(entity string, class []string) string {
		id, _, err := store.PutItem(ctx, domain.KnowledgeItem{NamespaceID: ns, Kind: "rccase", EntityID: entity,
			AssertedBy: "agent:scout", AssertedAt: time.Now().UTC(), Classification: class,
			SourceRef: domain.KnowledgeProposalSourceRefPrefix + "agent:scout:" + entity,
			Values:    []domain.StatementValue{{Predicate: "status", Type: "text", Text: "closed"}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(id)
	}
	readable := candidate("rccase/open", []string{"ops"})
	bobsPrivate := candidate("rccase/bob", []string{domain.OwnerTag("bob")})

	// The agent plane, wired as app.go wires it.
	gw := network.NewLLMGateway(config.ExecutionConfig{
		Session: config.SessionConfig{SessionTokenTTLMultiplier: 5},
		LLM:     config.LLMConfig{LLMGatewayMaxConcurrency: 4},
	}, llm.NewSpendMeter(0, nil, nil))
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("planner", []domain.ToolGrant{{Tool: domain.VerbProposeKnowledge}})
	srvImpl := &network.Server{LLMGateway: gw, Knowledge: bridge, ToolExecutor: &domain.ToolExecutor{Grants: grants},
		RunPrincipal: func(context.Context, domain.LeaseBinding) string { return "alice" }}
	secrets := map[string]string{authz.AgentTokenSecretName("planner"): "tok-planner"}
	verifier, err := authz.NewAgentCredentialVerifier(func(n string) (string, bool) { v, ok := secrets[n]; return v, ok },
		authz.NewInstanceTokens(), func(context.Context, string) (bool, error) { return false, nil }, false)
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
	asPlanner := withMD("authorization", "Bearer tok-planner", "x-agent-id", "planner")

	// Another person's private candidate: refused, worded exactly as an unknown id,
	// and no approval exists.
	for _, id := range []string{bobsPrivate, "ki_does_not_exist"} {
		resp, err := c.RequestKnowledgeCommit(asPlanner, &pb.RequestKnowledgeCommitRequest{LeaseId: string(lease), CandidateId: id, Reason: "confirm"})
		if err != nil || resp.GetAccepted() || !strings.Contains(resp.GetDetail(), "unknown or not readable by agent:planner") {
			t.Fatalf("candidate %s: %+v %v — want a refusal that says nothing about whether it exists", id, resp, err)
		}
	}
	if raised, _ := hub.List(ctx, domain.ApprovalQuery{}); len(raised) != 0 {
		t.Fatalf("a refused request raised %d approval(s)", len(raised))
	}

	// The readable candidate: a knowledge_commit approval for alice, naming it.
	resp, err := c.RequestKnowledgeCommit(asPlanner, &pb.RequestKnowledgeCommitRequest{LeaseId: string(lease), CandidateId: readable, Reason: "confirm"})
	if err != nil || !resp.GetAccepted() || resp.GetApprovalId() == "" {
		t.Fatalf("readable candidate: %+v %v", resp, err)
	}
	ap, err := hub.Get(ctx, resp.GetApprovalId())
	if err != nil || ap.Kind != domain.ApprovalKindKnowledgeCommit || ap.Object != readable || ap.Beneficiary != domain.UserPrincipal("alice") {
		t.Fatalf("approval = %+v %v", ap, err)
	}
	// Alice approves; the commit is asserted by her, never by the agent.
	if _, err := hub.Answer(ctx, ap.ID, domain.ApprovalAnswer{Approved: true, By: domain.Approver{Principal: domain.UserPrincipal("alice")}}); err != nil {
		t.Fatal(err)
	}
	var assertedBy string
	if err := pool.QueryRow(ctx, `SELECT asserted_by FROM knowledge_items WHERE namespace_id = $1 AND entity_id = 'rccase/open' AND source_ref = $2`,
		ns, domain.KnowledgeCommitSourceRefPrefix+ap.ID).Scan(&assertedBy); err != nil || assertedBy != "user:alice" {
		t.Fatalf("committed item asserted by %q (%v), want user:alice", assertedBy, err)
	}
}
