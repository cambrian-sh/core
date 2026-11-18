package app

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/pypackages"
)

// packageProbePy is a probe agent: it serves its --socket, imports --module
// (reporting the directory it came from), optionally tries to list --peek (a
// path it must not be able to read), and reports over the kernel connection.
const packageProbePy = `
import os, sys, time
from concurrent import futures
import grpc
from cambrian_agent_sdk.channel import build_channel

def arg(name):
    a = sys.argv
    return a[a.index(name) + 1] if name in a else ""

sock, addr, pid, mod, peek = arg("--socket"), arg("--substrate-addr"), arg("--probe-id"), arg("--module"), arg("--peek")
server = grpc.server(futures.ThreadPoolExecutor(max_workers=1))
server.add_insecure_port("unix:" + sock)
server.start()
try:
    m = __import__(mod)
    imp = "ok@" + os.path.dirname(os.path.dirname(os.path.abspath(m.__file__)))
except Exception as e:
    imp = "failed(" + type(e).__name__ + ")"
extra = ""
if peek:
    try:
        os.listdir(peek)
        extra = ";peek=read"
    except OSError as e:
        extra = ";peek=denied(" + type(e).__name__ + ")"
report = "probe=%s;import=%s%s" % (pid, imp, extra)
raw = report.encode()
def varint(n):
    out = b""
    while True:
        b, n = n & 0x7F, n >> 7
        out += bytes([b | (0x80 if n else 0)])
        if not n:
            return out
req = b"\x0a" + varint(len(raw)) + raw
check = build_channel(addr).unary_unary("/grpc.health.v1.Health/Check",
    request_serializer=lambda b: b, response_deserializer=lambda b: b)
for _ in range(50):
    try:
        check(req, timeout=3)
        break
    except grpc.RpcError as e:
        time.sleep(0.2)
time.sleep(600)
`

// ADR-0157 D7 P3 end to end, through the real path: the app's own wiring
// (startPythonEnv, buildApprovalHub over the 0033 tables, wirePythonPackages
// over the 0057 tables), the real pinned uv resolving and installing from the
// configured index (or wheelhouse), and a real agent process launched by
// GetOrBootInstance in the sandbox (Windows: the AppContainer, refusing on any
// missing control; Linux: bubblewrap).
//
// Steps: (1) an agent declaring a package is held, its boot refused with the
// approval named, and the card lists the dependencies with hashes; (2) a
// kernel restart while the card is pending keeps it (same card, no second);
// (3) an Operator approves it on the restarted kernel, the layer is installed,
// and the agent boots and imports the package FROM ITS LAYER; (4) another
// agent can neither import it nor read the layer; (5) a changed version is
// refused with a clear reason (a new card); (6) the install is audited.
//
// Environment: PYPKG_E2E_KERNEL is a kernel dir whose runtime/python holds a
// managed base env built with `python-env build --dir <it>` (SDK-only is
// enough); PG_TEST_DSN is a migrated, disposable database (through 0057).
// Optional: PYPKG_E2E_PACKAGE (name==version), PYPKG_E2E_MODULE and
// PYPKG_E2E_OTHER_VERSION (default python-dateutil==2.9.0.post0, dateutil,
// 2.8.2); PYPKG_E2E_WHEELHOUSE resolves and installs offline from a directory.
func TestE2E_PythonPackageLayerThroughOperatorApproval(t *testing.T) {
	kernelDir, dsn := os.Getenv("PYPKG_E2E_KERNEL"), os.Getenv("PG_TEST_DSN")
	if kernelDir == "" || dsn == "" {
		t.Skip("set PYPKG_E2E_KERNEL and PG_TEST_DSN to run the package layer e2e")
	}
	pkgName, pkgVersion, module, otherVersion := "python-dateutil", "2.9.0.post0", "dateutil", "2.8.2"
	if v := os.Getenv("PYPKG_E2E_PACKAGE"); v != "" {
		pkgName, pkgVersion, _ = strings.Cut(v, "==")
	}
	if v := os.Getenv("PYPKG_E2E_MODULE"); v != "" {
		module = v
	}
	if v := os.Getenv("PYPKG_E2E_OTHER_VERSION"); v != "" {
		otherVersion = v
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	const prefix = "pypkge2e-"
	cleanRows := func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, _ = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
		_, _ = tx.Exec(ctx, `DELETE FROM python_package_events WHERE target LIKE 'pypkge2e-%'`)
		_, _ = tx.Exec(ctx, `DELETE FROM python_package_approvals WHERE target LIKE 'pypkge2e-%'`)
		_, _ = tx.Exec(ctx, `DELETE FROM approvals WHERE agent_id LIKE 'pypkge2e-%'`)
		_ = tx.Commit(ctx)
	}
	cleanRows()
	t.Cleanup(cleanRows)

	cfg := config.DefaultConfig()
	cfg.Execution.Agents.Python.BuildOnBoot = false
	if wh := os.Getenv("PYPKG_E2E_WHEELHOUSE"); wh != "" {
		cfg.Execution.Agents.Python.Wheelhouse = []string{wh}
		cfg.Execution.Agents.Python.Offline = true
	}
	envRoot := cfg.Execution.Agents.Python.ResolvedRoot(absOr(kernelDir))
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(envRoot, "layers")) })
	_ = os.RemoveAll(filepath.Join(envRoot, "layers"))

	// The "kernel": one gRPC server on TCP and on the kernel sockets.
	rec := &recordingHealth{}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, rec)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sockets := newAgentSocketServer()
	sockets.start(srv)
	go func() { _ = srv.Serve(tcp) }()
	defer func() { srv.Stop(); sockets.stop() }()

	work, err := os.MkdirTemp("", "p3e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	code := filepath.Join(work, "code")
	hidden := filepath.Join(work, "kernel")
	for _, d := range []string{code, filepath.Join(hidden, "configs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(code, "probe.py"), []byte(packageProbePy), 0o644); err != nil {
		t.Fatal(err)
	}

	holder, other, changed := prefix+"analyst", prefix+"reporter", prefix+"changed"
	manifests := map[string]*domain.AgentManifest{
		holder:  {PythonPackages: []domain.PythonPackage{{Name: pkgName, Version: pkgVersion}}},
		changed: {PythonPackages: []domain.PythonPackage{{Name: pkgName, Version: otherVersion}}},
	}
	manifestErr := func(_ context.Context, id string) (*domain.AgentManifest, error) { return manifests[id], nil }

	// boot builds one kernel's worth of wiring: the env gate, THE hub, the
	// package service, an instance manager in the sandbox.
	type kernelUnderTest struct {
		env pythonEnvGate
		hub *domain.AddressedHub
		svc *pypackages.Service
		im  *agentmgr.InstanceManager
	}
	bootKernel := func() kernelUnderTest {
		env := startPythonEnv(ctx, cfg, kernelDir)
		if _, err := env.Interpreter(); err != nil {
			t.Fatalf("the managed base env under %s is not usable: %v", kernelDir, err)
		}
		hub, err := buildApprovalHub(ctx, cfg, pool)
		if err != nil {
			t.Fatal(err)
		}
		svc := wirePythonPackages(ctx, cfg.Execution.Agents.Python, pool, env, hub, manifestErr)
		if svc == nil {
			t.Fatal("python package service not wired (is PG_TEST_DSN migrated through 0057?)")
		}
		sb := agentmgr.NewAgentSandbox(agentmgr.AgentSandboxOptions{
			WorkRoot:  filepath.Join(work, "w"),
			KernelDir: hidden,
			// As anchorAgentSandboxPaths: a root outside the kernel dir is kernel state.
			KernelStatePaths: []string{filepath.Join(hidden, "configs"), envRoot},
			ThirdPartyRefuse: true,
			FirstPartyRefuse: true,
		})
		im := agentmgr.NewInstanceManager(tcp.Addr().String())
		im.SetPythonEnv(env)
		im.SetPythonLayers(svc)
		im.SetManifestResolver(func(_ context.Context, id string) *domain.AgentManifest { return manifests[id] })
		im.SetAgentContainment(agentmgr.AgentContainment{Sandbox: sb})
		im.SetAgentTransport(agentmgr.AgentTransport{KernelSocket: sockets.Open})
		im.SetAgentBootTimeout(90 * time.Second)
		return kernelUnderTest{env: env, hub: hub, svc: svc, im: im}
	}
	def := func(id string, args ...string) *domain.AgentDefinition {
		return &domain.AgentDefinition{ID: id, Runtime: domain.RuntimePython, ExecPath: "probe.py", Dir: code, TrustTier: "first-party",
			LaunchArgs: append([]string{"--probe-id", id, "--module", module}, args...)}
	}
	pendingFor := func(k kernelUnderTest, agent string) []domain.ApprovalItem {
		items, err := k.hub.List(ctx, domain.ApprovalQuery{States: []domain.ApprovalState{domain.ApprovalPending}})
		if err != nil {
			t.Fatal(err)
		}
		var out []domain.ApprovalItem
		for _, it := range items {
			if it.AgentID == agent && it.Kind == domain.ApprovalKindPythonPackage {
				out = append(out, it)
			}
		}
		return out
	}

	// 1. Held, with the card raised and listing the dependencies with hashes.
	k1 := bootKernel()
	bctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_, err = k1.im.GetOrBootInstance(bctx, def(holder), "")
	if !errors.Is(err, pypackages.ErrNotApproved) {
		t.Fatalf("an agent declaring an unapproved package must be held, got %v", err)
	}
	cards := pendingFor(k1, holder)
	if len(cards) != 1 {
		t.Fatalf("one card for the declared package, got %d", len(cards))
	}
	card := cards[0]
	if !strings.Contains(err.Error(), card.ID) || !strings.Contains(err.Error(), pkgName+"=="+pkgVersion) {
		t.Errorf("the refusal must name the package and its approval: %v", err)
	}
	t.Logf("held: %v", err)
	t.Logf("card %s rule=%s\n%s", card.ID, card.RuleSummary(), card.Object)
	if card.RuleSummary() != "operator" || card.AllowStanding || strings.Count(card.Object, "sha256:") < 2 {
		t.Errorf("card: rule=%q standing=%v object=%s", card.RuleSummary(), card.AllowStanding, card.Object)
	}

	// 2. Restart while pending: same card, no second.
	k1.svc.Stop()
	k2 := bootKernel()
	_, err = k2.im.GetOrBootInstance(bctx, def(holder), "")
	if !errors.Is(err, pypackages.ErrNotApproved) || !strings.Contains(err.Error(), card.ID) {
		t.Fatalf("after a restart the agent still waits on card %s: %v", card.ID, err)
	}
	if got := pendingFor(k2, holder); len(got) != 1 || got[0].ID != card.ID {
		t.Fatalf("the pending card must survive the restart unchanged: %+v", got)
	}

	// A non-operator cannot answer it.
	member := domain.Approver{Principal: domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "member-1"}, Roles: []string{"member"}}
	if _, err := k2.hub.Answer(ctx, card.ID, domain.ApprovalAnswer{Approved: true, By: member}); !errors.Is(err, domain.ErrApprovalNotAddressee) {
		t.Fatalf("a non-operator answer must be refused: %v", err)
	}

	// 3. An Operator approves on the restarted kernel: installed, the agent boots
	// and imports the package from its own layer.
	start := time.Now()
	if _, err := k2.hub.Answer(ctx, card.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op-e2e")}); err != nil {
		t.Fatal(err)
	}
	k2.svc.Stop() // waits for the background install the approval started
	t.Logf("approval → layer installed: %s", time.Since(start).Round(time.Millisecond))
	start = time.Now()
	inst, err := k2.im.GetOrBootInstance(bctx, def(holder), "")
	if err != nil {
		t.Fatalf("approved ⇒ the agent boots (refusing on any missing sandbox control): %v", err)
	}
	t.Logf("spawn with layer: %s", time.Since(start).Round(time.Millisecond))
	defer k2.im.EvictInstance(inst.ID)
	got := rec.waitFor(bctx, "probe="+holder+";")
	t.Logf("report: %s", got)
	layers := filepath.Join(envRoot, "layers")
	imp, _, _ := strings.Cut(strings.TrimPrefix(got, "probe="+holder+";import="), ";")
	if !strings.HasPrefix(imp, "ok@") || !strings.HasPrefix(filepath.Clean(strings.TrimPrefix(imp, "ok@")), filepath.Clean(layers)) {
		t.Fatalf("the agent must import %s from its own layer under %s: %s", module, layers, got)
	}
	layerDir := filepath.Dir(filepath.Clean(strings.TrimPrefix(imp, "ok@"))) // <layer>/site-packages → <layer>

	// 4. Another agent: no import, no read of the layer.
	inst2, err := k2.im.GetOrBootInstance(bctx, def(other, "--peek", layerDir), "")
	if err != nil {
		t.Fatalf("an agent that declares nothing boots: %v", err)
	}
	defer k2.im.EvictInstance(inst2.ID)
	got2 := rec.waitFor(bctx, "probe="+other+";")
	t.Logf("other agent: %s", got2)
	if !strings.Contains(got2, "import=failed(") || !strings.Contains(got2, "peek=denied(") {
		t.Fatalf("another agent must neither import %s nor read %s: %s", module, layerDir, got2)
	}

	// 5. A changed version is refused with a clear reason and its own card.
	_, err = k2.im.GetOrBootInstance(bctx, def(changed), "")
	if !errors.Is(err, pypackages.ErrNotApproved) || !strings.Contains(err.Error(), pkgName+"=="+otherVersion) {
		t.Fatalf("a changed version must be held naming it: %v", err)
	}
	t.Logf("changed version held: %v", err)

	// 6. Audited.
	store := postgres.NewPgPythonPackageStore(pool)
	recs, err := store.ForTarget(ctx, holder)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records: %v %v", recs, err)
	}
	evs, err := store.Events(ctx, recs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var acts []string
	for _, e := range evs {
		acts = append(acts, e.Action+"/"+e.Actor)
	}
	t.Logf("audit: %v", acts)
	if len(acts) < 3 || !strings.HasPrefix(acts[0], "requested/") || !strings.HasPrefix(acts[1], "approved/op-e2e") || !strings.HasPrefix(acts[2], "installed/") {
		t.Fatalf("audit trail: %v", acts)
	}
}
