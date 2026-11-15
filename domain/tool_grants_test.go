package domain

import (
	"context"
	"errors"
	"slices"
	"sort"
	"testing"
)

func grantTools(gs []ToolGrant) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Tool)
	}
	sort.Strings(out)
	return out
}

// failingGrantStore is an operator layer whose reads and/or writes fail.
type failingGrantStore struct {
	*InMemoryGrantsStore
	readErr, writeErr error
}

func (f *failingGrantStore) GrantsFor(ctx context.Context, id string) ([]ToolGrant, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.InMemoryGrantsStore.GrantsFor(ctx, id)
}

func (f *failingGrantStore) AllGrants(ctx context.Context) (map[string][]ToolGrant, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.InMemoryGrantsStore.AllGrants(ctx)
}

func (f *failingGrantStore) ReplaceGrants(ctx context.Context, id string, gs []ToolGrant) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	return f.InMemoryGrantsStore.ReplaceGrants(ctx, id, gs)
}

func TestLayeredGrantsStore_RequiresOperatorLayer(t *testing.T) {
	if _, err := NewLayeredGrantsStore(nil); err == nil {
		t.Fatal("a nil operator layer was accepted")
	}
}

// The merged view: profile grants plus operator grants, operator policy wins.
func TestLayeredGrantsStore_MergesLayers(t *testing.T) {
	ctx := context.Background()
	op := NewInMemoryGrantsStore()
	s, err := NewLayeredGrantsStore(op)
	if err != nil {
		t.Fatal(err)
	}
	s.Profile().Set("a", []ToolGrant{{Tool: "fs/read"}, {Tool: "fs/list"}})
	pol := ToolResourcePolicy{Filesystem: FilesystemPolicy{AllowRoots: []string{"/data"}}}
	op.Set("a", []ToolGrant{{Tool: "fs/read", Policy: pol}, {Tool: "propose_knowledge"}})
	got, err := s.GrantsFor(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if names := grantTools(got); !slices.Equal(names, []string{"fs/list", "fs/read", "propose_knowledge"}) {
		t.Fatalf("merged = %v", names)
	}
	for _, g := range got {
		if g.Tool == "fs/read" && len(g.Policy.Filesystem.AllowRoots) != 1 {
			t.Fatalf("operator policy did not win: %+v", g)
		}
	}
	if none, _ := s.GrantsFor(ctx, ""); none != nil {
		t.Fatalf("empty agent got grants %v", none)
	}
	all, err := s.AllGrants(ctx)
	if err != nil || len(all["a"]) != 3 {
		t.Fatalf("AllGrants = %v, %v", all, err)
	}
}

// ReplaceGrants persists only what the profile does not already derive; a
// policy set on a profile tool is persisted; a revoked profile tool leaves the
// profile layer until the next sync.
func TestLayeredGrantsStore_ReplaceSplitsLayers(t *testing.T) {
	ctx := context.Background()
	op := NewInMemoryGrantsStore()
	s, _ := NewLayeredGrantsStore(op)
	s.Profile().Set("a", []ToolGrant{{Tool: "fs/read"}, {Tool: "fs/list"}})

	cur, _ := s.GrantsFor(ctx, "a")
	if err := s.ReplaceGrants(ctx, "a", append(cur, ToolGrant{Tool: "propose_knowledge"})); err != nil {
		t.Fatal(err)
	}
	if held, _ := op.GrantsFor(ctx, "a"); !slices.Equal(grantTools(held), []string{"propose_knowledge"}) {
		t.Fatalf("operator layer = %v, want only the operator grant", grantTools(held))
	}

	pol := ToolResourcePolicy{Network: NetworkPolicy{AllowDomains: []string{"example.com"}}}
	cur, _ = s.GrantsFor(ctx, "a")
	for i := range cur {
		if cur[i].Tool == "fs/list" {
			cur[i].Policy = pol
		}
	}
	if err := s.ReplaceGrants(ctx, "a", cur); err != nil {
		t.Fatal(err)
	}
	if held, _ := op.GrantsFor(ctx, "a"); !slices.Equal(grantTools(held), []string{"fs/list", "propose_knowledge"}) {
		t.Fatalf("operator layer = %v, want the policied profile tool persisted", grantTools(held))
	}

	// Revoke the profile tool fs/read: gone from the view (until a profile sync).
	cur, _ = s.GrantsFor(ctx, "a")
	next := cur[:0:0]
	for _, g := range cur {
		if g.Tool != "fs/read" {
			next = append(next, g)
		}
	}
	if err := s.ReplaceGrants(ctx, "a", next); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GrantsFor(ctx, "a")
	if names := grantTools(got); !slices.Equal(names, []string{"fs/list", "propose_knowledge"}) {
		t.Fatalf("after revoke = %v", names)
	}
}

// An operator-layer read error is returned, never an empty or profile-only set.
func TestLayeredGrantsStore_ReadErrorIsReturned(t *testing.T) {
	ctx := context.Background()
	op := &failingGrantStore{InMemoryGrantsStore: NewInMemoryGrantsStore(), readErr: errors.New("db down")}
	s, _ := NewLayeredGrantsStore(op)
	s.Profile().Set("a", []ToolGrant{{Tool: "fs/read"}})
	if got, err := s.GrantsFor(ctx, "a"); err == nil || got != nil {
		t.Fatalf("GrantsFor on a failing store = %v, %v; want nil + error", got, err)
	}
	if _, err := s.AllGrants(ctx); err == nil {
		t.Fatal("AllGrants on a failing store returned no error")
	}
	if err := s.ReplaceGrants(ctx, "a", nil); err == nil {
		t.Fatal("ReplaceGrants read a failing store without error")
	}
}

// A failed operator write changes neither layer.
func TestLayeredGrantsStore_WriteErrorChangesNothing(t *testing.T) {
	ctx := context.Background()
	op := &failingGrantStore{InMemoryGrantsStore: NewInMemoryGrantsStore(), writeErr: errors.New("db down")}
	s, _ := NewLayeredGrantsStore(op)
	s.Profile().Set("a", []ToolGrant{{Tool: "fs/read"}})
	if err := s.ReplaceGrants(ctx, "a", []ToolGrant{{Tool: "propose_knowledge"}}); err == nil {
		t.Fatal("ReplaceGrants hid a write error")
	}
	got, _ := s.GrantsFor(ctx, "a")
	if names := grantTools(got); !slices.Equal(names, []string{"fs/read"}) {
		t.Fatalf("after a failed write = %v, want unchanged", names)
	}
}

// The executor denies a call when the grants store errors (fail closed).
func TestLayeredGrantsStore_ExecutorDeniesOnStoreError(t *testing.T) {
	ctx := context.Background()
	op := &failingGrantStore{InMemoryGrantsStore: NewInMemoryGrantsStore()}
	op.Set("a", []ToolGrant{{Tool: "propose_knowledge"}})
	s, _ := NewLayeredGrantsStore(op)
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "propose_knowledge"})
	e, err := NewToolExecutor(ToolExecutorControls{Registry: reg, Grants: s, Authz: AllowAllAuthorizer{}},
		ToolExecutor{MCPHandler: okToolHandler{}, InlineThreshold: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	req := ToolCallRequest{AgentID: "a", ToolName: "propose_knowledge", ArgsJSON: []byte(`{}`)}
	if r := e.Execute(ctx, req); r.Denied {
		t.Fatalf("granted call denied: %s", r.DenyReason)
	}
	op.readErr = errors.New("db down")
	if r := e.Execute(ctx, req); !r.Denied {
		t.Fatal("call allowed while the grants store fails")
	}
}

type okToolHandler struct{}

func (okToolHandler) Execute(context.Context, ToolCall) ([]byte, error) { return []byte(`{}`), nil }
