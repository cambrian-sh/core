package pypackages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/pyenv"
)

var dateutil = domain.PythonPackage{Name: "python-dateutil", Version: "2.9.0"}

var dateutilLock = domain.PythonPackageLock{Entries: []domain.PythonLockEntry{
	{Name: "python-dateutil", Version: "2.9.0", Hashes: []string{strings.Repeat("5", 64)}},
	{Name: "six", Version: "1.17.0", Hashes: []string{strings.Repeat("6", 64)}},
}}

// approveRuntime runs request_python_package for pkg and approves its card
// inside the window.
func (h *harness) approveRuntime(agent string, pkg domain.PythonPackage) RequestResult {
	h.t.Helper()
	go func() {
		for i := 0; i < 400; i++ {
			for _, it := range h.pending() {
				if strings.Contains(it.ToolName, pkg.Name) {
					_ = h.answer(it.ID, true, domain.OperatorApprover("op-1"))
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	r := h.request(agent, pkg, 10*time.Second)
	h.svc.wg.Wait()
	return r
}

func actionsOf(evs []domain.PythonPackageEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Action
	}
	return out
}

// P5 unit: removal builds a generation without the package, switches the
// agent to it (the running instance is recycled when idle), and audits the
// removal with the new generation; the record leaves the layer for good.
func TestRemove_BuildsAGenerationWithoutItAndAuditsIt(t *testing.T) {
	h, ch := runtimeHarness(t)
	h.env.locks["python-dateutil==2.9.0"] = dateutilLock
	if _, err := h.ensure("analyst"); err != nil {
		t.Fatal(err)
	}
	if r := h.approveRuntime("analyst", polars); r.Status != RequestInstalled {
		t.Fatalf("polars: %+v", r)
	}
	if r := h.approveRuntime("analyst", dateutil); r.Status != RequestInstalled {
		t.Fatalf("dateutil: %+v", r)
	}
	before, _ := h.svc.Current("analyst", base1)
	if !strings.Contains(before.Dir, "polars-1.9.0") || !strings.Contains(before.Dir, "python-dateutil-2.9.0") {
		t.Fatalf("layer before removal: %s", before.Dir)
	}
	changesBefore := len(ch.list())

	gen, err := h.svc.Remove(context.Background(), "analyst", "Polars", "op-1", "no longer needed")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := h.svc.Current("analyst", base1)
	if after.Dir == before.Dir || strings.Contains(after.Dir, "polars") || !strings.Contains(after.Dir, "python-dateutil") {
		t.Fatalf("layer after removal: %s (before %s)", after.Dir, before.Dir)
	}
	if gen != filepath.Base(after.Dir) {
		t.Fatalf("returned generation %q, current %q", gen, filepath.Base(after.Dir))
	}
	if _, err := os.Stat(before.Dir); err != nil {
		t.Fatalf("the old generation stays on disk for the running instance (GC removes it later): %v", err)
	}
	if len(ch.list()) != changesBefore+1 {
		t.Fatalf("the switch must recycle the agent's instances: %v", ch.list())
	}
	views, err := h.svc.Packages(context.Background(), "analyst")
	if err != nil {
		t.Fatal(err)
	}
	var pol *PackageView
	for i := range views {
		if views[i].Record.Name == "polars" {
			pol = &views[i]
		}
	}
	if pol == nil || pol.Record.State != domain.PythonPackageRemoved || !pol.Runtime || pol.Declared {
		t.Fatalf("polars record: %+v", pol)
	}
	last := pol.Events[len(pol.Events)-1]
	if last.Action != domain.PythonPackageActionRemoved || last.Actor != "op-1" || last.Generation != gen || !strings.Contains(last.Detail, "no longer needed") {
		t.Fatalf("removal audit: %+v (trail %v)", last, actionsOf(pol.Events))
	}
	// The next spawn keeps it out.
	if l, err := h.ensure("analyst"); err != nil || l.Dir != after.Dir {
		t.Fatalf("next spawn: %+v %v", l, err)
	}
	// Removing the last one: no layer at all.
	gen, err = h.svc.Remove(context.Background(), "analyst", "python-dateutil", "op-1", "done")
	if err != nil || gen != "" {
		t.Fatalf("last removal: %q %v", gen, err)
	}
	if cur, _ := h.svc.Current("analyst", base1); cur.Dir != "" {
		t.Fatalf("no packages left ⇒ no layer: %+v", cur)
	}
}

// Cautious choice (a): asking for a removed package again raises a fresh card;
// the overruled approval is not reused.
func TestRemove_ReAddNeedsAFreshApproval(t *testing.T) {
	h, _ := runtimeHarness(t)
	if r := h.approveRuntime("analyst", polars); r.Status != RequestInstalled {
		t.Fatalf("%+v", r)
	}
	if _, err := h.svc.Remove(context.Background(), "analyst", "polars", "op-1", "test"); err != nil {
		t.Fatal(err)
	}
	r := h.request("analyst", polars, 20*time.Millisecond)
	if r.Status != RequestPending {
		t.Fatalf("a removed package asked again must wait for a fresh decision: %+v", r)
	}
	if items := h.pending(); len(items) != 1 || items[0].ID != r.ApprovalID {
		t.Fatalf("one fresh card: %+v", items)
	}
}

// Cautious choice (b): a package the profile declares is not removed by an
// operator command (the next spawn would reinstall it); the refusal names the
// profile. Unknown packages are refused as not found.
func TestRemove_RefusesDeclaredAndUnknown(t *testing.T) {
	h, _ := runtimeHarness(t)
	h.svc.cfg.Declared = func(context.Context, string) []domain.PythonPackage { return []domain.PythonPackage{polars} }
	_, err := h.svc.Remove(context.Background(), "analyst", "polars", "op-1", "x")
	if !errors.Is(err, ErrDeclaredPackage) || !strings.Contains(err.Error(), "python_packages") {
		t.Fatalf("declared: %v", err)
	}
	_, err = h.svc.Remove(context.Background(), "analyst", "numpy", "op-1", "x")
	if !errors.Is(err, ErrNoSuchPackage) {
		t.Fatalf("unknown: %v", err)
	}
}

// D7.10.3: a package dropped from the profile is built out of the layer at the
// next check and the removal is audited; the record stays approved, so
// declaring it again reinstalls it without a card.
func TestEnsure_ProfileDropIsAudited(t *testing.T) {
	h := newHarness(t)
	h.env.locks["python-dateutil==2.9.0"] = dateutilLock
	if _, err := h.ensure("analyst", polars, dateutil); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("%v", err)
	}
	for _, it := range h.pending() {
		if err := h.answer(it.ID, true, domain.OperatorApprover("op-1")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.ensure("analyst", polars, dateutil); err != nil {
		t.Fatal(err)
	}
	l, err := h.ensure("analyst", dateutil)
	if err != nil || strings.Contains(l.Dir, "polars") {
		t.Fatalf("after the profile drop: %+v %v", l, err)
	}
	var rec domain.PythonPackageApproval
	for _, r := range h.store.filter(func(r domain.PythonPackageApproval) bool { return r.Name == "polars" }) {
		rec = r
	}
	acts := h.store.actions(rec.ID)
	if rec.State != domain.PythonPackageApproved || acts[len(acts)-1] != domain.PythonPackageActionRemoved {
		t.Fatalf("polars: state %s, trail %v", rec.State, acts)
	}
	// A second check audits nothing new.
	if _, err := h.ensure("analyst", dateutil); err != nil {
		t.Fatal(err)
	}
	if n := len(h.store.actions(rec.ID)); n != len(acts) {
		t.Fatalf("removal audited twice: %v", h.store.actions(rec.ID))
	}
	// Declared again: reinstalled, no card.
	if _, err := h.ensure("analyst", polars, dateutil); err != nil {
		t.Fatalf("re-declared: %v", err)
	}
	if len(h.pending()) != 0 {
		t.Fatal("re-declaring an approved package raises no card")
	}
}

// Retry re-runs the check: an approved package whose install failed is
// installed on the operator's command.
func TestRetry_InstallsAfterAFailure(t *testing.T) {
	h, _ := runtimeHarness(t)
	h.env.mu.Lock()
	h.env.root = filepath.Join(h.env.root, "missing", "\x00bad") // BuildLayer fails
	h.env.mu.Unlock()
	r := h.approveRuntime("analyst", polars)
	if r.Status != RequestPending {
		t.Fatalf("an install failure after approval leaves it pending: %+v", r)
	}
	rec := h.store.filter(func(domain.PythonPackageApproval) bool { return true })[0]
	if acts := h.store.actions(rec.ID); acts[len(acts)-1] != domain.PythonPackageActionInstallFailed {
		t.Fatalf("trail: %v", acts)
	}
	h.env.mu.Lock()
	h.env.root = t.TempDir()
	h.env.mu.Unlock()
	gen, err := h.svc.Retry(context.Background(), "analyst")
	if err != nil || gen == "" {
		t.Fatalf("retry: %q %v", gen, err)
	}
	if acts := h.store.actions(rec.ID); acts[len(acts)-1] != domain.PythonPackageActionInstalled {
		t.Fatalf("trail after retry: %v", acts)
	}
}

// GCKeep blocks package checks until the collector releases it.
func TestGCKeep_BlocksChecksUntilReleased(t *testing.T) {
	h, _ := runtimeHarness(t)
	if _, err := h.ensure("analyst"); err != nil {
		t.Fatal(err)
	}
	layers, release := h.svc.GCKeep()
	if d, ok := layers["analyst"]; !ok || d != "" {
		t.Fatalf("layers: %v", layers)
	}
	done := make(chan struct{})
	go func() {
		_, _ = h.svc.Ensure(context.Background(), "analyst", nil, base1)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("a package check ran while GC held the layers")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	<-done
}

// Admin composes the listing: records with trails, the env inventory filtered
// to the target, its current layer and the last GC report.
func TestAdmin_List(t *testing.T) {
	h, _ := runtimeHarness(t)
	if r := h.approveRuntime("analyst", polars); r.Status != RequestInstalled {
		t.Fatalf("%+v", r)
	}
	b := &pyenv.Builder{Root: t.TempDir()}
	a, err := NewAdmin(h.svc, b, fakeCollector{})
	if err != nil {
		t.Fatal(err)
	}
	lst, err := a.List(context.Background(), "analyst")
	if err != nil {
		t.Fatal(err)
	}
	if len(lst.Packages) != 1 || lst.Packages[0].Record.Name != "polars" || len(lst.Packages[0].Events) < 3 || lst.Layers["analyst"] == "" {
		t.Fatalf("listing: %+v", lst)
	}
	if _, err := NewAdmin(nil, b, fakeCollector{}); err == nil {
		t.Fatal("NewAdmin must refuse a missing part")
	}
}

type fakeCollector struct{}

func (fakeCollector) Run(context.Context) pyenv.GCReport { return pyenv.GCReport{FreedBytes: 1} }
func (fakeCollector) Last() pyenv.GCReport               { return pyenv.GCReport{} }
