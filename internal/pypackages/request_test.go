package pypackages

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/pyenv"
)

// runtimeHarness is the harness with the runtime config P4 needs: the base,
// the declaration lookup and a layer-change recorder.
func runtimeHarness(t *testing.T) (*harness, *changes) {
	t.Helper()
	h := newHarness(t)
	ch := &changes{}
	h.svc.cfg.Base = func() (pyenv.Interpreter, error) { return base1, nil }
	h.svc.cfg.Declared = func(context.Context, string) []domain.PythonPackage { return nil }
	h.svc.cfg.OnLayerChange = ch.add
	h.svc.cfg.RequestPoll = 5 * time.Millisecond
	return h, ch
}

type changes struct {
	mu     sync.Mutex
	agents []string
}

func (c *changes) add(a string) { c.mu.Lock(); c.agents = append(c.agents, a); c.mu.Unlock() }
func (c *changes) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.agents...)
}

func (h *harness) request(agent string, pkg domain.PythonPackage, wait time.Duration) RequestResult {
	h.t.Helper()
	r, err := h.svc.Request(context.Background(), agent, pkg, "to read the quarterly parquet files", RuntimeRequester+agent, wait)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// D7.9.3-4: no decision inside the window ⇒ pending with the approval id; a
// repeat while pending returns the same item, never a second card; the card
// says it was requested at runtime and carries the agent's rationale.
func TestRequest_PendingAfterWindowSameItemOnRepeat(t *testing.T) {
	h, _ := runtimeHarness(t)
	start := time.Now()
	r := h.request("analyst", polars, 40*time.Millisecond)
	if r.Status != RequestPending || r.ApprovalID == "" {
		t.Fatalf("no decision ⇒ pending with the approval id: %+v", r)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the tool must not wait beyond its window")
	}
	again := h.request("analyst", polars, 10*time.Millisecond)
	if again.Status != RequestPending || again.ApprovalID != r.ApprovalID {
		t.Fatalf("a repeat while pending returns the same item: %+v vs %+v", again, r)
	}
	items := h.pending()
	if len(items) != 1 || items[0].ID != r.ApprovalID {
		t.Fatalf("one card, the one returned: %+v", items)
	}
	it := items[0]
	for _, want := range []string{"requested at runtime", "polars==1.9.0", "pyarrow==17.0.0", "quarterly parquet"} {
		if !strings.Contains(it.ArgsPreview+"\n"+it.Object, want) {
			t.Errorf("card lacks %q: preview=%q\n%s", want, it.ArgsPreview, it.Object)
		}
	}
	rec := h.store.filter(func(domain.PythonPackageApproval) bool { return true })[0]
	if rec.RequestedBy != "agent:analyst" || !IsRuntimeRequest(rec) {
		t.Errorf("requested_by: %q", rec.RequestedBy)
	}
}

// An Operator approves inside the window ⇒ installed into the agent's own
// layer; a running instance on the old layer is told to recycle; asking again
// ⇒ already_installed.
func TestRequest_ApprovedInsideWindowIsInstalled(t *testing.T) {
	h, ch := runtimeHarness(t)
	// The agent is running with no layer (its spawn's Ensure had nothing).
	if _, err := h.ensure("analyst"); err != nil {
		t.Fatal(err)
	}
	go func() {
		for i := 0; i < 400; i++ {
			if items := h.pending(); len(items) == 1 {
				_ = h.answer(items[0].ID, true, domain.OperatorApprover("op-1"))
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	r := h.request("analyst", polars, 10*time.Second)
	if r.Status != RequestInstalled {
		t.Fatalf("approved inside the window ⇒ installed: %+v", r)
	}
	h.svc.Stop() // the hub handler's background Ensure has finished too
	cur, err := h.svc.Current("analyst", base1)
	if err != nil || cur.Dir == "" || !strings.Contains(cur.Dir, "polars-1.9.0") {
		t.Fatalf("the agent's layer holds the package: %+v %v", cur, err)
	}
	if got := ch.list(); len(got) == 0 || got[0] != "analyst" {
		t.Fatalf("the layer change must recycle the agent's instances: %v", got)
	}
	if again := h.request("analyst", polars, time.Second); again.Status != RequestAlreadyInstalled {
		t.Fatalf("asked again ⇒ already_installed: %+v", again)
	}
	// The next spawn keeps the runtime-approved package without a declaration.
	l, err := h.ensure("analyst")
	if err != nil || l.Dir != cur.Dir {
		t.Fatalf("a runtime approval belongs to the agent's layer at its next spawn: %+v %v", l, err)
	}
	// Another agent gets nothing from it.
	if l, err := h.ensure("reporter"); err != nil || l.Dir != "" {
		t.Fatalf("approvals are per agent: %+v %v", l, err)
	}
}

// Approval after the window applies to the agent's later runs (the hub handler
// installs it in the background).
func TestRequest_ApprovalAfterWindowAppliesLater(t *testing.T) {
	h, _ := runtimeHarness(t)
	r := h.request("analyst", polars, 10*time.Millisecond)
	if r.Status != RequestPending {
		t.Fatalf("%+v", r)
	}
	if err := h.answer(r.ApprovalID, true, domain.OperatorApprover("op-1")); err != nil {
		t.Fatal(err)
	}
	h.svc.Stop()
	if cur, err := h.svc.Current("analyst", base1); err != nil || cur.Dir == "" {
		t.Fatalf("approved after the window ⇒ installed for later runs: %+v %v", cur, err)
	}
}

// Denied ⇒ denied (no new card); unresolvable ⇒ the reason, no card; a
// package already in the base ⇒ already_installed.
func TestRequest_DeniedUnresolvableAndInBase(t *testing.T) {
	h, _ := runtimeHarness(t)
	r := h.request("analyst", polars, 10*time.Millisecond)
	if err := h.answer(r.ApprovalID, false, domain.OperatorApprover("op-1")); err != nil {
		t.Fatal(err)
	}
	d := h.request("analyst", polars, 10*time.Millisecond)
	if d.Status != RequestDenied || !strings.Contains(d.Detail, "denied") || len(h.pending()) != 0 {
		t.Fatalf("denied ⇒ denied, no new card: %+v pending=%d", d, len(h.pending()))
	}

	u := h.request("analyst", domain.PythonPackage{Name: "nosuchpkg", Version: "0.0.1"}, time.Second)
	if u.Status != RequestUnresolvable || !strings.Contains(u.Detail, "not on the index") || len(h.pending()) != 0 {
		t.Fatalf("unresolvable ⇒ the reason, no card: %+v", u)
	}
	bad := h.request("analyst", domain.PythonPackage{Name: "polars", Version: ">=1"}, time.Second)
	if bad.Status != RequestUnresolvable {
		t.Fatalf("a non-exact version is refused before resolving: %+v", bad)
	}

	h.env.errs["numpy==2.1.0"] = fmt.Errorf("numpy==2.1.0: %w at that version; it needs no layer", pyenv.ErrInBase)
	b := h.request("analyst", domain.PythonPackage{Name: "numpy", Version: "2.1.0"}, time.Second)
	if b.Status != RequestAlreadyInstalled {
		t.Fatalf("in base ⇒ already_installed: %+v", b)
	}
}

// --- the kernel tool ---

type toolFixture struct {
	defs     map[string]*domain.AgentDefinition
	mans     map[string]*domain.AgentManifest
	leases   map[domain.LeaseID]string
	children map[domain.LeaseID]bool
}

func (f *toolFixture) deps() ToolDeps {
	return ToolDeps{
		LeaseAgent: func(l domain.LeaseID) (string, bool) { a, ok := f.leases[l]; return a, ok },
		IsChild:    func(l domain.LeaseID) bool { return f.children[l] },
		Agent: func(_ context.Context, id string) (*domain.AgentDefinition, *domain.AgentManifest, error) {
			d, ok := f.defs[id]
			if !ok {
				return nil, nil, fmt.Errorf("no agent %s", id)
			}
			return d, f.mans[id], nil
		},
	}
}

func newToolFixture() *toolFixture {
	return &toolFixture{
		defs: map[string]*domain.AgentDefinition{
			"analyst": {ID: "analyst", Runtime: domain.RuntimePython},
			"globber": {ID: "globber", Runtime: domain.RuntimePython},
			"bare":    {ID: "bare", Runtime: domain.RuntimePython},
			"jsagent": {ID: "jsagent", Runtime: domain.RuntimeBun},
		},
		mans: map[string]*domain.AgentManifest{
			"analyst": {ToolAllowlist: []string{"web_search", ToolName}},
			"globber": {ToolAllowlist: []string{"*"}},
			"bare":    {},
			"jsagent": {ToolAllowlist: []string{ToolName}},
		},
		leases: map[domain.LeaseID]string{"l-analyst": "analyst", "l-child": "analyst", "l-glob": "globber",
			"l-bare": "bare", "l-js": "jsagent", "l-ghost": "ghost"},
		children: map[domain.LeaseID]bool{"l-child": true},
	}
}

// D7.9.1-2: offered only to a kernel-run Python agent whose allowlist names
// the tool exactly, never to a subtask worker; the call refuses the same set.
// Its effect is admin.
func TestTool_GatingAndEffect(t *testing.T) {
	h, _ := runtimeHarness(t)
	f := newToolFixture()
	tool, err := NewTool(h.svc, 20*time.Millisecond, f.deps())
	if err != nil {
		t.Fatal(err)
	}
	if fx := tool.Spec().Effects; len(fx) != 1 || fx[0] != domain.EffectAdmin {
		t.Fatalf("effect admin: %v", fx)
	}
	for lease, want := range map[domain.LeaseID]bool{
		"l-analyst": true, "l-child": false, "l-glob": false, "l-bare": false, "l-js": false, "l-ghost": false, "l-unknown": false,
	} {
		if got := tool.OfferedTo(context.Background(), lease); got != want {
			t.Errorf("OfferedTo(%s) = %v, want %v", lease, got, want)
		}
	}
	args := []byte(`{"name":"polars","version":"1.9.0","rationale":"parquet"}`)
	for _, c := range []struct {
		agent string
		lease domain.LeaseID
		want  string
	}{
		{"globber", "l-glob", "allowlist"},
		{"analyst", "l-child", "subtask"},
		{"jsagent", "l-js", "Python agents"},
		{"", "l-analyst", "authenticated"},
	} {
		if _, err := tool.Call(context.Background(), domain.KernelToolCall{AgentID: c.agent, ArgsJSON: args, SessionTokenID: c.lease}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s/%s: want a refusal naming %q, got %v", c.agent, c.lease, c.want, err)
		}
	}
	if len(h.pending()) != 0 {
		t.Fatal("a refused call raises no card")
	}
	if _, err := tool.Call(context.Background(), domain.KernelToolCall{AgentID: "analyst", SessionTokenID: "l-analyst",
		ArgsJSON: []byte(`{"name":"polars","version":"1.9.0"}`)}); err == nil || !strings.Contains(err.Error(), "rationale") {
		t.Fatalf("a call without rationale is refused: %v", err)
	}
	if _, err := tool.Call(context.Background(), domain.KernelToolCall{AgentID: "analyst", SessionTokenID: "l-analyst",
		ArgsJSON: []byte(`{"name":"polars","version":"1.9.0","rationale":"x","pip_args":"--pre"}`)}); err == nil {
		t.Fatal("unknown arguments are refused")
	}
	out, err := tool.Call(context.Background(), domain.KernelToolCall{AgentID: "analyst", ArgsJSON: args, SessionTokenID: "l-analyst"})
	if err != nil {
		t.Fatal(err)
	}
	var r RequestResult
	if err := json.Unmarshal(out, &r); err != nil || r.Status != RequestPending || r.ApprovalID == "" {
		t.Fatalf("the allowed call raises the card and returns pending: %s %v", out, err)
	}
	if items := h.pending(); len(items) != 1 || items[0].AgentID != "analyst" {
		t.Fatalf("the card is the authenticated caller's, not an argument's: %+v", items)
	}
}

func TestNewTool_RequiresEveryLookup(t *testing.T) {
	h, _ := runtimeHarness(t)
	if _, err := NewTool(h.svc, time.Second, ToolDeps{}); err == nil {
		t.Fatal("missing lookups must refuse construction (fail closed)")
	}
	if _, err := NewTool(nil, time.Second, newToolFixture().deps()); err == nil {
		t.Fatal("no service must refuse construction")
	}
}
