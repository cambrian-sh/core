package migrate

import (
	"strings"
	"testing"
)

// 0057_python_package_approvals.sql (ADR-0157 D7.8.4, ENG-2009): durable per-agent Python
// package approvals and their append-only audit. Additive.
func TestLoadMigrations_Includes0057PythonPackageApprovals(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 57 {
			continue
		}
		if m.name != "python_package_approvals" {
			t.Fatalf("migration 57 is %q", m.name)
		}
		if IsDestructive(m.sql) {
			t.Error("0057 only creates tables: it must not carry the destructive marker")
		}
		if !strings.Contains(m.sql, NotDestructiveMarker) {
			t.Error("0057 must carry the not-destructive marker (every migration from 0051 on does)")
		}
		for _, want := range []string{
			"CREATE TABLE IF NOT EXISTS python_package_approvals",
			"UNIQUE (target, name, version, lock_sha256)",
			"CREATE TABLE IF NOT EXISTS python_package_events",
			"BEFORE UPDATE OR DELETE ON python_package_events",
		} {
			if !strings.Contains(m.sql, want) {
				t.Errorf("0057 must contain %q", want)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if strings.HasPrefix(l, "--") {
				continue
			}
			if strings.HasPrefix(l, "DELETE ") || strings.HasPrefix(l, "UPDATE ") || strings.Contains(l, "DROP TABLE") || strings.Contains(l, "DROP COLUMN") {
				t.Errorf("0057 must not delete or rewrite rows: %q", line)
			}
		}
		return
	}
	t.Fatal("0057_python_package_approvals is not embedded")
}
