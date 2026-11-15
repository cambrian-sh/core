package migrate

import (
	"strings"
	"testing"
)

// 0052_tool_grants.sql (ENG-1116): additive, one table keyed by the natural key
// (agent_id, tool), not destructive.
func TestLoadMigrations_Includes0052ToolGrants(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 52 {
			continue
		}
		if m.name != "tool_grants" {
			t.Fatalf("migration 52 is %q", m.name)
		}
		if IsDestructive(m.sql) {
			t.Error("0052 only creates a table: it must not carry the destructive marker")
		}
		for _, s := range []string{"CREATE TABLE IF NOT EXISTS tool_grants", "PRIMARY KEY (agent_id, tool)", "policy      JSONB"} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0052 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if !strings.HasPrefix(l, "--") && (strings.Contains(l, "DROP ") || strings.Contains(l, "DELETE ")) {
				t.Errorf("0052 must be additive: %q", line)
			}
		}
		return
	}
	t.Fatal("0052_tool_grants is not embedded")
}
