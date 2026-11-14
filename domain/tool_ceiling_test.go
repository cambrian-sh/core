package domain

import (
	"context"
	"slices"
	"testing"
)

func TestMatchToolGlob(t *testing.T) {
	for _, c := range []struct {
		p, n string
		want bool
	}{
		{"mcp:filesystem/read_*", "mcp:filesystem/read_text_file", true},
		{"mcp:filesystem/read_*", "mcp:filesystem/write_file", false},
		{"mcp:fetch/*", "mcp:fetch/a/b", true}, // fnmatch: * crosses /
		{"mcp:time/get_?", "mcp:time/get_x", true},
		{"mcp:time/get_?", "mcp:time/get_xy", false},
		{"a*b*c", "abxbxc", true},
		{"a*b*c", "abxbx", false},
		{`mcp:x/a\b`, `mcp:x/a\b`, true}, // \ is literal
		{"web_*", "Web_search", false},   // case-sensitive
	} {
		if got := MatchToolGlob(c.p, c.n); got != c.want {
			t.Errorf("MatchToolGlob(%q, %q) = %v, want %v", c.p, c.n, got, c.want)
		}
	}
	if (ToolAllowlist{"mcp:fs/*"}).Allows("") {
		t.Error("an empty tool name must never be allowed")
	}
}

// profileExecutor is an executor with a profile agent "assistant" whose allowlist
// is mcp:fs/read_*; its grants are the allowlist's expansion (what the composition
// root's profileGrantSync writes).
func profileExecutor(unrestricted bool) (*ToolExecutor, *fakeHandler) {
	reg := NewInMemoryToolRegistry()
	for _, n := range []string{"mcp:fs/read_file", "mcp:fs/write_file", "mcp:shell/run"} {
		reg.Register(SystemTool{Name: n, Schema: []byte(`{"type":"object"}`)})
	}
	allow := ToolAllowlist{"mcp:fs/read_*"}
	grants := NewInMemoryGrantsStore()
	var gs []ToolGrant
	for _, n := range allow.Expand(reg.All()) {
		gs = append(gs, ToolGrant{Tool: n})
	}
	grants.Set("assistant", gs)
	grants.Set("other", []ToolGrant{{Tool: "mcp:shell/run"}})
	ceiling := NewProfileToolCeiling()
	ceiling.Set("assistant", allow)
	h := &fakeHandler{result: []byte(`{"ok":true}`)}
	e := newExec(reg, grants, nil)
	e.MCPHandler = h
	e.Unrestricted = unrestricted
	e.Ceiling = ceiling
	e.Overlay = NewRunGrantOverlay()
	return e, h
}

// The profile allowlist is enforced kernel-side in both grant modes: in the normal
// mode through the expanded grants, and under tools_unrestricted through the ceiling.
func TestProfileCeiling_EnforcedInBothModes(t *testing.T) {
	for _, unrestricted := range []bool{false, true} {
		e, h := profileExecutor(unrestricted)
		ctx := context.Background()
		call := func(agent, tool string) ToolCallResponse {
			return e.Execute(ctx, ToolCallRequest{AgentID: agent, ToolName: tool, ArgsJSON: []byte(`{}`)})
		}
		if r := call("assistant", "mcp:fs/read_file"); r.Denied {
			t.Errorf("unrestricted=%v: allowlisted tool denied: %s", unrestricted, r.DenyReason)
		}
		h.called = false
		if r := call("assistant", "mcp:shell/run"); !r.Denied || h.called {
			t.Errorf("unrestricted=%v: a tool outside the allowlist ran (denied=%v called=%v)", unrestricted, r.Denied, h.called)
		}
		if r := call("assistant", "mcp:fs/write_file"); !r.Denied {
			t.Errorf("unrestricted=%v: write_file is outside mcp:fs/read_* but was allowed", unrestricted)
		}
		// A principal no ceiling binds is unaffected.
		if r := call("other", "mcp:shell/run"); r.Denied {
			t.Errorf("unrestricted=%v: an unbound principal was denied its grant: %s", unrestricted, r.DenyReason)
		}
		menu := names(e.AvailableTools(ctx, "assistant"))
		if !slices.Equal(menu, []string{"mcp:fs/read_file"}) {
			t.Errorf("unrestricted=%v: menu = %v, want only the allowlisted tool", unrestricted, menu)
		}
	}
}

// A skill cannot widen a profile: its run-scoped grant is clipped by the ceiling,
// both when conferred and when called.
func TestProfileCeiling_OutranksSkillOverlay(t *testing.T) {
	e, _ := profileExecutor(false)
	ctx := context.Background()
	granted, clipped := e.ConferSkillGrants(ctx, "lease-1", "assistant", []string{"mcp:fs/read_file", "mcp:shell/run"})
	if !slices.Equal(granted, []string{"mcp:fs/read_file"}) || len(clipped) != 1 {
		t.Fatalf("granted=%v clipped=%v", granted, clipped)
	}
	e.Overlay.Activate("lease-1", []string{"mcp:shell/run"}) // even if it got onto the overlay
	r := e.Execute(ctx, ToolCallRequest{AgentID: "assistant", ToolName: "mcp:shell/run", SessionTokenID: "lease-1", ArgsJSON: []byte(`{}`)})
	if !r.Denied {
		t.Fatal("an overlay grant outside the profile ceiling was honoured")
	}
	if got := e.AvailableToolsNamedFor(ctx, "assistant", "lease-1", []string{"mcp:shell/run"}); len(got) != 0 {
		t.Fatalf("describe served a tool outside the ceiling: %v", names(got))
	}
}

func TestToolAllowlist_ExpandAndUnmatched(t *testing.T) {
	tools := []SystemTool{{Name: "mcp:fs/read_file"}, {Name: "mcp:fs/list_dir"}, {Name: "mcp:time/now"}}
	a := ToolAllowlist{"mcp:fs/read_*", "mcp:fs/list_*", "mcp:pdf/*"}
	if got := a.Expand(tools); !slices.Equal(got, []string{"mcp:fs/list_dir", "mcp:fs/read_file"}) {
		t.Errorf("Expand = %v", got)
	}
	if got := a.Unmatched(tools); !slices.Equal(got, []string{"mcp:pdf/*"}) {
		t.Errorf("Unmatched = %v", got)
	}
}

// ENG-632 (owner decision 2026-11-15): a profile's local: pattern lets its ceiling
// admit contributed tools on the named machines only; the grant is separate.
func TestProfileToolCeiling_AdmitsContributedToolsItNames(t *testing.T) {
	c := NewProfileToolCeiling()
	c.Set("analyst", ToolAllowlist{"mcp:filesystem/read_*", "local:*/read_*"})
	for tool, want := range map[string]bool{
		"local:laptop/read_file":  true,
		"local:laptop/write_file": false,
		"local:desk/read_notes":   true,
		"mcp:filesystem/read_x":   true,
	} {
		if inside, bound := c.Allows("analyst", tool); !bound || inside != want {
			t.Errorf("%s: inside=%v bound=%v, want inside=%v", tool, inside, bound, want)
		}
	}
	if (ToolAllowlist{"local:*/read_*"}).Expand([]SystemTool{{Name: "mcp:filesystem/read_x"}}) != nil {
		t.Error("a local: pattern must expand to no registry grant")
	}
}
