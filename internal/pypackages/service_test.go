package pypackages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/pyenv"
)

// fakeEnv stands in for pyenv.Builder: configured locks per spec, layers as
// directories under root, every resolve and build counted.
type fakeEnv struct {
	root string

	mu       sync.Mutex
	locks    map[string]domain.PythonPackageLock // spec → lock
	errs     map[string]error                    // spec → resolve error
	resolves int
	builds   int
	prepared []string
}

func newFakeEnv(t *testing.T) *fakeEnv {
	return &fakeEnv{root: t.TempDir(), locks: map[string]domain.PythonPackageLock{}, errs: map[string]error{}}
}

func (f *fakeEnv) ResolvePackage(_ context.Context, _ pyenv.Interpreter, pkg domain.PythonPackage) (domain.PythonPackageLock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if err := f.errs[pkg.Spec()]; err != nil {
		return domain.PythonPackageLock{}, err
	}
	l, ok := f.locks[pkg.Spec()]
	if !ok {
		return domain.PythonPackageLock{}, fmt.Errorf("%s: %w: not on the index", pkg.Spec(), pyenv.ErrUnresolvable)
	}
	return l, nil
}

func (f *fakeEnv) dir(agent string, base pyenv.Interpreter, entries []domain.PythonLockEntry) string {
	var parts []string
	for _, e := range entries {
		parts = append(parts, e.Name+"-"+e.Version)
	}
	return filepath.Join(f.root, "layers", agent, filepath.Base(base.Home)+"_"+strings.Join(parts, "_"))
}

func (f *fakeEnv) FindLayer(agent string, base pyenv.Interpreter, entries []domain.PythonLockEntry) (pyenv.Layer, bool) {
	d := f.dir(agent, base, entries)
	if _, err := os.Stat(d); err != nil {
		return pyenv.Layer{}, false
	}
	return pyenv.Layer{Dir: d, Boot: filepath.Join(d, "boot"), ID: strings.Repeat("a", 64), Base: base.Home, Reused: true}, true
}

func (f *fakeEnv) BuildLayer(_ context.Context, req pyenv.LayerRequest) (pyenv.Layer, error) {
	f.mu.Lock()
	f.builds++
	f.mu.Unlock()
	d := f.dir(req.Agent, req.Base, req.Entries)
	if err := os.MkdirAll(filepath.Join(d, "boot"), 0o755); err != nil {
		return pyenv.Layer{}, err
	}
	if req.Prepare != nil {
		if err := req.Prepare(d); err == nil {
			f.mu.Lock()
			f.prepared = append(f.prepared, d)
			f.mu.Unlock()
		}
	}
	return pyenv.Layer{Dir: d, Boot: filepath.Join(d, "boot"), ID: strings.Repeat("b", 64), Base: req.Base.Home}, nil
}

func (f *fakeEnv) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolves, f.builds
}

var (
	polarsLock = domain.PythonPackageLock{Index: "index https://pypi.org/simple", Entries: []domain.PythonLockEntry{
		{Name: "polars", Version: "1.9.0", Hashes: []string{strings.Repeat("1", 64), strings.Repeat("2", 64)}},
		{Name: "pyarrow", Version: "17.0.0", Hashes: []string{strings.Repeat("3", 64)}},
	}}
	polars2Lock = domain.PythonPackageLock{Entries: []domain.PythonLockEntry{
		{Name: "polars", Version: "1.10.0", Hashes: []string{strings.Repeat("4", 64)}},
	}}
	base1 = pyenv.Interpreter{Path: "/env/base/gen1/bin/python3", Home: "/env/base/gen1"}
)

type harness struct {
	t     *testing.T
	store *memStore
	astor domain.ApprovalStore
	hub   *domain.AddressedHub
	env   *fakeEnv
	svc   *Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, store: newMemStore(), astor: domain.NewMemoryApprovalStore(), env: newFakeEnv(t)}
	h.env.locks["polars==1.9.0"] = polarsLock
	h.env.locks["polars==1.10.0"] = polars2Lock
	h.restart()
	return h
}

// restart is a kernel restart: a new hub and service over the same durable
// stores (approvals and package records).
func (h *harness) restart() {
	h.t.Helper()
	if h.svc != nil {
		h.svc.Stop()
	}
	h.hub = domain.NewAddressedHub(domain.AddressedHubConfig{Store: h.astor})
	svc, err := New(h.store, h.hub, h.env, Config{ApprovalTTL: time.Hour})
	if err != nil {
		h.t.Fatal(err)
	}
	h.svc = svc
}

func (h *harness) ensure(agent string, decl ...domain.PythonPackage) (pyenv.Layer, error) {
	return h.svc.Ensure(context.Background(), agent, decl, base1)
}

func (h *harness) pending() []domain.ApprovalItem {
	items, err := h.hub.List(context.Background(), domain.ApprovalQuery{States: []domain.ApprovalState{domain.ApprovalPending}})
	if err != nil {
		h.t.Fatal(err)
	}
	return items
}

func (h *harness) answer(id string, approve bool, by domain.Approver) error {
	_, err := h.hub.Answer(context.Background(), id, domain.ApprovalAnswer{Approved: approve, By: by})
	return err
}

var polars = domain.PythonPackage{Name: "polars", Version: "1.9.0"}

// The agent is held until an Operator approves; the card lists the full
// dependency set with every hash; approval installs the layer and the agent
// starts with it (D7.6.3, D7.8.1, D7.8.6).
func TestEnsure_HeldUntilApprovedThenInstalled(t *testing.T) {
	h := newHarness(t)
	_, err := h.ensure("analyst", polars)
	if !errors.Is(err, ErrNotApproved) {
		t.Fatalf("an unapproved declaration must refuse the spawn, got %v", err)
	}
	items := h.pending()
	if len(items) != 1 {
		t.Fatalf("one card per declared package, got %d", len(items))
	}
	it := items[0]
	if !strings.Contains(err.Error(), "polars==1.9.0") || !strings.Contains(err.Error(), it.ID) {
		t.Errorf("the refusal must name the package and the approval it waits on: %v", err)
	}
	if it.Kind != domain.ApprovalKindPythonPackage || it.AgentID != "analyst" || it.ToolName != "python_package:polars==1.9.0" {
		t.Errorf("card shape: %+v", it)
	}
	if it.AllowStanding || it.RuleSummary() != "operator" {
		t.Errorf("python_package is Operator-only with no standing approvals: rule=%q standing=%v", it.RuleSummary(), it.AllowStanding)
	}
	for _, want := range []string{"polars==1.9.0", "pyarrow==17.0.0", "sha256:" + strings.Repeat("1", 64),
		"sha256:" + strings.Repeat("2", 64), "sha256:" + strings.Repeat("3", 64), "agent analyst", "wheels only"} {
		if !strings.Contains(it.Object, want) {
			t.Errorf("card text lacks %q:\n%s", want, it.Object)
		}
	}
	if !strings.Contains(it.ArgsPreview, "+1 dependencies") {
		t.Errorf("preview: %q", it.ArgsPreview)
	}
	// A second spawn while pending joins the same card.
	if _, err := h.ensure("analyst", polars); !errors.Is(err, ErrNotApproved) {
		t.Fatal(err)
	}
	if n := len(h.pending()); n != 1 {
		t.Fatalf("a repeated spawn must not raise a second card, got %d", n)
	}
	if err := h.answer(it.ID, true, domain.OperatorApprover("op-1")); err != nil {
		t.Fatal(err)
	}
	l, err := h.ensure("analyst", polars)
	if err != nil || l.Dir == "" {
		t.Fatalf("approved ⇒ layer installed and the agent starts: %+v %v", l, err)
	}
	if cur, err := h.svc.Current("analyst", base1); err != nil || cur.Dir != l.Dir {
		t.Fatalf("Current must return the installed layer: %+v %v", cur, err)
	}
	rec := h.store.filter(func(domain.PythonPackageApproval) bool { return true })[0]
	if rec.State != domain.PythonPackageApproved || rec.DecidedBy == "" {
		t.Errorf("record: %+v", rec)
	}
	if got := strings.Join(h.store.actions(rec.ID), ","); got != "requested,approved,installed" {
		t.Errorf("audit trail: %s", got)
	}
}

// Only the Operator role answers a package card; the hub refuses anyone else
// at answer time (D7.8.3).
func TestAnswerByNonOperatorRefused(t *testing.T) {
	h := newHarness(t)
	_, _ = h.ensure("analyst", polars)
	it := h.pending()[0]
	someone := domain.Approver{Principal: domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "u-1"}, Roles: []string{"member"}}
	if err := h.answer(it.ID, true, someone); !errors.Is(err, domain.ErrApprovalNotAddressee) {
		t.Fatalf("a non-operator answer must be refused, got %v", err)
	}
	if _, err := h.ensure("analyst", polars); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("still held: %v", err)
	}
}

// A kernel restart while the card is pending keeps it (no second card); the
// answer given to the new process installs (D7.8.2, E2E requirement).
func TestRestartWhilePendingKeepsTheCard(t *testing.T) {
	h := newHarness(t)
	_, _ = h.ensure("analyst", polars)
	first := h.pending()[0].ID
	h.restart()
	if _, err := h.ensure("analyst", polars); !errors.Is(err, ErrNotApproved) || !strings.Contains(err.Error(), first) {
		t.Fatalf("after a restart the agent still waits on the same card %s: %v", first, err)
	}
	if items := h.pending(); len(items) != 1 || items[0].ID != first {
		t.Fatalf("the pending card survives a restart unchanged: %+v", items)
	}
	if err := h.answer(first, true, domain.OperatorApprover("op-1")); err != nil {
		t.Fatal(err)
	}
	if l, err := h.ensure("analyst", polars); err != nil || l.Dir == "" {
		t.Fatalf("the restarted kernel installs after the approval: %v", err)
	}
}

// An approval answered while the kernel was down (the handler never ran) is
// picked up from the card at the next spawn.
func TestApprovalRecordedOnlyOnTheCardIsReconciled(t *testing.T) {
	h := newHarness(t)
	_, _ = h.ensure("analyst", polars)
	it := h.pending()[0]
	// Approve through a hub with no handler registered (a process that died
	// before boot wired the kind).
	bare := domain.NewAddressedHub(domain.AddressedHubConfig{Store: h.astor})
	if _, err := bare.Answer(context.Background(), it.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op-2")}); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if l, err := h.ensure("analyst", polars); err != nil || l.Dir == "" {
		t.Fatalf("the approval on the card must be honoured: %v", err)
	}
}

// Reinstalling an approved lock (the layer was lost) raises no card and is
// audited as a reinstall (D7.8.5); a new version is a new card (upgrade).
func TestReinstallNoCardUpgradeNewCard(t *testing.T) {
	h := newHarness(t)
	_, _ = h.ensure("analyst", polars)
	_ = h.answer(h.pending()[0].ID, true, domain.OperatorApprover("op-1"))
	l, err := h.ensure("analyst", polars)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(l.Dir); err != nil {
		t.Fatal(err)
	}
	h.restart()
	l2, err := h.ensure("analyst", polars)
	if err != nil || l2.Dir == "" {
		t.Fatalf("a lost layer is rebuilt from the approved lock: %v", err)
	}
	if n := len(h.pending()); n != 0 {
		t.Fatalf("a reinstall must raise no card, got %d pending", n)
	}
	rec := h.store.filter(func(domain.PythonPackageApproval) bool { return true })[0]
	if got := strings.Join(h.store.actions(rec.ID), ","); got != "requested,approved,installed,reinstalled" {
		t.Errorf("audit trail: %s", got)
	}
	// Upgrade: the profile now declares 1.10.0.
	_, err = h.ensure("analyst", domain.PythonPackage{Name: "polars", Version: "1.10.0"})
	if !errors.Is(err, ErrNotApproved) {
		t.Fatalf("an upgrade is a new version: a new card, agent held: %v", err)
	}
	items := h.pending()
	if len(items) != 1 || items[0].ToolName != "python_package:polars==1.10.0" {
		t.Fatalf("upgrade card: %+v", items)
	}
}

// Approval scope is per agent (A1-Q9): agent B asking for what A has is a new
// card, pre-filled with "already approved for".
func TestApprovalIsPerAgent(t *testing.T) {
	h := newHarness(t)
	_, _ = h.ensure("analyst", polars)
	_ = h.answer(h.pending()[0].ID, true, domain.OperatorApprover("op-1"))
	if _, err := h.ensure("analyst", polars); err != nil {
		t.Fatal(err)
	}
	_, err := h.ensure("reporter", polars)
	if !errors.Is(err, ErrNotApproved) {
		t.Fatalf("another agent needs its own approval: %v", err)
	}
	items := h.pending()
	if len(items) != 1 || items[0].AgentID != "reporter" {
		t.Fatalf("one new card for reporter: %+v", items)
	}
	if !strings.Contains(items[0].Object, "already approved at this version for: analyst (same lock)") {
		t.Errorf("the card notes the existing approval:\n%s", items[0].Object)
	}
	if cur, _ := h.svc.Current("reporter", base1); cur.Dir != "" {
		t.Errorf("reporter must have no layer: %+v", cur)
	}
}

// A denial keeps the agent stopped with the denial named.
func TestDeniedStaysStopped(t *testing.T) {
	h := newHarness(t)
	_, _ = h.ensure("analyst", polars)
	_ = h.answer(h.pending()[0].ID, false, domain.OperatorApprover("op-1"))
	_, err := h.ensure("analyst", polars)
	if !errors.Is(err, ErrNotApproved) || !strings.Contains(err.Error(), "denied") || !strings.Contains(err.Error(), "polars==1.9.0") {
		t.Fatalf("denied ⇒ held with the denial named: %v", err)
	}
	if n := len(h.pending()); n != 0 {
		t.Fatalf("a denial raises no new card, got %d", n)
	}
}

// An expired card is asked again on the next spawn (fail closed, D7.8.7).
func TestExpiredCardIsAskedAgain(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.svc.cfg.Now = func() time.Time { return now }
	_, _ = h.ensure("analyst", polars)
	first := h.pending()[0]
	// Expire it through the hub's own path (answering after the window).
	expHub := domain.NewAddressedHub(domain.AddressedHubConfig{Store: h.astor, Now: func() time.Time { return now.Add(2 * time.Hour) }})
	if _, err := expHub.Answer(context.Background(), first.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op")}); !errors.Is(err, domain.ErrApprovalResolved) {
		t.Fatalf("answering after the TTL must expire the card: %v", err)
	}
	_, err := h.ensure("analyst", polars)
	if !errors.Is(err, ErrNotApproved) {
		t.Fatal(err)
	}
	items := h.pending()
	if len(items) != 1 || items[0].ID == first.ID {
		t.Fatalf("an expired card is replaced by a new one: %+v", items)
	}
	rec := h.store.filter(func(domain.PythonPackageApproval) bool { return true })[0]
	if rec.ApprovalID != items[0].ID || rec.State != domain.PythonPackagePending {
		t.Errorf("record re-pointed at the new card: %+v", rec)
	}
}

// A conflict with the base, or a version with no wheel, refuses the agent
// with the package named and raises no card (failure table: resolution
// failure).
func TestUnresolvableRefusedWithoutCard(t *testing.T) {
	h := newHarness(t)
	h.env.errs["numpy==1.0.0"] = fmt.Errorf("numpy==1.0.0: %w at 2.1.0, and a layer cannot change a base version: %w", pyenv.ErrInBase, pyenv.ErrUnresolvable)
	_, err := h.ensure("analyst", domain.PythonPackage{Name: "numpy", Version: "1.0.0"})
	if !errors.Is(err, ErrDeclaration) || !strings.Contains(err.Error(), "numpy==1.0.0") || !strings.Contains(err.Error(), "base") {
		t.Fatalf("conflict with the base must refuse naming the package: %v", err)
	}
	_, err = h.ensure("analyst", domain.PythonPackage{Name: "nowheel", Version: "0.1"})
	if !errors.Is(err, ErrDeclaration) || !strings.Contains(err.Error(), "nowheel==0.1") {
		t.Fatalf("unresolvable must refuse naming the package: %v", err)
	}
	if n := len(h.pending()); n != 0 {
		t.Fatalf("no card for an unresolvable package, got %d", n)
	}
}

// A package already in the base at that version needs no layer and no card.
func TestInBaseSameVersionNeedsNothing(t *testing.T) {
	h := newHarness(t)
	h.env.errs["numpy==2.1.0"] = fmt.Errorf("numpy==2.1.0: %w at that version; it needs no layer", pyenv.ErrInBase)
	l, err := h.ensure("analyst", domain.PythonPackage{Name: "numpy", Version: "2.1.0"})
	if err != nil || l.Dir != "" || len(h.pending()) != 0 {
		t.Fatalf("in base: no layer, no card: %+v %v", l, err)
	}
}

// No durable store ⇒ declarations refused naming the migration; no
// declarations ⇒ nothing changes.
func TestNoStoreFailsClosedForDeclarationsOnly(t *testing.T) {
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{})
	svc, err := New(nil, hub, newFakeEnv(t), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ensure(context.Background(), "a", []domain.PythonPackage{polars}, base1); !errors.Is(err, ErrNotApproved) || !strings.Contains(err.Error(), "0057") {
		t.Fatalf("no store ⇒ refused naming the migration: %v", err)
	}
	if l, err := svc.Ensure(context.Background(), "a", nil, base1); err != nil || l.Dir != "" {
		t.Fatalf("no declaration ⇒ no layer, no error: %v", err)
	}
	if _, err := New(nil, nil, newFakeEnv(t), Config{}); err == nil {
		t.Error("a hub is required")
	}
	if _, err := New(nil, hub, nil, Config{}); err == nil {
		t.Error("an env is required")
	}
}

// Two approved packages that pin one dependency at different versions cannot
// share a layer: refused with both named.
func TestUnionConflictRefused(t *testing.T) {
	h := newHarness(t)
	h.env.locks["a==1"] = domain.PythonPackageLock{Entries: []domain.PythonLockEntry{{Name: "a", Version: "1", Hashes: []string{"aa"}}, {Name: "dep", Version: "1", Hashes: []string{"d1"}}}}
	h.env.locks["b==1"] = domain.PythonPackageLock{Entries: []domain.PythonLockEntry{{Name: "b", Version: "1", Hashes: []string{"bb"}}, {Name: "dep", Version: "2", Hashes: []string{"d2"}}}}
	decl := []domain.PythonPackage{{Name: "a", Version: "1"}, {Name: "b", Version: "1"}}
	_, _ = h.ensure("x", decl...)
	for _, it := range h.pending() {
		_ = h.answer(it.ID, true, domain.OperatorApprover("op"))
	}
	_, err := h.ensure("x", decl...)
	if !errors.Is(err, ErrDeclaration) || !strings.Contains(err.Error(), "dep==1") || !strings.Contains(err.Error(), "dep==2") {
		t.Fatalf("conflicting approved locks must refuse naming both: %v", err)
	}
}

// An approval installs the layer in the background when the service can see
// the declaration, so the agent's next spawn finds it ready.
func TestApprovalInstallsInTheBackground(t *testing.T) {
	h := newHarness(t)
	h.svc.cfg.Base = func() (pyenv.Interpreter, error) { return base1, nil }
	h.svc.cfg.Declared = func(context.Context, string) []domain.PythonPackage { return []domain.PythonPackage{polars} }
	_, _ = h.ensure("analyst", polars)
	if err := h.answer(h.pending()[0].ID, true, domain.OperatorApprover("op-1")); err != nil {
		t.Fatal(err)
	}
	h.svc.Stop() // waits for the background install
	if _, builds := h.env.counts(); builds != 1 {
		t.Fatalf("the approval must install once in the background, builds=%d", builds)
	}
	if cur, err := h.svc.Current("analyst", base1); err != nil || cur.Dir == "" {
		t.Fatalf("the layer is current after the background install: %+v %v", cur, err)
	}
}

// Current refuses a layer checked on another base generation.
func TestCurrentRefusesAnotherBase(t *testing.T) {
	h := newHarness(t)
	_, _ = h.ensure("analyst", polars)
	_ = h.answer(h.pending()[0].ID, true, domain.OperatorApprover("op-1"))
	if _, err := h.ensure("analyst", polars); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Current("analyst", pyenv.Interpreter{Home: "/env/base/gen2"}); err == nil {
		t.Fatal("a layer from another base generation must not be launched")
	}
}
