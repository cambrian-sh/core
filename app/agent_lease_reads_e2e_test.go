package app

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentplane"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/substrate/network"
)

// T-20261118-chat-agent-and-composed-worker-memory-reads-are end to end: a turn
// dispatched by the kernel under a lease reaches a REAL Python SDK agent
// (CognitiveAgent.serve, the runtime's dispatch_run), whose recall and skill
// menu go back over its credential-authenticated channel (sandbox on, the
// agent's kernel-only unix socket, the per-instance token) through the
// production surface + agent-credential interceptors to network.Server
// QueryMemory and ListSkills.
//
// The decision point is a stand-in for premium's ENG-518 rule (core may not
// import premium): a credential-authenticated agent-plane read with no task
// session, conversation owner or ordering conversation on ctx is refused
// no_lease. Asserted: neither the seed recall (react._safe_recall), an author's
// lease-less self.memory.recall, nor the loop's skill menu
// (react._list_system_skills) is refused; the recall returns the hit and the
// menu the skill — both decided with the turn's conversation owner, resolved by
// the kernel from the lease.
//
// Needs the kernel-managed env's interpreter with the agent SDK of this change
// (python-env build --sdk-dir): LEASE_READS_E2E_PY.
func TestE2E_AgentTurnPresentsItsLeaseOnRecallAndMenus(t *testing.T) {
	py := os.Getenv("LEASE_READS_E2E_PY")
	if py == "" {
		t.Skip("set LEASE_READS_E2E_PY to run the lease-reads e2e")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	base, err := os.MkdirTemp("", "q72e")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(base, 0o755)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	code := filepath.Join(base, "code")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(code, "q72_turn.py"), []byte(leaseReadsProbePy), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := metabolism.NewInMemoryRegistry()
	turnAgent := &domain.AgentDefinition{ID: "q72_turn", Runtime: domain.RuntimePython, ExecPath: "q72_turn.py", Dir: code}
	reg.SetAgent(*turnAgent)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mgr := agentManagerWithPython(t, agentmgr.NewAgentManager(reg, lis.Addr().String(), nil), py)
	im := mgr.InstanceManager
	im.SetAgentBootTimeout(60 * time.Second)
	kernelDir := filepath.Join(base, "kernel")
	if err := os.MkdirAll(filepath.Join(kernelDir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	im.SetAgentContainment(agentmgr.AgentContainment{Sandbox: agentmgr.NewAgentSandbox(agentmgr.AgentSandboxOptions{
		WorkRoot: filepath.Join(base, "w"), KernelDir: kernelDir, KernelStatePaths: []string{filepath.Join(kernelDir, "configs")},
	})})
	sockets := newAgentSocketServer()
	im.SetAgentTransport(agentmgr.AgentTransport{KernelSocket: sockets.Open})

	exec := config.ExecutionConfig{
		Session: config.SessionConfig{SessionTokenTTLMultiplier: 5},
		LLM:     config.LLMConfig{LLMGatewayMaxConcurrency: 4},
	}
	gw := network.NewLLMGateway(exec, llm.NewSpendMeter(0, nil, nil))
	az := &leaseRuleAuthorizer{}
	skills := domain.NewInMemorySkillRegistry()
	skills.Register(domain.Skill{Name: "warehouse-lookup", Description: "Looks up a warehouse.", Instructions: "BODY"})
	srvImpl := &network.Server{
		LLMGateway: gw, Manager: mgr, Authz: az, ExecCfg: exec,
		MemorySearcher: decidingSearcher{az: az},
		SkillRegistry:  skills,
		ConversationOwner: func(_ context.Context, b domain.LeaseBinding) string {
			return strings.TrimPrefix(b.ConversationID, "conv-of-")
		},
	}

	verifier, err := authz.NewAgentCredentialVerifier(nil, im.Tokens(), kernelSpawnedAgent(reg), false)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(authz.UnarySurfaceInterceptor(nil), authz.UnaryAgentAuthInterceptor(verifier, nil)),
		grpc.ChainStreamInterceptor(authz.StreamSurfaceInterceptor(nil), authz.StreamAgentAuthInterceptor(verifier, nil)),
	)
	pb.RegisterOrchestratorServer(srv, srvImpl)
	sockets.start(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		_ = im.KillAllAgents(context.Background())
		srv.Stop()
		sockets.stop()
	})

	// The turn's lease, bound as a chat turn binds it (TurnService): the agent and
	// the conversation, no task session.
	lease, err := gw.Acquire(ctx, domain.StepAllocation{}, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gw.BindLease(lease, domain.LeaseBinding{AgentID: "q72_turn", ConversationID: "conv-of-dana", OriginMessageID: "msg-1"})

	// The kernel dispatches the turn (the agent transport every lane uses).
	transport := agentplane.New(mgr)
	resp, err := transport.CallAgent(ctx, "q72_turn", &domain.Handoff{
		Payload: &domain.Payload{Type: "text", Data: []byte("what is the warehouse codename?")},
		Context: map[string]string{"_session_token_id": string(lease)},
	}, "")
	if err != nil {
		t.Fatalf("dispatch the turn: %v", err)
	}
	var out struct {
		Seed   []string `json:"seed"`
		Author []string `json:"author"`
		Skills []string `json:"skills"`
		Error  string   `json:"error"`
	}
	if err := json.Unmarshal(resp.Payload.Data, &out); err != nil || out.Error != "" {
		t.Fatalf("agent answer %q: %v", resp.Payload.Data, err)
	}

	az.mu.Lock()
	defer az.mu.Unlock()
	if az.credentialed == 0 {
		t.Fatal("no read reached the decision point over the agent's credential (the e2e did not exercise the authenticated path)")
	}
	if len(az.denied) != 0 {
		t.Fatalf("reads refused no_lease: %v", az.denied)
	}
	if len(out.Seed) != 1 || out.Seed[0] != "Kingfisher" || len(out.Author) != 1 {
		t.Fatalf("recall: seed %v author %v, want the hit", out.Seed, out.Author)
	}
	if len(out.Skills) != 1 || out.Skills[0] != "warehouse-lookup" {
		t.Fatalf("skill menu %v, want [warehouse-lookup]", out.Skills)
	}
	for _, o := range az.owners {
		if o != "dana" {
			t.Fatalf("a read was decided without the turn's conversation owner: %v", az.owners)
		}
	}
}

// leaseRuleAuthorizer mirrors premium's refuseLeaseless (ENG-518) and records
// what each agent-plane read was decided with.
type leaseRuleAuthorizer struct {
	domain.AllowAllAuthorizer
	mu           sync.Mutex
	denied       []string
	owners       []string
	credentialed int
}

func (a *leaseRuleAuthorizer) ReadFilter(ctx context.Context, p domain.PrincipalRef, s domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	if p.Kind == domain.PrincipalAgent && s.Kind == domain.SurfaceAgent && bearerAuthenticated(ctx) {
		_, hasSession := domain.SessionIDFromContext(ctx)
		owner := domain.ConversationOwnerFromContext(ctx)
		a.mu.Lock()
		a.credentialed++
		a.owners = append(a.owners, owner)
		if !hasSession && owner == "" && domain.ConversationIDFromContext(ctx) == "" {
			a.denied = append(a.denied, p.String())
			a.mu.Unlock()
			return nil, domain.AccessDecision{Principal: p, Surface: s, Reason: domain.ReasonNoLease}
		}
		a.mu.Unlock()
	}
	return a.AllowAllAuthorizer.ReadFilter(ctx, p, s)
}

func bearerAuthenticated(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	for _, v := range md.Get("authorization") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "bearer ") {
			return true
		}
	}
	return false
}

// decidingSearcher asks the decision point exactly as QueryService.readFilter
// does (the caller as an agent principal on the request's surface) and returns
// one hit only when the read is authorized.
type decidingSearcher struct{ az domain.Authorizer }

func (d decidingSearcher) Search(ctx context.Context, _, callerID string) ([]domain.SearchResult, error) {
	pred, _ := d.az.ReadFilter(ctx, domain.AgentPrincipal(callerID), domain.SurfaceFromContext(ctx))
	if pred == nil {
		return nil, nil
	}
	return []domain.SearchResult{{Document: domain.Document{ID: "doc-1", Text: "Kingfisher"}, Score: 0.9}}, nil
}

func (d decidingSearcher) SearchActions(ctx context.Context, q, c string) ([]domain.SearchResult, error) {
	return d.Search(ctx, q, c)
}

func (d decidingSearcher) SearchProcedures(context.Context, string, string) ([]domain.SearchResult, error) {
	return nil, nil
}

// leaseReadsProbePy: an SDK CognitiveAgent served by the SDK runtime. Its run()
// makes the reads a reasoning turn makes, none of which the author passes a
// lease to by hand except the seed recall (which forwards task.session_token_id
// exactly as run_think does).
const leaseReadsProbePy = `
import json, os
from cambrian_agent_sdk import CognitiveAgent
from cambrian_agent_sdk.react import _list_system_skills, _safe_recall

class Turn(CognitiveAgent):
    def run(self, task):
        try:
            seed = _safe_recall(self, task.text, task.session_token_id)
            author = self.memory.recall(task.text)
            skills = _list_system_skills(self, query=task.text)
            return json.dumps({"seed": [h["text"] for h in seed], "author": [h["text"] for h in author],
                               "skills": [s["name"] for s in skills]})
        except Exception as e:
            return json.dumps({"error": repr(e)})

Turn(agent_id=os.environ.get("CAMBRIAN_AGENT_ID", "q72_turn")).serve()
`
