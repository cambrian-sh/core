package migrate

import (
	"strings"
	"testing"
)

// 0066_approval_notice_action_history.sql (ENG-784): approvals.duplicate_notice with
// a constant default, and the action_history table. Not destructive.
func TestLoadMigrations_Includes0066ApprovalNoticeActionHistory(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 66 {
			continue
		}
		if m.name != "approval_notice_action_history" {
			t.Fatalf("migration 66 is %q", m.name)
		}
		if IsDestructive(m.sql) || !strings.Contains(m.sql, NotDestructiveMarker) {
			t.Error("0066 must carry the not-destructive marker and no destructive one")
		}
		for _, want := range []string{
			"ALTER TABLE approvals ADD COLUMN IF NOT EXISTS duplicate_notice TEXT NOT NULL DEFAULT '';",
			"CREATE TABLE IF NOT EXISTS action_history (",
			"CREATE INDEX IF NOT EXISTS idx_action_history_done_at ON action_history (done_at);",
		} {
			if !strings.Contains(m.sql, want) {
				t.Errorf("0066 lacks %q", want)
			}
		}
		return
	}
	t.Fatal("0066_approval_notice_action_history is not embedded")
}
