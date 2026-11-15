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
		e := newTestToolExecutor(t, domain.ToolExecutorControls{Registry: reg, Grants: grants, Unrestricted: unrestricted,
			Ceiling: profileToolCeiling(profiles)}, domain.ToolExecutor{MCPHandler: okHandler{}, InlineThreshold: 1 << 20})
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

// decisions/skill-access-grants.md: profile agents are bound to their profile's
// skills (the base of the effective set); other agents are unbound.
func TestProfileSkillSets_BindsOnlyProfileAgents(t *testing.T) {
	agents := collectProfileAgents([]DiscoveredAgent{
		{Definition: domain.AgentDefinition{ID: "analyst", Profile: "analyst"}, Manifest: &domain.AgentManifest{Skills: []string{"file-report"}}},
		{Definition: domain.AgentDefinition{ID: "legacy"}, Manifest: &domain.AgentManifest{Skills: []string{"x"}}},
	})
	sets := profileSkillSets(agents)
	if s, ok := sets("analyst"); !ok || len(s) != 1 || s[0] != "file-report" {
		t.Fatalf("analyst = %v %v", s, ok)
	}
	if _, ok := sets("legacy"); ok {
		t.Fatal("a non-profile agent must be unbound")
	}
}

// The shipped assistant can organise files: loaded from disk through the real
// discovery path, its grants over the curated filesystem server's tools
// (configs/mcp.json.example) are exactly its allowlist's expansion, which now
// includes create_directory and move_file and still excludes the tools it was not
// given (edit/append/copy). Before, no shipped profile held move_file, so a "file
// my receipts" step could only reach an agent that would fake the move
// (T-20261106-no-shipped-agent-profile-can-organise-files-or).
func TestShippedAssistant_GrantsEqualItsAllowlistAndCanMoveFiles(t *testing.T) {
	ctx := context.Background()
	agentsDir, _ := filepath.Abs(filepath.Join("..", "agents"))
	src := newBuiltinFilesystemAgentSource(agentsDir).withSkillTools(profileSkillIndex(filepath.Join("..", "skills")))
	got, err := src.DiscoverAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var assistant *profileAgent
	for _, p := range collectProfileAgents(got) {
		if p.ID == "assistant" {
			p := p
			assistant = &p
		}
	}
	if assistant == nil {
		t.Fatal("the shipped assistant profile was not discovered")
	}
	for _, da := range got {
		if da.Definition.ID == "assistant" {
			if m := da.Manifest; m == nil || !slices.Contains(m.Capabilities, "file_organisation") {
				t.Errorf("assistant manifest does not declare file_organisation: %+v", m)
			}
		}
	}

	reg := domain.NewInMemoryToolRegistry()
	for _, n := range []string{"read_text_file", "read_file", "read_media_file", "read_multiple_files",
		"list_directory", "list_directory_with_sizes", "directory_tree", "search_files", "get_file_info",
		"list_allowed_directories", "write_file", "edit_file", "append_file", "create_directory",
		"move_file", "copy_file"} {
		reg.Register(domain.SystemTool{Name: "mcp:filesystem/" + n})
	}
	reg.Register(domain.SystemTool{Name: "mcp:shell/shell_execute"})
	grants := domain.NewInMemoryGrantsStore()
	newProfileGrantSync(grants, reg.All, []profileAgent{*assistant}).Sync(ctx, true)
	gs, _ := grants.GrantsFor(ctx, "assistant")
	names := grantNames(gs)
	want := assistant.Allow.Expand(reg.All())
	sort.Strings(want)
	if !slices.Equal(names, want) {
		t.Fatalf("grants %v != allowlist expansion %v", names, want)
	}
	for _, must := range []string{"mcp:filesystem/move_file", "mcp:filesystem/create_directory", "mcp:filesystem/list_directory"} {
		if !slices.Contains(names, must) {
			t.Errorf("assistant lacks %s: %v", must, names)
		}
	}
	for _, never := range []string{"mcp:filesystem/edit_file", "mcp:filesystem/append_file", "mcp:filesystem/copy_file", "mcp:shell/shell_execute"} {
		if slices.Contains(names, never) {
			t.Errorf("assistant was granted %s, outside its allowlist", never)
		}
	}
}
