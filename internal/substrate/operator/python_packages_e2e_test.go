package operator_test

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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/pyenv"
	"github.com/cambrian-sh/core/internal/pypackages"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// layoutEnv stands in for the pinned installer only: it "resolves" known specs
// into fixed locks and "installs" a layer as a generation directory at the
// path the real builder names (Builder.LayerPath), with the real layer marker,
// so the real inventory and GC see exactly what a real install leaves.
type layoutEnv struct {
	b     *pyenv.Builder
	locks map[string]domain.PythonPackageLock
}

func (e *layoutEnv) ResolvePackage(_ context.Context, _ pyenv.Interpreter, p domain.PythonPackage) (domain.PythonPackageLock, error) {
	if l, ok := e.locks[p.Spec()]; ok {
		return l, nil
	}
	return domain.PythonPackageLock{}, fmt.Errorf("%s: %w: not on the index", p.Spec(), pyenv.ErrUnresolvable)
}

func (e *layoutEnv) layer(dir string, base pyenv.Interpreter) pyenv.Layer {
	return pyenv.Layer{Dir: dir, Boot: filepath.Join(dir, "boot"), ID: filepath.Base(dir) + strings.Repeat("0", 48), Base: base.Home}
}

func (e *layoutEnv) FindLayer(agent string, base pyenv.Interpreter, entries []domain.PythonLockEntry) (pyenv.Layer, bool) {
	dir := e.b.LayerPath(agent, base, entries)
	if _, err := os.Stat(filepath.Join(dir, "generation.json")); err != nil {
		return pyenv.Layer{}, false
	}
	l := e.layer(dir, base)
	l.Reused = true
	return l, true
}

func (e *layoutEnv) BuildLayer(_ context.Context, r pyenv.LayerRequest) (pyenv.Layer, error) {
	dir := e.b.LayerPath(r.Agent, r.Base, r.Entries)
	if err := os.MkdirAll(filepath.Join(dir, "site-packages"), 0o755); err != nil {
		return pyenv.Layer{}, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "boot"), 0o755); err != nil {
		return pyenv.Layer{}, err
	}
	m := pyenv.LayerMarker{Kind: "layer", ID: filepath.Base(dir) + strings.Repeat("0", 48), Agent: r.Agent, Base: filepath.Base(r.Base.Home),
		Declared: r.Declared, Entries: r.Entries, Health: "ok", BuiltAt: time.Now().UTC()}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "generation.json"), raw, 0o644); err != nil {
		return pyenv.Layer{}, err
	}
	return e.layer(dir, r.Base), nil
}

func writeJSONFile(t *testing.T, p string, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func lockOf(name, version, hash string) domain.PythonPackageLock {
	return domain.PythonPackageLock{Entries: []domain.PythonLockEntry{{Name: name, Version: version, Hashes: []string{strings.Repeat(hash, 64)}}}}
}

// ENG-2012 (ADR-0157 D7.10/D7.12, P5) end to end through the operator plane:
// the 0057+0067 tables, the approval hub, the package service, the real env
// inventory and GC over a real env root, behind the contract 0119 RPCs.
//   - a package dropped from a running agent's profile: RetryPythonPackageInstall
//     builds a new generation without it (audited "removed"); the old one is kept
//     while an instance runs on it and collected by PrunePythonEnv once it is gone;
//   - a runtime package is removed by RemovePythonPackage (a new generation,
//     state removed, audited); asking for it again raises a fresh card;
//   - a declared package cannot be removed by command; an unknown one is NotFound;
//   - ListPythonPackages lists the audit trail, the generations and sizes.
func TestPg_E2E_PythonPackageLifecycleOnTheOperatorPlane(t *testing.T) {
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
		_, _ = tx.Exec(ctx, `DELETE FROM python_package_events WHERE target LIKE 'opp5-%'`)
		_, _ = tx.Exec(ctx, `DELETE FROM python_package_approvals WHERE target LIKE 'opp5-%'`)
		_ = tx.Commit(ctx)
	}
	clean()
	t.Cleanup(clean)
	store := postgres.NewPgPythonPackageStore(pool)
	if !store.Ready(ctx) {
		t.Fatal("0057 tables missing")
	}

	// A real env root: one current base generation.
	root := t.TempDir()
	pins, err := pyenv.Pins()
	if err != nil {
		t.Fatal(err)
	}
	b := &pyenv.Builder{Root: root, Pins: pins, Platform: "windows-x86_64"}
	const gen = "0123456789abcdef"
	baseHome := filepath.Join(root, "base", gen)
	writeJSONFile(t, filepath.Join(baseHome, "generation.json"), pyenv.Marker{Kind: "base", ID: gen + strings.Repeat("0", 48), Health: "ok", BuiltAt: time.Now().Add(-time.Hour)})
	writeJSONFile(t, filepath.Join(root, "current.json"), pyenv.Current{Base: gen})
	base := pyenv.Interpreter{Path: filepath.Join(baseHome, "python.exe"), Home: baseHome}

	const agent = "opp5-analyst"
	env := &layoutEnv{b: b, locks: map[string]domain.PythonPackageLock{
		"tinypkg==1.0.0":  lockOf("tinypkg", "1.0.0", "a"),
		"otherpkg==2.0.0": lockOf("otherpkg", "2.0.0", "b"),
		"rtpkg==3.0.0":    lockOf("rtpkg", "3.0.0", "c"),
	}}
	var mu sync.Mutex
	declared := []domain.PythonPackage{{Name: "tinypkg", Version: "1.0.0"}, {Name: "otherpkg", Version: "2.0.0"}}
	var inUse []string
	changes := 0
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{Store: domain.NewMemoryApprovalStore()})
	svc, err := pypackages.New(store, hub, env, pypackages.Config{
		Base: func() (pyenv.Interpreter, error) { return base, nil },
		Declared: func(context.Context, string) []domain.PythonPackage {
			mu.Lock()
			defer mu.Unlock()
			return append([]domain.PythonPackage(nil), declared...)
		},
		OnLayerChange: func(string) { mu.Lock(); changes++; mu.Unlock() },
		RequestPoll:   20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Stop)
	gc, err := pyenv.NewCollector(b, pyenv.GCPolicy{KeepPrevious: 1}, func(context.Context) pyenv.KeepSet {
		layers, release := svc.GCKeep()
		mu.Lock()
		defer mu.Unlock()
		return pyenv.KeepSet{InUse: append([]string(nil), inUse...), Layers: layers, KnownAgents: map[string]bool{agent: true}, Release: release}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pypackages.NewAdmin(svc, b, gc)
	if err != nil {
		t.Fatal(err)
	}
	op, _, audit, _ := newCommandService()
	op.SetPythonPackages(admin)
	opc := opCtx()
	approveAll := func() {
		items, _ := hub.List(ctx, domain.ApprovalQuery{States: []domain.ApprovalState{domain.ApprovalPending}})
		for _, it := range items {
			if _, err := hub.Answer(ctx, it.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op-e2e")}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The agent's spawn: held, approved, installed; an instance runs on L1.
	if _, err := svc.Ensure(ctx, agent, declared, base); err == nil {
		t.Fatal("unapproved declaration must hold the agent")
	}
	approveAll()
	svc.Stop() // background installs started by the approvals
	l1, err := svc.Ensure(ctx, agent, declared, base)
	if err != nil || l1.Dir == "" {
		t.Fatalf("L1: %+v %v", l1, err)
	}
	mu.Lock()
	inUse = []string{baseHome, l1.Dir}
	mu.Unlock()

	// 1. The profile drops otherpkg; the operator applies it now.
	mu.Lock()
	declared = declared[:1]
	mu.Unlock()
	ack, err := op.RetryPythonPackageInstall(opc, &pb.RetryPythonPackageInstallOpRequest{CommandId: "p5-retry", Reason: "profile edited", Target: agent})
	if err != nil || ack.GetGeneration() == "" || ack.GetGeneration() == filepath.Base(l1.Dir) {
		t.Fatalf("retry: %+v %v (L1 %s)", ack, err, l1.Dir)
	}
	l2 := filepath.Join(filepath.Dir(l1.Dir), ack.GetGeneration())
	mu.Lock()
	gotChange := changes > 0
	mu.Unlock()
	if !gotChange {
		t.Fatal("the switch must recycle the agent's instances (OnLayerChange)")
	}
	// The instance still runs on L1: prune keeps it.
	pr, err := op.PrunePythonEnv(opc, &pb.PrunePythonEnvOpRequest{CommandId: "p5-prune-1", Reason: "tidy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l1.Dir); err != nil {
		t.Fatalf("a generation a live instance runs on was collected: %+v", pr.GetReport())
	}
	// The instance recycled: L1 goes, L2 stays.
	mu.Lock()
	inUse = []string{baseHome, l2}
	mu.Unlock()
	pr, err = op.PrunePythonEnv(opc, &pb.PrunePythonEnvOpRequest{CommandId: "p5-prune-2", Reason: "tidy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l1.Dir); !os.IsNotExist(err) {
		t.Fatalf("L1 not collected after the recycle: %+v", pr.GetReport())
	}
	if _, err := os.Stat(l2); err != nil {
		t.Fatalf("the current layer was collected: %v", err)
	}
	if len(pr.GetReport().GetRemoved()) != 1 || pr.GetReport().GetRemoved()[0].GetKind() != "layer" || pr.GetReport().GetFreedBytes() <= 0 {
		t.Fatalf("prune report: %+v", pr.GetReport())
	}

	// 2. A runtime package, then removed by command.
	go func() {
		for i := 0; i < 200; i++ {
			items, _ := hub.List(ctx, domain.ApprovalQuery{States: []domain.ApprovalState{domain.ApprovalPending}})
			if len(items) == 1 {
				_, _ = hub.Answer(ctx, items[0].ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op-e2e")})
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	r, err := svc.Request(ctx, agent, domain.PythonPackage{Name: "rtpkg", Version: "3.0.0"}, "needs it", pypackages.RuntimeRequester+agent, 10*time.Second)
	if err != nil || r.Status != pypackages.RequestInstalled {
		t.Fatalf("runtime request: %+v %v", r, err)
	}
	svc.Stop()
	rm, err := op.RemovePythonPackage(opc, &pb.RemovePythonPackageOpRequest{CommandId: "p5-rm", Reason: "one-off analysis done", Target: agent, Name: "rtpkg"})
	if err != nil {
		t.Fatal(err)
	}
	if rm.GetGeneration() != filepath.Base(l2) {
		t.Fatalf("removing rtpkg must land on the content-addressed tinypkg-only generation %s, got %q", filepath.Base(l2), rm.GetGeneration())
	}
	if rows, _ := audit.Query(ctx, operator.AuditFilter{CommandID: "p5-rm"}); len(rows) != 1 || rows[0].ActionType != "remove_python_package" {
		t.Fatalf("command audit: %+v", rows)
	}
	// 3. Refusals.
	if _, err := op.RemovePythonPackage(opc, &pb.RemovePythonPackageOpRequest{CommandId: "p5-rm2", Reason: "x", Target: agent, Name: "tinypkg"}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "python_packages") {
		t.Fatalf("declared: %v", err)
	}
	if _, err := op.RemovePythonPackage(opc, &pb.RemovePythonPackageOpRequest{CommandId: "p5-rm3", Reason: "x", Target: agent, Name: "numpy"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown: %v", err)
	}
	// 4. The listing: audit trails, generations, sizes.
	lst, err := op.ListPythonPackages(opc, &pb.ListPythonPackagesOpRequest{Target: agent})
	if err != nil {
		t.Fatal(err)
	}
	trail := map[string][]string{}
	state := map[string]string{}
	for _, p := range lst.GetPackages() {
		for _, e := range p.GetEvents() {
			trail[p.GetName()] = append(trail[p.GetName()], e.GetAction())
		}
		state[p.GetName()] = p.GetState()
	}
	t.Logf("trails: %v", trail)
	last := func(n string) string { a := trail[n]; return a[len(a)-1] }
	if state["otherpkg"] != "approved" || last("otherpkg") != "removed" || state["rtpkg"] != "removed" || last("rtpkg") != "removed" || state["tinypkg"] != "approved" {
		t.Fatalf("states %v trails %v", state, trail)
	}
	var current int
	for _, l := range lst.GetLayers() {
		if l.GetCurrent() {
			current++
			if l.GetDir() != filepath.Base(l2) || l.GetBytes() < 0 {
				t.Fatalf("current layer: %+v", l)
			}
		}
	}
	if current != 1 || len(lst.GetBases()) != 1 || !lst.GetBases()[0].GetCurrent() || lst.GetTotalBytes() <= 0 || lst.GetRoot() != root {
		t.Fatalf("listing env: layers %+v bases %+v total %d", lst.GetLayers(), lst.GetBases(), lst.GetTotalBytes())
	}
	// 5. Asking for the removed package again: a fresh card.
	again, err := svc.Request(ctx, agent, domain.PythonPackage{Name: "rtpkg", Version: "3.0.0"}, "again", pypackages.RuntimeRequester+agent, 50*time.Millisecond)
	if err != nil || again.Status != pypackages.RequestPending {
		t.Fatalf("re-request after removal: %+v %v", again, err)
	}
}
