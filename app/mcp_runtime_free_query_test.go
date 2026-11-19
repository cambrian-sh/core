package app

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/integrations"
)

// ENG-2232: the catalog's free_query flags reach a server installed from that
// entry whatever its own tool list says; a server with no catalog id keeps its
// own policy.
func TestMCPServerFromConfig_StampsCatalogFreeQuery(t *testing.T) {
	cat, err := integrations.Load("")
	if err != nil {
		t.Fatal(err)
	}
	sb := sandboxPolicyFromConfig(config.DefaultConfig().MCP.Sandbox, cat, nil)

	// Legacy postgres install: execute_sql listed without the flag,
	// get_query_plan not listed at all, a read-only server default.
	legacy := config.MCPServerConfig{ID: "pg", Transport: "stdio", Endpoint: "npx", CatalogID: "postgres",
		Tools:             []config.MCPToolConfig{{Name: "execute_sql", Effects: []string{"read"}}, {Name: "list_tables", Effects: []string{"read"}}},
		DefaultToolPolicy: &config.MCPToolConfig{Effects: []string{"read"}}}
	sc := mcpServerFromConfig(legacy, sb)
	for _, name := range []string{"execute_sql", "get_query_plan"} {
		if p := sc.Tools[name]; !p.FreeQuery || !p.Dangerous {
			t.Errorf("%s: want free_query + dangerous from the catalog, got %+v", name, p)
		}
	}
	if p := sc.Tools["list_tables"]; p.FreeQuery {
		t.Error("list_tables must stay typed")
	}

	// A hand-written server with no catalog id: its own policy only.
	own := config.MCPServerConfig{ID: "mine", Transport: "stdio", Endpoint: "db",
		Tools: []config.MCPToolConfig{{Name: "execute_sql", Dangerous: true}, {Name: "q", Dangerous: true, FreeQuery: true}}}
	sc = mcpServerFromConfig(own, sb)
	if sc.Tools["execute_sql"].FreeQuery || !sc.Tools["q"].FreeQuery {
		t.Errorf("custom server policy changed: %+v", sc.Tools)
	}
}
