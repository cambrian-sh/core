package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/pyenv"
	"github.com/cambrian-sh/core/internal/pypackages"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// localPkgEnv stands in for the pinned uv only: it "resolves" the specs it
// knows into a fixed lock and "installs" a layer as a directory. Everything
// around it is the kernel's: the 0057 tables, THE approval hub over the 0033
// tables, the package service, the kernel tool wired by the app, and the tool
// executor's kernel-tool dispatch.
type localPkgEnv struct {
	root  string
	locks map[string]domain.PythonPackageLock
}

func (e *localPkgEnv) ResolvePackage(_ context.Context, _ pyenv.Interpreter, p domain.PythonPackage) (domain.PythonPackageLock, error) {
	if l, ok := e.locks[p.Spec()]; ok {
		return l, nil
	}
	return domain.PythonPackageLock{}, fmt.Errorf("%s: %w: not on the index", p.Spec(), pyenv.ErrUnresolvable)
}

func (e *localPkgEnv) dir(agent string, entries []domain.PythonLockEntry) string {
	var parts []string
	for _, x := range entries {
		parts = append(parts, x.Name+"-"+x.Version)
	}
	return filepath.Join(e.root, agent, strings.Join(parts, "_"))
}

func (e *localPkgEnv) FindLayer(agent string, base pyenv.Interpreter, entries []domain.PythonLockEntry) (pyenv.Layer, bool) {
	d := e.dir(agent, entries)
	if _, err := os.Stat(d); err != nil {
		return pyenv.Layer{}, false
	}
	return pyenv.Layer{Dir: d, Boot: filepath.Join(d, "boot"), ID: strings.Repeat("c", 64), Base: base.Home}, true
}

func (e *localPkgEnv) BuildLayer(_ context.Context, r pyenv.LayerRequest) (pyenv.Layer, error) {
	d := e.dir(r.Agent, r.Entries)
	if err := os.MkdirAll(filepath.Join(d, "boot"), 0o755); err != nil {
		return pyenv.Layer{}, err
	}
	return pyenv.Layer{Dir: d, Boot: filepath.Join(d, "boot"), ID: strings.Repeat("d", 64), Base: r.Base.Home}, nil
}

type leaseTable map[domain.LeaseID]string

func (l leaseTable) ResolveLease(id domain.LeaseID) (domain.LeaseBinding, bool) {
	a, ok := l[id]
	return domain.LeaseBinding{AgentID: a}, ok
}

// ADR-0157 D7.9 (P4) end to end through the kernel's own path: an allowlisted
// Python agent calls request_python_package through the tool executor; the
// card is raised durably for the Operator role; an Operator approving inside
// the window installs the package into the agent's own layer (installed) and
// the agent's live instances are told to recycle; a request left unanswered
// returns pending, survives a kernel restart as the same card, and is
// installed for the agent's later runs once approved; the trail is audited.
func TestPg_RequestPythonPackageThroughTheKernelTool(t *testing.T) {
	dsn, _ := pgscratch.Migrated(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	clean := func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, _ = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
		_, _ = tx.Exec(ctx, `DELETE FROM python_package_events WHERE target LIKE 'pyreq-%'`)
		_, _ = tx.Exec(ctx, `DELETE FROM python_package_approvals WHERE target LIKE 'pyreq-%'`)
		_, _ = tx.Exec(ctx, `DELETE FROM approvals WHERE agent_id LIKE 'pyreq-%'`)
		_ = tx.Commit(ctx)
	}
	clean()
	t.Cleanup(clean)

	cfg := config.DefaultConfig()
	cfg.Execution.Agents.Python.RequestWaitSeconds = 3
	const analyst, plain = "pyreq-analyst", "pyreq-plain"
	reg := metabolism.NewInMemoryRegistry()
	reg.SetAgent(domain.AgentDefinition{ID: analyst, Name: analyst, Runtime: domain.RuntimePython})
	reg.SetManifest(analyst, &domain.AgentManifest{ToolAllowlist: []string{"web_search", pypackages.ToolName}})
	reg.SetAgent(domain.AgentDefinition{ID: plain, Name: plain, Runtime: domain.RuntimePython})
	reg.SetManifest(plain, &domain.AgentManifest{ToolAllowlist: []string{"*"}})
	leases := leaseTable{"lease-analyst": analyst, "lease-plain": plain}

	env := &localPkgEnv{root: t.TempDir(), locks: map[string]domain.PythonPackageLock{
		"tinypkg==1.0.0":  {Entries: []domain.PythonLockEntry{{Name: "tinypkg", Version: "1.0.0", Hashes: []string{strings.Repeat("a", 64)}}}},
		"otherpkg==2.0.0": {Entries: []domain.PythonLockEntry{{Name: "otherpkg", Version: "2.0.0", Hashes: []string{strings.Repeat("b", 64)}}}},
	}}
	base := pyenv.Interpreter{Path: "/env/base/gen7/bin/python3", Home: "/env/base/gen7"}
	var mu sync.Mutex
	var changed []string
	store := postgres.NewPgPythonPackageStore(pool)
	if !store.Ready(ctx) {
		t.Fatal("0057 tables missing")
	}
	type kernel struct {
		hub *domain.AddressedHub
		svc *pypackages.Service
		te  *domain.ToolExecutor
	}
	boot := func() kernel {
		hub, err := buildApprovalHub(ctx, cfg, pool)
		if err != nil {
			t.Fatal(err)
		}
		svc, err := pypackages.New(store, hub, env, pypackages.Config{
			ApprovalTTL: cfg.Execution.Agents.Python.ApprovalTTL(),
			Base:        func() (pyenv.Interpreter, error) { return base, nil },
			Declared:    func(context.Context, string) []domain.PythonPackage { return nil },
			OnLayerChange: func(a string) {
				mu.Lock()
				changed = append(changed, a)
				mu.Unlock()
			},
			RequestPoll: 50 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		te := newTestToolExecutor(t, domain.ToolExecutorControls{}, domain.ToolExecutor{})
		wirePythonPackageTool(cfg.Execution.Agents.Python, svc, te, leases, reg)
		return kernel{hub: hub, svc: svc, te: te}
	}
	pendingCards := func(k kernel) []domain.ApprovalItem {
		items, err := k.hub.List(ctx, domain.ApprovalQuery{States: []domain.ApprovalState{domain.ApprovalPending}})
		if err != nil {
			t.Fatal(err)
		}
		var out []domain.ApprovalItem
		for _, it := range items {
			if it.AgentID == analyst && it.Kind == domain.ApprovalKindPythonPackage {
				out = append(out, it)
			}
		}
		return out
	}
	call := func(k kernel, agent string, lease domain.LeaseID, args string) (pypackages.RequestResult, domain.ToolCallResponse) {
		out := k.te.Execute(ctx, domain.ToolCallRequest{AgentID: agent, ToolName: pypackages.ToolName,
			ArgsJSON: []byte(args), SessionTokenID: lease})
		var r pypackages.RequestResult
		if out.Error == "" && !out.Denied {
			if err := json.Unmarshal(out.ResultJSON, &r); err != nil {
				t.Fatalf("result %s: %v", out.ResultJSON, err)
			}
		}
		return r, out
	}

	k := boot()
	// Offered only to the allowlisted agent's lease.
	offered := func(lease domain.LeaseID) bool {
		for _, s := range k.te.KernelToolSpecsFor(ctx, lease) {
			if s.Name == pypackages.ToolName {
				return true
			}
		}
		return false
	}
	if !offered("lease-analyst") || offered("lease-plain") || offered("lease-unknown") {
		t.Fatal("request_python_package must be offered only to a Python agent whose allowlist names it")
	}
	if _, out := call(k, plain, "lease-plain", `{"name":"tinypkg","version":"1.0.0","rationale":"x"}`); !strings.Contains(out.Error, "allowlist") {
		t.Fatalf("a glob allowlist does not grant the admin-effect tool: %+v", out)
	}

	// The agent was spawned with no layer (its spawn's Ensure had nothing).
	if _, err := k.svc.Ensure(ctx, analyst, nil, base); err != nil {
		t.Fatal(err)
	}
	// 1. Approved inside the window ⇒ installed; the live instances recycle.
	go func() {
		for i := 0; i < 100; i++ {
			if cards := pendingCards(k); len(cards) == 1 {
				_, _ = k.hub.Answer(ctx, cards[0].ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op-e2e")})
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()
	r, out := call(k, analyst, "lease-analyst", `{"name":"tinypkg","version":"1.0.0","rationale":"parse the vendor's tiny format"}`)
	if r.Status != pypackages.RequestInstalled {
		t.Fatalf("approved inside the window ⇒ installed: %+v %+v", r, out)
	}
	t.Logf("installed: %+v", r)
	k.svc.Stop()
	mu.Lock()
	gotChange := len(changed) > 0 && changed[0] == analyst
	mu.Unlock()
	if !gotChange {
		t.Fatal("the layer change must reach OnLayerChange (the agent manager's stale recycle)")
	}
	cur, err := k.svc.Current(analyst, base)
	if err != nil || !strings.Contains(cur.Dir, "tinypkg-1.0.0") {
		t.Fatalf("the agent's layer holds the package: %+v %v", cur, err)
	}

	// 2. Unanswered ⇒ pending; a kernel restart keeps the same card; a repeat
	// returns it; approval after the window installs it for later runs.
	k = boot()
	start := time.Now()
	r, out = call(k, analyst, "lease-analyst", `{"name":"otherpkg","version":"2.0.0","rationale":"second need"}`)
	if r.Status != pypackages.RequestPending || r.ApprovalID == "" {
		t.Fatalf("no decision ⇒ pending: %+v %+v", r, out)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("the tool must not wait beyond its window: %s", el)
	}
	k.svc.Stop()
	cfg.Execution.Agents.Python.RequestWaitSeconds = 1
	k2 := boot() // a kernel restart
	again, _ := call(k2, analyst, "lease-analyst", `{"name":"otherpkg","version":"2.0.0","rationale":"second need"}`)
	if again.Status != pypackages.RequestPending || again.ApprovalID != r.ApprovalID {
		t.Fatalf("after a restart a repeat returns the same request: %+v vs %+v", again, r)
	}
	if n := len(pendingCards(k2)); n != 1 {
		t.Fatalf("one pending card, got %d", n)
	}
	if _, err := k2.hub.Answer(ctx, r.ApprovalID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op-e2e")}); err != nil {
		t.Fatal(err)
	}
	k2.svc.Stop()                                    // the approval's background install
	l, err := k2.svc.Ensure(ctx, analyst, nil, base) // the agent's next spawn
	if err != nil || !strings.Contains(l.Dir, "otherpkg-2.0.0") || !strings.Contains(l.Dir, "tinypkg-1.0.0") {
		t.Fatalf("approved after the window ⇒ in the agent's layer at its next run: %+v %v", l, err)
	}

	// 3. Audited, as the agent's runtime request.
	recs, err := store.ForTarget(ctx, analyst)
	if err != nil || len(recs) != 2 {
		t.Fatalf("records: %+v %v", recs, err)
	}
	for _, rec := range recs {
		if rec.RequestedBy != "agent:"+analyst || rec.State != domain.PythonPackageApproved {
			t.Errorf("record %s: requested_by=%q state=%s", rec.Name, rec.RequestedBy, rec.State)
		}
		evs, err := store.Events(ctx, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		var acts []string
		for _, e := range evs {
			acts = append(acts, e.Action)
		}
		t.Logf("%s audit: %v", rec.Name, acts)
		if len(acts) < 3 || acts[0] != "requested" || acts[1] != "approved" || acts[2] != "installed" {
			t.Errorf("%s audit trail: %v", rec.Name, acts)
		}
	}
}
