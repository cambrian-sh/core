package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/integrations"
)

// webReadTools are the tools the shipped manifests declare egress_read (ADR-0150
// D9, owner 2026-11-13: "web reads in subtasks — read-only web tools only: GET
// fetch, search"). "*" is a server's default policy (every tool of a pure search
// API). T-20261113-no-shipped-mcp-manifest-declares-egress-read-so.
var webReadTools = map[string]bool{
	"fetch/fetch":                true,
	"web-search/search":          true,
	"web-search/fetch_content":   true,
	"searxng/searxng_web_search": true,
	"searxng/web_url_read":       true,
	"brave-search/*":             true,
	"tavily/*":                   true,
	"exa/*":                      true,
}

// subtaskOK reports whether a tool registered under this policy is inside the
// default subtask effect ceiling (execution.lane.subtasks.allowed_effects), the
// way the kernel decides it: registration resolves the declared effects, then
// domain.EffectsBeyond applies the ceiling (inferred egress counts as send).
func subtaskOK(t *testing.T, name string, p mcp.ToolPolicy) (inside, egress bool) {
	t.Helper()
	st := domain.SystemTool{Name: name}
	for _, e := range p.Effects {
		st.Effects = append(st.Effects, domain.ToolEffect(e))
		if e == string(domain.EffectEgress) || e == string(domain.EffectEgressRead) || e == string(domain.EffectEgressSend) {
			egress = true
		}
	}
	st.Dangerous = p.Dangerous
	reg, err := domain.ValidateRegistration(st, false)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var allowed []domain.ToolEffect
	for _, e := range config.DefaultSubtaskAllowedEffects {
		allowed = append(allowed, domain.ToolEffect(e))
	}
	return len(domain.EffectsBeyond(reg, allowed)) == 0 && !reg.EffectsInferred, egress
}

// policies lists each declared (server/tool → policy), "*" for a server default.
func policies(id string, tools []config.MCPToolConfig, def *config.MCPToolConfig) map[string]mcp.ToolPolicy {
	out := map[string]mcp.ToolPolicy{}
	for _, tc := range tools {
		out[id+"/"+tc.Name] = mcpToolPolicyFromConfig(tc)
	}
	if def != nil {
		out[id+"/*"] = mcpToolPolicyFromConfig(*def)
	}
	return out
}

// Exactly the read-only web tools reach a subtask by their effects, in the
// reference configs (mcp.json.example, what the use-case suite renders its
// bundle from, and mcp.integrations.json.example, which operators copy — it
// kept plain egress on its web-search servers until ENG-1760) and in the
// built-in catalog (what a console install renders); every other egress tool
// (mail, chat, SaaS reads, file converters, shells) stays outside the ceiling.
func TestShippedManifests_OnlyReadOnlyWebToolsDeclareEgressRead(t *testing.T) {
	declared := map[string]mcp.ToolPolicy{}

	for _, ex := range []struct{ file, prefix string }{
		{"mcp.json.example", "example:"}, {"mcp.integrations.json.example", "integrations:"},
	} {
		raw, err := os.ReadFile(filepath.Join("..", "configs", ex.file))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			MCP struct {
				Servers []config.MCPServerConfig `json:"servers"`
			} `json:"mcp"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", ex.file, err)
		}
		for _, s := range doc.MCP.Servers {
			for k, v := range policies(s.ID, s.Tools, s.DefaultToolPolicy) {
				declared[ex.prefix+k] = v
			}
		}
	}
	cat, err := integrations.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cat.Entries() {
		for k, v := range policies(e.ID, e.Server.Tools, e.Server.DefaultToolPolicy) {
			declared["catalog:"+k] = v
		}
	}

	var gotWeb []string
	for key, p := range declared {
		inside, egress := subtaskOK(t, key, p)
		_, toolKey, _ := strings.Cut(key, ":")
		if egress && inside {
			gotWeb = append(gotWeb, key)
			if !webReadTools[toolKey] {
				t.Errorf("%s reaches a subtask with egress but is not a read-only web tool (effects %v)", key, p.Effects)
			}
		}
		if webReadTools[toolKey] && !inside {
			t.Errorf("%s is a read-only web tool but stays outside the subtask ceiling (effects %v)", key, p.Effects)
		}
	}
	sort.Strings(gotWeb)
	for _, must := range []string{"example:fetch/fetch", "example:web-search/search", "catalog:fetch/fetch",
		"integrations:searxng/searxng_web_search", "integrations:brave-search/*", "integrations:tavily/*", "integrations:exa/*"} {
		if i := sort.SearchStrings(gotWeb, must); i >= len(gotWeb) || gotWeb[i] != must {
			t.Errorf("%s missing from the subtask-reachable web tools %v", must, gotWeb)
		}
	}
}
