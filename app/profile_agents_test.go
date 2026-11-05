package app

import (
	"context"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func grantNames(gs []domain.ToolGrant) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Tool)
	}
	sort.Strings(out)
	return out
}

// Grants equal the allowlist's expansion over the live registry, and follow the
// registry when an MCP server's tools change (the sink drives the re-sync).
func TestProfileGrantSync_FollowsTheRegistry(t *testing.T) {
	ctx := context.Background()
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "mcp:fs/read_file"})
	reg.Register(domain.SystemTool{Name: "mcp:shell/run"})
	grants := domain.NewInMemoryGrantsStore()
	sync := newProfileGrantSync(grants, reg.All, []profileAgent{
		{ID: "assistant", Allow: domain.ToolAllowlist{"mcp:fs/read_*", "mcp:fetch/*"}},
	})
	sync.Sync(ctx, true)
	got, _ := grants.GrantsFor(ctx, "assistant")
	if names := grantNames(got); !slices.Equal(names, []string{"mcp:fs/read_file"}) {
		t.Fatalf("boot grants = %v", names)
	}

	// An operator grant survives re-syncs (the ceiling still bounds it).
	grants.Set("assistant", append(got, domain.ToolGrant{Tool: "mcp:time/now"}))

	sink := newMCPToolSink(reg, &domain.ToolIndexer{Store: &sinkFakeStore{}, Embedder: sinkFakeEmb{}})
	sink.onChange = func(ctx context.Context) { sync.Sync(ctx, false) }
	sink.SetServerTools(ctx, "fetch", []domain.SystemTool{{Name: "mcp:fetch/fetch"}})
	got, _ = grants.GrantsFor(ctx, "assistant")
	if names := grantNames(got); !slices.Equal(names, []string{"mcp:fetch/fetch", "mcp:fs/read_file", "mcp:time/now"}) {
		t.Fatalf("after the fetch server connected: grants = %v", names)
	}
	sink.RemoveServerTools(ctx, "fetch")
	got, _ = grants.GrantsFor(ctx, "assistant")
	if names := grantNames(got); !slices.Equal(names, []string{"mcp:fs/read_file", "mcp:time/now"}) {
		t.Fatalf("after the fetch server dropped: grants = %v", names)
	}
}

// End to end at the executor: grants from the sync + the ceiling, in both grant modes.
func TestProfileAgent_AllowlistEnforcedKernelSide(t *testing.T) {
	ctx := context.Background()
	for _, unrestricted := range []bool{false, true} {
		reg := domain.NewInMemoryToolRegistry()
		reg.Register(domain.SystemTool{Name: "mcp:fs/read_file"})
		reg.Register(domain.SystemTool{Name: "mcp:shell/run"})
		grants := domain.NewInMemoryGrantsStore()
		profiles := []profileAgent{{ID: "assistant", Allow: domain.ToolAllowlist{"mcp:fs/read_*"}}}
		newProfileGrantSync(grants, reg.All, profiles).Sync(ctx, true)
		e := &domain.ToolExecutor{Registry: reg, Grants: grants, Unrestricted: unrestricted,
			Ceiling: profileToolCeiling(profiles), MCPHandler: okHandler{}, InlineThreshold: 1 << 20}
		if r := e.Execute(ctx, domain.ToolCallRequest{AgentID: "assistant", ToolName: "mcp:shell/run", ArgsJSON: []byte(`{}`)}); !r.Denied {
			t.Errorf("unrestricted=%v: tool outside the allowlist was not denied", unrestricted)
		}
		if r := e.Execute(ctx, domain.ToolCallRequest{AgentID: "assistant", ToolName: "mcp:fs/read_file", ArgsJSON: []byte(`{}`)}); r.Denied {
			t.Errorf("unrestricted=%v: allowlisted tool denied: %s", unrestricted, r.DenyReason)
		}
	}
}

type okHandler struct{}

func (okHandler) Execute(context.Context, domain.ToolCall) ([]byte, error) { return []byte(`{}`), nil }

func TestReconcileProfileAgents_PrunesUndiscoveredProfileRuntimeRecords(t *testing.T) {
	reg := &fakeModelReg{agents: []domain.AgentDefinition{
		{ID: "assistant", ExecPath: "worker_agent.py", Profile: "assistant"},
		{ID: "old-profile", ExecPath: "worker_agent.py", Profile: "old-profile"},
		{ID: "worker_agent", ExecPath: "worker_agent.py"},
		{ID: "chat_agent", ExecPath: "chat_agent.py"},
	}}
	registered := []DiscoveredAgent{{Definition: domain.AgentDefinition{ID: "assistant", Profile: "assistant"}}}
	reconcileProfileAgents(context.Background(), reg, registered)
	sort.Strings(reg.deleted)
	if !slices.Equal(reg.deleted, []string{"old-profile", "worker_agent"}) {
		t.Fatalf("deleted = %v", reg.deleted)
	}
}

func TestProfileModelPreference(t *testing.T) {
	roles := map[string]string{"planner": "big-gen"}
	fn := profileModelPreference([]profileAgent{
		{ID: "a", Model: "role:planner"}, {ID: "b", Model: "fast-gen"}, {ID: "c"}, {ID: "d", Model: "role:facts"},
	}, func() map[string]string { return roles })
	for id, want := range map[string]string{"a": "big-gen", "b": "fast-gen", "c": "", "d": "", "nobody": ""} {
		if got := fn(id); got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
	roles["planner"] = "other-gen" // a live role reassignment is picked up per call
	if got := fn("a"); got != "other-gen" {
		t.Errorf("role reassignment not live: %q", got)
	}
	if profileModelPreference([]profileAgent{{ID: "c"}}, nil) != nil {
		t.Error("no preferences ⇒ no resolver")
	}
}

// The shipped agents directory registers the three starter profiles, with their
// skills found in the shipped skills directory, and not worker_agent.py itself.
func TestBuiltinSource_RegistersShippedProfiles(t *testing.T) {
	agentsDir, _ := filepath.Abs(filepath.Join("..", "agents"))
	src := newBuiltinFilesystemAgentSource(agentsDir).withSkillTools(profileSkillIndex(filepath.Join("..", "skills")))
	got, err := src.DiscoverAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	profiles := collectProfileAgents(got)
	var ids []string
	for _, p := range profiles {
		ids = append(ids, p.ID)
		if len(p.Allow) == 0 || p.TrustTier == "" {
			t.Errorf("%s: allowlist/trust tier not carried: %+v", p.ID, p)
		}
	}
	if !slices.Equal(ids, []string{"assistant", "data-analyst", "researcher"}) {
		t.Fatalf("profile agents = %v", ids)
	}
	for _, da := range got {
		if da.Definition.ID == "worker_agent" {
			t.Fatal("worker_agent.py registered as an agent")
		}
	}
}
