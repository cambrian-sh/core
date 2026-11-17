package agentmgr

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/supervision/signal"

	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ADR-0157 S3 (D4) through the REAL boot path: GetOrBootInstance /
// BootDedicatedInstance → bootAgentInstance → mint → spawn. The agent is this
// test binary in its "token" shape (TestMain): it writes what credential it
// received, and where, next to its socket, then serves grpc.health on it.
//
// No test here prints a token: values are compared, never logged.

// tokenProbeReport is what the token-probe agent saw.
type tokenProbeReport struct {
	Args    []string `json:"args"`
	Token   string   `json:"token"`
	AgentID string   `json:"agent_id"`
}

// runTokenProbeAgent is the token-probe agent (TestMain, AGENTMGR_CHAOS_HELPER=token).
func runTokenProbeAgent() {
	var sock string
	for i, a := range os.Args {
		if a == "--socket" && i+1 < len(os.Args) {
			sock = os.Args[i+1]
		}
	}
	rep := tokenProbeReport{Args: os.Args[1:], Token: os.Getenv(AgentTokenEnv), AgentID: os.Getenv(AgentIDEnv)}
	b, _ := json.Marshal(rep)
	if err := os.WriteFile(sock+".token.json", b, 0o600); err != nil {
		os.Exit(4)
	}
	lis, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(3)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, health.NewServer())
	_ = srv.Serve(lis)
}

func tokenProbeAgent(id string) *domain.AgentDefinition {
	return &domain.AgentDefinition{ID: id, Runtime: domain.RuntimeBinary, ExecPath: os.Args[0]}
}

func tokenProbeManager(t *testing.T) *InstanceManager {
	t.Helper()
	im := NewInstanceManager("", "127.0.0.1:1")
	im.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=token"})
	t.Cleanup(func() { _ = im.KillAllAgents(context.Background()) })
	return im
}

// readTokenReport waits for the agent's report (it is written before the
// socket, so it exists once the boot returned).
func readTokenReport(t *testing.T, inst *domain.Instance) tokenProbeReport {
	t.Helper()
	b, err := os.ReadFile(inst.SocketPath + ".token.json")
	if err != nil {
		t.Fatalf("token probe report: %v", err)
	}
	_ = os.Remove(inst.SocketPath + ".token.json")
	var rep tokenProbeReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func skipShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("launches a real subprocess; skipped in -short")
	}
}

// The token arrives in the environment, never on argv (closes
// T-20261115-per-instance-auth-token-is-passed-on-argv-but), it is the live
// instance token in the kernel's registry, and both checks accept it: the
// agent-plane verifier and the SignalStream gate (D4.3).
func TestInstanceToken_InEnvNotArgv_AndBothChecksAcceptIt(t *testing.T) {
	skipShort(t)
	im := tokenProbeManager(t)
	def := tokenProbeAgent("token_probe")
	inst, err := im.GetOrBootInstance(context.Background(), def, "")
	if err != nil {
		t.Fatal(err)
	}
	rep := readTokenReport(t, inst)

	if len(rep.Token) != 64 {
		t.Fatalf("env %s: token of %d hex chars, want 64 (32 bytes)", AgentTokenEnv, len(rep.Token))
	}
	if rep.AgentID != def.ID {
		t.Fatalf("env %s = %q, want %q", AgentIDEnv, rep.AgentID, def.ID)
	}
	for _, a := range rep.Args {
		if a == "--auth-token" || strings.Contains(a, rep.Token) {
			t.Fatal("the credential reached argv")
		}
	}
	id, ok := im.Tokens().Lookup(rep.Token)
	if !ok || id.AgentID != def.ID || id.InstanceID != inst.ID {
		t.Fatalf("token not registered for this instance: ok=%v id=%+v", ok, id)
	}

	// The SignalStream gate (signal.ValidateToken → FindInstanceByToken).
	ctx := bearerCtx(rep.Token, def.ID)
	got, err := signal.ValidateToken(ctx, im)
	if err != nil || got == nil || got.ID != inst.ID {
		t.Fatalf("SignalStream gate refused the live token: inst=%v err=%v", got, err)
	}
	// The agent-plane verifier, built on the same registry.
	v := strictVerifier(t, im, nil)
	if _, err := v.Authenticate(ctx, "/cambrian.Orchestrator/ListTools", nil); err != nil {
		t.Fatalf("agent plane refused the live token: %v", err)
	}

	// After eviction BOTH refuse it at once (instant revocation).
	im.EvictInstance(inst.ID)
	if _, err := signal.ValidateToken(ctx, im); err == nil {
		t.Fatal("SignalStream gate accepted an evicted instance's token")
	}
	if _, err := v.Authenticate(ctx, "/cambrian.Orchestrator/ListTools", nil); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("agent plane accepted an evicted instance's token: %v", err)
	}
}

// A restart invalidates only the restarted instance's token; a sibling
// instance of the same agent keeps its own (D4.2 — tokens are per instance).
func TestInstanceToken_RestartInvalidatesOldToken_SiblingKeepsItsOwn(t *testing.T) {
	skipShort(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	im := tokenProbeManager(t)
	def := tokenProbeAgent("token_probe")
	ctx := context.Background()

	first, err := im.GetOrBootInstance(ctx, def, "")
	if err != nil {
		t.Fatal(err)
	}
	firstTok := readTokenReport(t, first).Token
	sibling, err := im.BootDedicatedInstance(ctx, def)
	if err != nil {
		t.Fatal(err)
	}
	siblingTok := readTokenReport(t, sibling).Token
	if firstTok == siblingTok {
		t.Fatal("two instances of one agent share a token")
	}

	// Restart the indexed instance: evict + boot again.
	im.EvictInstance(first.ID)
	second, err := im.GetOrBootInstance(ctx, def, "")
	if err != nil {
		t.Fatal(err)
	}
	secondTok := readTokenReport(t, second).Token

	if _, ok := im.Tokens().Lookup(firstTok); ok {
		t.Fatal("the restarted instance's old token is still valid")
	}
	if id, ok := im.Tokens().Lookup(siblingTok); !ok || id.InstanceID != sibling.ID {
		t.Fatal("the sibling instance lost its token when another instance restarted")
	}
	if id, ok := im.Tokens().Lookup(secondTok); !ok || id.InstanceID != second.ID || secondTok == firstTok {
		t.Fatal("the restarted instance did not get a fresh token")
	}

	im.EvictInstance(sibling.ID)
	im.EvictInstance(second.ID)
	waitTokens(t, im, 0)
	waitNoExits(t, im)
}

// Every teardown path revokes: EvictAgent (all instances of the agent),
// ReleaseInstance (JIT), KillAllAgents, and the process exiting on its own.
func TestInstanceToken_EveryTeardownRevokes(t *testing.T) {
	skipShort(t)
	ctx := context.Background()

	t.Run("EvictAgent", func(t *testing.T) {
		im := tokenProbeManager(t)
		def := tokenProbeAgent("token_probe")
		a, err := im.GetOrBootInstance(ctx, def, "")
		if err != nil {
			t.Fatal(err)
		}
		_ = readTokenReport(t, a)
		// A second indexed instance of the same agent (pool shape).
		b, err := im.bootAgentInstance(ctx, def, true)
		if err != nil {
			t.Fatal(err)
		}
		_ = readTokenReport(t, b)
		if im.Tokens().Len() != 2 {
			t.Fatalf("live tokens = %d, want 2", im.Tokens().Len())
		}
		im.EvictAgent(def.ID)
		if n := im.Tokens().Len(); n != 0 {
			t.Fatalf("EvictAgent left %d live tokens", n)
		}
	})
	t.Run("ReleaseInstance", func(t *testing.T) {
		im := tokenProbeManager(t)
		inst, err := im.GetOrBootInstance(ctx, tokenProbeAgent("token_probe"), "")
		if err != nil {
			t.Fatal(err)
		}
		_ = readTokenReport(t, inst)
		im.ReleaseInstance(inst.ID)
		if n := im.Tokens().Len(); n != 0 {
			t.Fatalf("ReleaseInstance left %d live tokens", n)
		}
	})
	t.Run("KillAllAgents", func(t *testing.T) {
		im := tokenProbeManager(t)
		inst, err := im.GetOrBootInstance(ctx, tokenProbeAgent("token_probe"), "")
		if err != nil {
			t.Fatal(err)
		}
		_ = readTokenReport(t, inst)
		if err := im.KillAllAgents(ctx); err != nil {
			t.Fatal(err)
		}
		if n := im.Tokens().Len(); n != 0 {
			t.Fatalf("KillAllAgents left %d live tokens", n)
		}
	})
	t.Run("ProcessExit", func(t *testing.T) {
		im := tokenProbeManager(t)
		inst, err := im.GetOrBootInstance(ctx, tokenProbeAgent("token_probe"), "")
		if err != nil {
			t.Fatal(err)
		}
		tok := readTokenReport(t, inst).Token
		// The process dies without the manager tearing it down (a crash).
		if err := im.GetCmd(inst.ID).Process.Kill(); err != nil {
			t.Fatal(err)
		}
		waitTokens(t, im, 0)
		if im.FindInstanceByToken(tok) != nil {
			t.Fatal("a crashed instance's token still finds the instance")
		}
	})
}

// A boot that fails after the token was minted leaves no live token.
func TestInstanceToken_FailedBootRevokes(t *testing.T) {
	skipShort(t)
	im := NewInstanceManager("", "127.0.0.1:1")
	if _, err := im.GetOrBootInstance(context.Background(), noSocketAgent(), ""); err == nil {
		t.Fatal("boot of an agent that exits at once must fail")
	}
	if n := im.Tokens().Len(); n != 0 {
		t.Fatalf("a failed boot left %d live tokens", n)
	}
}

// Building a command issues no credential (only the boot path mints), so a
// command built and never started cannot leave a usable token behind.
func TestBuildAgentCmd_IssuesNoCredential(t *testing.T) {
	im := NewInstanceManager("python", "localhost:0")
	def := &domain.AgentDefinition{ID: "analyst_agent", Runtime: domain.RuntimePython, ExecPath: "agent.py"}
	cmd, err := im.buildAgentCmd(def, &domain.Instance{ID: "inst-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if env := strings.Join(cmd.Env, "\n"); strings.Contains(env, AgentTokenEnv) {
		t.Fatal("buildAgentCmd put a credential in the environment")
	}
	if args := strings.Join(cmd.Args, " "); strings.Contains(args, "--auth-token") {
		t.Fatal("buildAgentCmd put a credential on argv")
	}
	if im.Tokens().Len() != 0 {
		t.Fatal("buildAgentCmd minted a token")
	}
}

func bearerCtx(token, agentID string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token, "x-agent-id", agentID))
}

// strictVerifier is the production verifier over this manager's registry.
func strictVerifier(t *testing.T, im *InstanceManager, stored map[string]string) *authz.AgentCredentialVerifier {
	t.Helper()
	v, err := authz.NewAgentCredentialVerifier(
		func(n string) (string, bool) { s, ok := stored[n]; return s, ok },
		im.Tokens(), func(context.Context, string) bool { return true }, false)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func waitTokens(t *testing.T, im *InstanceManager, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for im.Tokens().Len() != want {
		if time.Now().After(deadline) {
			t.Fatalf("live tokens = %d, want %d", im.Tokens().Len(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitNoExits waits until every child's Wait goroutine has returned, so the
// goleak check sees only goroutines this test failed to end.
func waitNoExits(t *testing.T, im *InstanceManager) {
	t.Helper()
	im.mu.Lock()
	var pending []*processExit
	for _, pe := range im.exits {
		pending = append(pending, pe)
	}
	im.mu.Unlock()
	for _, pe := range pending {
		select {
		case <-pe.done:
		case <-time.After(10 * time.Second):
			t.Fatal("a child was not reaped")
		}
	}
}

// A daemon may emit its first signal while its boot still waits for the socket
// and health probe: the token is live from the start, so the SignalStream gate
// answers from the token before the instance is registered.
func TestFindInstanceByToken_AnswersWhileBooting(t *testing.T) {
	im := NewInstanceManager("python", "localhost:0")
	tok, err := im.tokens.Mint("pulse_agent", "inst-booting")
	if err != nil {
		t.Fatal(err)
	}
	got := im.FindInstanceByToken(tok)
	if got == nil || got.ID != "inst-booting" || got.AgentID != "pulse_agent" {
		t.Fatalf("booting instance not found by its token: %+v", got)
	}
	im.tokens.Revoke("inst-booting")
	if im.FindInstanceByToken(tok) != nil {
		t.Fatal("a revoked token still finds an instance")
	}
}
