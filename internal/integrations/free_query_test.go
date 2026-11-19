package integrations

import (
	"slices"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-2232: the catalog flags the database servers' free-SQL tools, and a
// free-query tool must be dangerous.
func TestBuiltinCatalogFlagsFreeSQLTools(t *testing.T) {
	cat, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	flagged := map[string][]string{}
	for _, e := range cat.Entries() {
		for _, tool := range e.Server.Tools {
			if tool.FreeQuery {
				flagged[e.ID] = append(flagged[e.ID], tool.Name)
				if !tool.Dangerous {
					t.Errorf("%s/%s: free_query without dangerous", e.ID, tool.Name)
				}
			}
		}
	}
	if !slices.Equal(flagged["postgres"], []string{"execute_sql", "get_query_plan"}) {
		t.Errorf("postgres free-query tools = %v", flagged["postgres"])
	}
	if !slices.Equal(flagged["sqlite"], []string{"execute_sql"}) {
		t.Errorf("sqlite free-query tools = %v", flagged["sqlite"])
	}
	// The typed catalog tools stay unflagged.
	pg, _ := cat.Get("postgres")
	for _, tool := range pg.Server.Tools {
		if tool.Name == "list_tables" && tool.FreeQuery {
			t.Error("list_tables is typed and must not be flagged")
		}
	}
}

func TestValidateRefusesFreeQueryThatIsNotDangerous(t *testing.T) {
	e := Entry{ID: "db", Name: "DB", Category: "database", Version: "1", TrustTier: TrustCommunity,
		Server: config.MCPServerConfig{Transport: "stdio", Endpoint: "db-mcp",
			Tools: []config.MCPToolConfig{{Name: "run_sql", FreeQuery: true, Effects: []string{"read"}}}}}
	errs := strings.Join(Validate(e), "; ")
	if !strings.Contains(errs, `tool "run_sql": free_query requires dangerous: true`) {
		t.Fatalf("errors = %q", errs)
	}
	e.Server.Tools[0].Dangerous = true
	if errs := strings.Join(Validate(e), "; "); strings.Contains(errs, "free_query") {
		t.Fatalf("a dangerous free-query tool is valid: %q", errs)
	}
}
