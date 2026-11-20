package migrate

import (
	"strings"
	"testing"
)

// 0067_python_package_removed_state.sql (ADR-0157 D7.10.3, ENG-2012): widens the
// approvals' state CHECK with 'removed'. Additive: no row is rewritten.
func TestLoadMigrations_Includes0067PythonPackageRemovedState(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 67 {
			continue
		}
		if m.name != "python_package_removed_state" {
			t.Fatalf("migration 67 is %q", m.name)
		}
		if IsDestructive(m.sql) || !strings.Contains(m.sql, NotDestructiveMarker) {
			t.Error("0067 widens a constraint: not-destructive marker, no destructive marker")
		}
		if !strings.Contains(m.sql, "'removed'") || !strings.Contains(m.sql, "python_package_approvals_state_check") {
			t.Error("0067 must add 'removed' to python_package_approvals_state_check")
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if strings.HasPrefix(l, "--") {
				continue
			}
			if strings.HasPrefix(l, "DELETE ") || strings.HasPrefix(l, "UPDATE ") || strings.Contains(l, "DROP TABLE") || strings.Contains(l, "DROP COLUMN") {
				t.Errorf("0067 must not delete or rewrite rows: %q", line)
			}
		}
		return
	}
	t.Fatal("0067_python_package_removed_state is not embedded")
}
