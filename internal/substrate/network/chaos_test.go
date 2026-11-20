//go:build chaos

// Real-service chaos scenarios, run by `make chaos-real` (release-gate). They drive a
// RUNNING kernel over the agent gRPC plane and inject faults through toxiproxy's HTTP
// API (scripts/chaos-compose.yml loads scripts/toxiproxy-config.json at start: proxies
// `postgres` on :5432 and `ollama` on :11434).
//
// Preconditions the harness does NOT provide yet
// (T-20261031-chaos-real-cannot-pass-and-injects-no-fault):
//   - a kernel serving CAMBRIAN_CHAOS_KERNEL_ADDR (default localhost:50051) whose
//     database DSN points at the toxiproxy `postgres` listener (localhost:5432) and
//     whose LLM provider points at the toxiproxy `ollama` listener (localhost:11434).
//     The compose file starts no kernel; until it does, start one by hand with that
//     config. Faults injected on a proxy the kernel does not use prove nothing.
//
// Every scenario FAILS (never skips) when the kernel or toxiproxy is unreachable: the
// old guard skipped on a grpc.NewClient error, which a lazy client never returns, and a
// skip in a release gate is a vacuous pass either way.
package network_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	defaultKernelAddr    = "localhost:50051"
	defaultToxiproxyAddr = "http://localhost:8474"
	// probeTimeout bounds the reachability checks so a missing service fails the
	// scenario in seconds, not at the first Execute's deadline.
	probeTimeout = 2 * time.Second
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// chaosKernel dials the kernel and proves it is SERVING with a real gRPC health check
// (the kernel registers grpc_health_v1, app/app.go). It fails the test otherwise.
func chaosKernel(t *testing.T) pb.OrchestratorClient {
	t.Helper()
	addr := envOr("CAMBRIAN_CHAOS_KERNEL_ADDR", defaultKernelAddr)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("chaos-real: bad kernel address %q: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("chaos-real: no kernel SERVING at %s (%v). scripts/chaos-compose.yml starts no kernel; "+
			"start one with its DSN on toxiproxy :5432 and its LLM on toxiproxy :11434, or set CAMBRIAN_CHAOS_KERNEL_ADDR", addr, err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("chaos-real: kernel at %s reports %s, not SERVING", addr, resp.GetStatus())
	}
	return pb.NewOrchestratorClient(conn)
}

// toxiproxy is a minimal client for the toxiproxy HTTP API (v2): enough to disable a
// proxy and to add/remove a toxic. No new module dependency for three calls.
type toxiproxy struct {
	base string
	http *http.Client
}

func chaosToxiproxy(t *testing.T) *toxiproxy {
	t.Helper()
	tp := &toxiproxy{base: envOr("CAMBRIAN_CHAOS_TOXIPROXY_URL", defaultToxiproxyAddr), http: &http.Client{Timeout: probeTimeout}}
	if err := tp.do(http.MethodGet, "/version", nil); err != nil {
		t.Fatalf("chaos-real: toxiproxy API not reachable at %s (%v); run docker compose -f scripts/chaos-compose.yml up -d", tp.base, err)
	}
	return tp
}

func (tp *toxiproxy) do(method, path string, body any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, tp.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := tp.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// disable turns a proxy off (every connection through it is closed and refused —
// a TCP blackout) or back on. The restore is registered as a cleanup so a failing
// scenario never leaves the fault in place for the next one.
func (tp *toxiproxy) disable(t *testing.T, proxy string) {
	t.Helper()
	if err := tp.do(http.MethodPost, "/proxies/"+proxy, map[string]any{"enabled": false}); err != nil {
		t.Fatalf("chaos-real: disable proxy %s: %v", proxy, err)
	}
	t.Cleanup(func() {
		if err := tp.do(http.MethodPost, "/proxies/"+proxy, map[string]any{"enabled": true}); err != nil {
			t.Errorf("chaos-real: RE-ENABLE proxy %s failed, later scenarios run against a blackout: %v", proxy, err)
		}
	})
}

// addToxic adds a downstream toxic and registers its removal.
func (tp *toxiproxy) addToxic(t *testing.T, proxy, name, typ string, attrs map[string]any) {
	t.Helper()
	body := map[string]any{"name": name, "type": typ, "stream": "downstream", "toxicity": 1.0, "attributes": attrs}
	if err := tp.do(http.MethodPost, "/proxies/"+proxy+"/toxics", body); err != nil {
		t.Fatalf("chaos-real: add toxic %s on %s: %v", name, proxy, err)
	}
	t.Cleanup(func() {
		if err := tp.do(http.MethodDelete, "/proxies/"+proxy+"/toxics/"+name, nil); err != nil {
			t.Errorf("chaos-real: REMOVE toxic %s on %s failed, later scenarios run with it: %v", name, proxy, err)
		}
	})
}

func chaosHandoff(id, task string) *pb.Handoff {
	return &pb.Handoff{
		Id:        id,
		FromAgent: "chaos-test",
		ToAgent:   "chaos-test",
		Payload:   &pb.Object{Id: "p", Type: "text", Data: []byte(task)},
	}
}

// executeBounded runs Execute under a client deadline and reports whether the KERNEL
// answered (with a result or a status of its own) before that deadline. A call that
// is still open when the client deadline fires is the hang these scenarios exist to
// catch; any answer the kernel gives, success or error, is not a hang.
func executeBounded(client pb.OrchestratorClient, in *pb.Handoff, deadline time.Duration) (resp *pb.Handoff, hung bool, err error) {
	ctx, cancel := context.WithTimeout(chaosAgentContext(), deadline)
	defer cancel()
	resp, err = client.Execute(ctx, in)
	return resp, errors.Is(ctx.Err(), context.DeadlineExceeded), err
}

// chaosAgentContext carries the ADR-0130 agent credential when one is configured
// (CAMBRIAN_CHAOS_AGENT_ID + CAMBRIAN_CHAOS_AGENT_TOKEN, the agent:<id>:token secret).
// Without one the call relies on the server.insecure_localhost loopback waiver.
func chaosAgentContext() context.Context {
	ctx := context.Background()
	if id := os.Getenv("CAMBRIAN_CHAOS_AGENT_ID"); id != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-agent-id", id)
		if tok := os.Getenv("CAMBRIAN_CHAOS_AGENT_TOKEN"); tok != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
		}
	}
	return ctx
}

// requireBaseline proves, BEFORE any fault, that this kernel and credential can
// complete a request. Without it a scenario's "the kernel answered" could be an
// Unauthenticated or FailedPrecondition refusal that has nothing to do with the fault.
func requireBaseline(t *testing.T, client pb.OrchestratorClient, deadline time.Duration) {
	t.Helper()
	resp, hung, err := executeBounded(client, chaosHandoff("chaos-baseline", "say hello"), deadline)
	if hung || err != nil || resp == nil {
		t.Fatalf("chaos-real: baseline Execute failed before any fault was injected (hung=%v): %v — "+
			"fix the kernel/credential setup first (CAMBRIAN_CHAOS_AGENT_ID / CAMBRIAN_CHAOS_AGENT_TOKEN)", hung, err)
	}
}

func envDuration(t *testing.T, key string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("chaos-real: %s=%q: %v", key, v, err)
	}
	return d
}

// TestChaosReal_PostgresTcpBlackout: with the database unreachable the kernel must
// still ANSWER the request (degraded or with an error status) instead of hanging, and
// must serve normally again once the database is back.
func TestChaosReal_PostgresTcpBlackout(t *testing.T) {
	client := chaosKernel(t)
	tp := chaosToxiproxy(t)
	deadline := envDuration(t, "CAMBRIAN_CHAOS_EXECUTE_DEADLINE", 2*time.Minute)
	requireBaseline(t, client, deadline)

	tp.disable(t, "postgres")
	_, hung, err := executeBounded(client, chaosHandoff("chaos-pg-blackout", "say hello"), deadline)
	if hung {
		t.Fatalf("Execute did not answer within %s while Postgres was blacked out (hang): %v", deadline, err)
	}
	t.Logf("under pg blackout the kernel answered: code=%s err=%v", status.Code(err), err)

	// Restore before the recovery check (the cleanup re-enables too; doing it now is
	// what makes the recovery assertion meaningful).
	if err := tp.do(http.MethodPost, "/proxies/postgres", map[string]any{"enabled": true}); err != nil {
		t.Fatalf("chaos-real: re-enable postgres: %v", err)
	}
	resp, hung, err := executeBounded(client, chaosHandoff("chaos-pg-recovered", "say hello"), deadline)
	if hung || err != nil || resp == nil {
		t.Fatalf("kernel did not recover after the Postgres blackout ended: hung=%v err=%v", hung, err)
	}
}

// TestChaosReal_LLMStreamingHang: the LLM upstream accepts the connection and then
// sends nothing (toxiproxy `timeout` toxic with timeout 0 holds the data forever).
// The kernel's own LLM timeouts must end the request — the client deadline (default
// 10m, above the kernel's llm_timeout_max_ms default of 300 s) must not be what ends it.
func TestChaosReal_LLMStreamingHang(t *testing.T) {
	client := chaosKernel(t)
	tp := chaosToxiproxy(t)
	deadline := envDuration(t, "CAMBRIAN_CHAOS_LLM_HANG_DEADLINE", 10*time.Minute)
	requireBaseline(t, client, envDuration(t, "CAMBRIAN_CHAOS_EXECUTE_DEADLINE", 2*time.Minute))

	tp.addToxic(t, "ollama", "chaos_llm_stall", "timeout", map[string]any{"timeout": 0})
	_, hung, err := executeBounded(client, chaosHandoff("chaos-llm-hang", "say hello"), deadline)
	if hung {
		t.Fatalf("Execute was still open after %s with a stalled LLM stream: the kernel bounds no LLM call on this path: %v", deadline, err)
	}
	t.Logf("with a stalled LLM the kernel answered: code=%s err=%v", status.Code(err), err)
}

// chaosOperator dials the operator plane on the kernel address with an operator
// credential: CAMBRIAN_CHAOS_OPERATOR_TOKEN (a bearer token), else
// CAMBRIAN_CHAOS_OPERATOR_USER + CAMBRIAN_CHAOS_OPERATOR_PASSWORD through Login. It
// fails (never skips) without one: the agent SIGKILL scenario needs the operator
// plane to find the worker's pid and to send the person's turn.
func chaosOperator(t *testing.T) (pb.OperatorConsoleClient, context.Context) {
	t.Helper()
	addr := envOr("CAMBRIAN_CHAOS_KERNEL_ADDR", defaultKernelAddr)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("chaos-real: bad kernel address %q: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	op := pb.NewOperatorConsoleClient(conn)
	tok := os.Getenv("CAMBRIAN_CHAOS_OPERATOR_TOKEN")
	if tok == "" {
		user, pass := os.Getenv("CAMBRIAN_CHAOS_OPERATOR_USER"), os.Getenv("CAMBRIAN_CHAOS_OPERATOR_PASSWORD")
		if user == "" || pass == "" {
			t.Fatal("chaos-real: the agent SIGKILL scenario needs an operator credential: set CAMBRIAN_CHAOS_OPERATOR_TOKEN, " +
				"or CAMBRIAN_CHAOS_OPERATOR_USER and CAMBRIAN_CHAOS_OPERATOR_PASSWORD")
		}
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		resp, err := op.Login(ctx, &pb.LoginRequest{Username: user, Password: pass})
		if err != nil {
			t.Fatalf("chaos-real: operator login failed: %v", err)
		}
		tok = resp.GetToken()
	}
	return op, metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+tok)
}

// chatWorkers returns the pids of every chat pool worker (the agent the lane runs,
// CAMBRIAN_CHAOS_CHAT_AGENT, default chat_agent) as OperatorConsole/ListAgentInstances
// reports them (contract 0117). Every one is killed, so the worker serving the turn
// is among them whatever the pool size (ENG-2266).
func chatWorkers(t *testing.T, op pb.OperatorConsoleClient, ctx context.Context) []int {
	t.Helper()
	agent := envOr("CAMBRIAN_CHAOS_CHAT_AGENT", "chat_agent")
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	resp, err := op.ListAgentInstances(cctx, &pb.ListAgentInstancesOpRequest{AgentId: agent})
	if err != nil {
		t.Fatalf("chaos-real: ListAgentInstances: %v (a kernel at contract 0117 or later, with the chat pool on, is required)", err)
	}
	var pids []int
	for _, in := range resp.GetInstances() {
		if in.GetPid() > 0 && in.GetStreamId() != "" {
			pids = append(pids, int(in.GetPid()))
		}
	}
	return pids
}

// awaitWorkers waits until there are at least min chat workers none of which is in
// skip (the respawned ones), bounded by within, and returns them. A second round
// waits for EVERY killed worker's replacement, so it kills the one the re-sent turn
// runs on, whichever stream that is.
func awaitWorkers(t *testing.T, op pb.OperatorConsoleClient, ctx context.Context, skip []int, min int, within time.Duration) []int {
	t.Helper()
	until := time.Now().Add(within)
	for {
		var fresh []int
		for _, pid := range chatWorkers(t, op, ctx) {
			if !slices.Contains(skip, pid) {
				fresh = append(fresh, pid)
			}
		}
		if len(fresh) >= min && len(fresh) > 0 {
			return fresh
		}
		if time.Now().After(until) {
			t.Fatalf("chaos-real: no live chat worker (other than %v) within %s", skip, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// killPid is the EXTERNAL kill: an OS signal from outside the kernel (SIGKILL on
// Unix, TerminateProcess on Windows), never the kernel's own instance manager.
func killPid(t *testing.T, pid int) {
	t.Helper()
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("chaos-real: find pid %d: %v", pid, err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("chaos-real: kill pid %d: %v", pid, err)
	}
}

// sendChaosTurn opens a conversation and sends one turn that needs no tool, so a
// re-drive is allowed (the killed attempt began no side-effecting call).
func sendChaosTurn(op pb.OperatorConsoleClient, ctx context.Context, id string, deadline time.Duration) (*pb.SendTurnOpResponse, error) {
	cctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	if _, err := op.OpenConversation(cctx, &pb.OpenConversationOpRequest{CommandId: id + "-open", Reason: "chaos-real agent sigkill",
		ConversationId: id, Title: "chaos agent sigkill"}); err != nil {
		return nil, fmt.Errorf("open conversation: %w", err)
	}
	return op.SendTurn(cctx, &pb.SendTurnOpRequest{CommandId: id + "-turn", Reason: "chaos-real agent sigkill",
		ConversationId: id, Text: "Without using any tool, reply with the single word OK."})
}

// TestChaosReal_AgentSigkill (ENG-1634; owner decisions 2026-11-19 and 2026-11-20):
// the chat pool worker is SIGKILLed from OUTSIDE the kernel (its pid read from
// OperatorConsole/ListAgentInstances) while a person's turn runs.
//
//   - One kill: the worker is respawned (a new pid on its stream) and the turn — which
//     began no side-effecting call — is re-driven once and answers.
//   - Two kills (the re-drive's worker too): the turn fails loudly
//     (assistant_restarted_twice: Unavailable, "restarted twice").
//
// Every chat pool worker is killed in a round, so the one serving the turn is hit
// whatever the pool size; each round lands CAMBRIAN_CHAOS_KILL_AFTER (default 300ms)
// after the turn was sent (or, in the second round, after the replacements are up).
// A scenario waits for a live worker first (CAMBRIAN_CHAOS_RESPAWN_WITHIN, default
// 90s): a boot takes tens of seconds (ENG-2266). A turn that answered before a kill
// is reported as a FAILED scenario (nothing was killed under it), never a pass.
// Run it against a FRESH kernel: it kills each chat worker three times, and a second
// back-to-back run trips the daemon restart policy's crash-loop quarantine, after
// which no worker comes back (that is the policy working, not this scenario failing).
func TestChaosReal_AgentSigkill(t *testing.T) {
	chaosKernel(t)
	op, ctx := chaosOperator(t)
	deadline := envDuration(t, "CAMBRIAN_CHAOS_EXECUTE_DEADLINE", 2*time.Minute)
	killAfter := envDuration(t, "CAMBRIAN_CHAOS_KILL_AFTER", 300*time.Millisecond)

	respawn := envDuration(t, "CAMBRIAN_CHAOS_RESPAWN_WITHIN", 90*time.Second)
	run := func(t *testing.T, kills int) (*pb.SendTurnOpResponse, error, []int) {
		// A live worker before anything is sent: the previous scenario's kill may still
		// be respawning (a boot takes tens of seconds).
		victims := awaitWorkers(t, op, ctx, nil, 1, respawn)
		type result struct {
			resp *pb.SendTurnOpResponse
			err  error
		}
		done := make(chan result, 1)
		id := fmt.Sprintf("chaos-sigkill-%d-%d", kills, time.Now().UnixNano())
		go func() {
			resp, err := sendChaosTurn(op, ctx, id, deadline)
			done <- result{resp, err}
		}()
		var killed []int
		for round := 1; round <= kills; round++ {
			if round > 1 {
				// The re-drive waits for a replacement and runs on it: kill the respawned
				// workers once they are up and the re-sent turn has reached one.
				victims = awaitWorkers(t, op, ctx, killed, len(victims), respawn)
			}
			select {
			case r := <-done:
				t.Fatalf("chaos-real: the turn ended before kill %d (%v, %v); lower CAMBRIAN_CHAOS_KILL_AFTER", round, r.resp, r.err)
			case <-time.After(killAfter):
			}
			for _, pid := range victims {
				killPid(t, pid)
				killed = append(killed, pid)
			}
		}
		r := <-done
		return r.resp, r.err, killed
	}

	t.Run("one kill: respawned and re-driven", func(t *testing.T) {
		resp, err, killed := run(t, 1)
		if err != nil || resp.GetReply() == nil {
			t.Fatalf("the turn did not survive one worker kill: %v", err)
		}
		if fresh := awaitWorkers(t, op, ctx, killed, 1, respawn); len(fresh) == 0 {
			t.Fatalf("the killed workers %v were not respawned", killed)
		}
	})
	t.Run("two kills: the turn fails loudly", func(t *testing.T) {
		_, err, killed := run(t, 2)
		if len(killed) < 2 {
			t.Fatalf("killed %v, want two rounds of workers", killed)
		}
		if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "restarted twice") {
			t.Fatalf("after two kills: %v — want Unavailable saying the assistant restarted twice", err)
		}
	})
}
