package migrate

import (
	"strings"
	"testing"
)

// ADR-0139 D13: 0029 adds the skill-lifecycle tables. Additive only (no DROP of
// anything that existed before it), and the ledger is append-only in the database.
func TestLoadMigrations_Includes0029SkillLifecycle(t *testing.T) {
	migs, err := loadMigrations(1536)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 29 {
			continue
		}
		if m.name != "skill_lifecycle" {
			t.Fatalf("migration 29 is %q", m.name)
		}
		for _, s := range []string{
			"CREATE TABLE IF NOT EXISTS skill_proposals",
			"CREATE TABLE IF NOT EXISTS skill_versions",
			"CREATE TABLE IF NOT EXISTS skill_records",
			"CREATE TABLE IF NOT EXISTS skill_ledger",
			"CREATE TABLE IF NOT EXISTS skill_run_uses",
			"UNIQUE (skill_name, seq)",
			"BEFORE UPDATE OR DELETE ON skill_ledger",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0029 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if strings.HasPrefix(l, "--") {
				continue
			}
			// The only DROP is the idempotent re-create of 0029's own trigger.
			if strings.Contains(l, "DROP ") && !strings.Contains(l, "DROP TRIGGER IF EXISTS TRG_SKILL_LEDGER_APPEND_ONLY") {
				t.Errorf("0029 must be additive: %q", line)
			}
		}
		return
	}
	t.Fatal("0029_skill_lifecycle is not embedded")
}
