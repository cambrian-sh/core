package mcp

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Integrations surface: live lifecycle, health, per-server secrets and literal
// env values, exercised against a REAL stdio server (this test binary in
// --mcp-env-server mode).

// fakeSecrets is a SecretResolver with the store's env-first precedence.
type fakeSecrets map[string]string

func (f fakeSecrets) Resolve(name, envVar string) string {
	if envVar != "" {
		if v := os.Getenv(envVar); v != "" {
			return v
		}
	}
	return f[name]
}

// recSink records the tool sets published per server.
type recSink struct {
	mu    sync.Mutex
	tools map[string][]domain.SystemTool
}

func newRecSink() *recSink { return &recSink{tools: map[string][]domain.SystemTool{}} }

func (r *recSink) SetServerTools(_ context.Context, id string, t []domain.SystemTool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[id] = t
}

func (r *recSink) RemoveServerTools(_ context.Context, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, id)
}

func (r *recSink) count(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tools[id])
}

// healthLog records every observed health change.
type healthLog struct {
	mu  sync.Mutex
	evs []ServerHealth
}

func (h *healthLog) add(s ServerHealth) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.evs = append(h.evs, s)
}

func (h *healthLog) states(id string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, e := range h.evs {
		if e.ServerID == id {
			out = append(out, e.State)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func envServer(t *testing.T, id string) ServerConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{ID: id, Transport: "stdio", Endpoint: exe, Args: []string{envServerFlag},
		DefaultPolicy: &ToolPolicy{Effects: []string{"read"}}, Sandbox: testSandboxSpec(t, id)}
}

// callEnv returns the env server's reported environment and args.
func callEnv(t *testing.T, conn *Connector, id string) string {
	t.Helper()
	h := &Handler{Sessions: conn}
	out, err := h.Execute(context.Background(), domain.ToolCall{ToolName: "mcp:" + id + "/env", ArgsJSON: []byte(`{}`)})
	if err != nil {
		t.Fatalf("env call on %s: %v", id, err)
	}
	return string(out)
}

// Enable (AddServer) starts the process, health goes starting → ready with the
// tool count, tools reach the sink; disable (StopServer) kills it, withdraws
// the tools and reports stopped; remove forgets the record. Every change is
// observed (it becomes the operator feed event).
func TestLifecycle_StartStopWithHealthAndToolSync(t *testing.T) {
	conn := NewConnector(CredentialSources{})
	defer conn.Close()
	hl := &healthLog{}
	conn.SetHealthObserver(hl.add)
	sink := newRecSink()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn.AddServer(ctx, envServer(t, "alpha"), sink, time.Hour)
	waitFor(t, "alpha ready", func() bool { h, _ := conn.Health("alpha"); return h.State == HealthReady })
	if sink.count("alpha") != 1 {
		t.Fatalf("tools not published live: %d", sink.count("alpha"))
	}
	h, _ := conn.Health("alpha")
	if h.ToolCount != 1 || h.LastHandshake.IsZero() || h.LastError != "" {
		t.Fatalf("health after start: %+v", h)
	}
	if _, ok := conn.Session("alpha"); !ok {
		t.Fatal("no live session")
	}

	// Reconfigure = restart: the restart counter moves.
	conn.AddServer(ctx, envServer(t, "alpha"), sink, time.Hour)
	waitFor(t, "alpha ready again", func() bool { h, _ := conn.Health("alpha"); return h.State == HealthReady && h.Restarts == 1 })

	conn.StopServer(ctx, "alpha", sink)
	if _, ok := conn.Session("alpha"); ok {
		t.Fatal("session survived StopServer")
	}
	if sink.count("alpha") != 0 {
		t.Fatal("tools survived StopServer")
	}
	if h, _ := conn.Health("alpha"); h.State != HealthStopped || h.ToolCount != 0 {
		t.Fatalf("health after stop: %+v", h)
	}

	conn.RemoveServer(ctx, "alpha", sink)
	if _, ok := conn.Health("alpha"); ok {
		t.Fatal("health record survived RemoveServer")
	}
	st := strings.Join(hl.states("alpha"), ",")
	for _, want := range []string{HealthStarting, HealthReady, HealthStopped} {
		if !strings.Contains(st, want) {
			t.Fatalf("observer missed %s: %s", want, st)
		}
	}
}

// A server that cannot start reports error with the cause; the cause never
// carries a secret injected into it.
func TestHealth_ErrorIsReportedAndRedacted(t *testing.T) {
	conn := NewConnector(CredentialSources{Secrets: fakeSecrets{EnvSecretName("broken", "BROKEN_API_KEY"): "sk-very-secret-value"}})
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The secret is also the (nonexistent) command, so the exec error quotes it.
	s := ServerConfig{ID: "broken", Transport: "stdio", Endpoint: "sk-very-secret-value", SecretEnv: []string{"BROKEN_API_KEY"},
		Sandbox: testSandboxSpec(t, "broken")}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	conn.ConnectAll(ctx, []ServerConfig{s})
	h, _ := conn.Health("broken")
	if h.State != HealthError || h.LastError == "" {
		t.Fatalf("want error health, got %+v", h)
	}
	if strings.Contains(h.LastError, "sk-very-secret-value") {
		t.Fatalf("SECRET LEAK in health: %q", h.LastError)
	}
	if strings.Contains(logs.String(), "sk-very-secret-value") {
		t.Fatalf("SECRET LEAK in logs: %s", logs.String())
	}
}

// Per-server credential sets: a stored secret reaches ONLY the server it was
// saved for; literal settings reach the process and fill "${NAME}" args.
func TestSecretsAndSettings_InjectedOnlyIntoTheirServer(t *testing.T) {
	conn := NewConnector(CredentialSources{Secrets: fakeSecrets{
		EnvSecretName("one", "SHARED_API_KEY"): "secret-for-one",
		EnvSecretName("two", "SHARED_API_KEY"): "secret-for-two",
	}})
	defer conn.Close()
	ctx := context.Background()

	one := envServer(t, "one")
	one.SecretEnv = []string{"SHARED_API_KEY"}
	one.EnvValues = map[string]string{"SANDBOX_NETWORK_MODE": "none", "ROOT_DIR": "/data/one"}
	one.Args = append(one.Args, "${ROOT_DIR}", "${UNSET_ONE}")
	two := envServer(t, "two")
	two.SecretEnv = []string{"SHARED_API_KEY"}
	three := envServer(t, "three") // declares no secret at all

	conn.ConnectAll(ctx, []ServerConfig{one, two, three})
	o, w, th := callEnv(t, conn, "one"), callEnv(t, conn, "two"), callEnv(t, conn, "three")
	if !strings.Contains(o, "SHARED_API_KEY=secret-for-one") || strings.Contains(o, "secret-for-two") {
		t.Errorf("server one env wrong: %s", o)
	}
	if !strings.Contains(w, "SHARED_API_KEY=secret-for-two") || strings.Contains(w, "secret-for-one") {
		t.Errorf("server two env wrong: %s", w)
	}
	if strings.Contains(th, "secret-for") {
		t.Errorf("server three received another server's secret: %s", th)
	}
	if !strings.Contains(o, "SANDBOX_NETWORK_MODE=none") {
		t.Errorf("literal env value missing: %s", o)
	}
	if !strings.Contains(o, "/data/one") || strings.Contains(o, "${UNSET_ONE}") || strings.Contains(o, "${ROOT_DIR}") {
		t.Errorf("args placeholders not expanded/dropped: %s", o)
	}
}

// D5: a kernel env var the server passes through outranks the stored secret.
func TestSecrets_PassthroughEnvWinsOverStore(t *testing.T) {
	t.Setenv("PRECEDENCE_API_KEY", "from-env")
	conn := NewConnector(CredentialSources{Secrets: fakeSecrets{EnvSecretName("p", "PRECEDENCE_API_KEY"): "from-store"}})
	defer conn.Close()
	s := envServer(t, "p")
	s.SecretEnv = []string{"PRECEDENCE_API_KEY"}
	s.Env = []string{"PRECEDENCE_API_KEY"}
	conn.ConnectAll(context.Background(), []ServerConfig{s})
	if out := callEnv(t, conn, "p"); !strings.Contains(out, "PRECEDENCE_API_KEY=from-env") {
		t.Fatalf("env var must win: %s", out)
	}
}

// TestServer: ephemeral when not running (lists tools with their policy),
// the live session when ready; the probe is refused for a dangerous tool.
func TestTestServer_EphemeralLiveAndProbeGuard(t *testing.T) {
	conn := NewConnector(CredentialSources{})
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := envServer(t, "t1")
	rep := conn.TestServer(ctx, s, "env", nil)
	if !rep.OK || rep.UsedLive || len(rep.Tools) != 1 || rep.Tools[0].PolicySource != "default" {
		t.Fatalf("ephemeral test: %+v", rep)
	}
	if !rep.ProbeOK || !strings.Contains(rep.ProbeOutput, "PATH") {
		t.Fatalf("read-only probe should run: %+v", rep)
	}
	if _, ok := conn.Session("t1"); ok {
		t.Fatal("an ephemeral test must not leave a session behind")
	}

	conn.AddServer(ctx, s, newRecSink(), time.Hour)
	waitFor(t, "t1 ready", func() bool { h, _ := conn.Health("t1"); return h.State == HealthReady })
	if rep := conn.TestServer(ctx, s, "", nil); !rep.OK || !rep.UsedLive {
		t.Fatalf("live test: %+v", rep)
	}

	closed := envServer(t, "t2")
	closed.DefaultPolicy = nil // fail closed: the env tool is dangerous
	rep = conn.TestServer(ctx, closed, "env", nil)
	if rep.ProbeTool != "" || rep.ProbeSkipped == "" || rep.Tools[0].PolicySource != "fail_closed" || !rep.Tools[0].Dangerous {
		t.Fatalf("a dangerous probe must be skipped: %+v", rep)
	}
	conn.RemoveServer(ctx, "t1", nil)
}
