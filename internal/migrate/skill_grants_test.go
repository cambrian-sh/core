package migrate

import (
	"strings"
	"testing"
)

// 0031 adds the per-agent skill access grant tables (decisions/skill-access-grants.md).
// Additive only, and the audit is append-only in the database.
func TestLoadMigrations_Includes0031SkillGrants(t *testing.T) {
	migs, err := loadMigrations(1536)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 31 {
			continue
		}
		if m.name != "skill_grants" {
			t.Fatalf("migration 31 is %q", m.name)
		}
		for _, s := range []string{
			"CREATE TABLE IF NOT EXISTS skill_grants",
			"CREATE TABLE IF NOT EXISTS skill_access_requests",
			"CREATE TABLE IF NOT EXISTS skill_grant_events",
			"UNIQUE (agent_id, skill_name)",
			"BEFORE UPDATE OR DELETE ON skill_grant_events",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0031 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if strings.HasPrefix(l, "--") {
				continue
			}
			if strings.Contains(l, "DROP ") && !strings.Contains(l, "DROP TRIGGER IF EXISTS TRG_SKILL_GRANT_EVENTS_APPEND_ONLY") {
				t.Errorf("0031 must be additive: %q", line)
			}
		}
		return
	}
	t.Fatal("0031_skill_grants is not embedded")
}
