package migrate

import (
	"strings"
	"testing"
)

// ADR-0084 D1 ships migration 0002_conversations.sql. This asserts the embedded loader
// actually discovers it — a filename the parser rejects would otherwise be silently absent
// and the conversations schema would never be applied.
func TestLoadMigrations_Includes0002Conversations(t *testing.T) {
	ms, err := loadMigrations(1024)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	var found bool
	var maxV int64
	for _, m := range ms {
		if m.version > maxV {
			maxV = m.version
		}
		if m.version == 2 {
			found = true
			if m.sql == "" {
				t.Error("migration 0002 loaded with empty SQL")
			}
		}
	}
	if !found {
		t.Fatalf("migration 0002 not discovered; loaded versions up to %d (%d migrations)", maxV, len(ms))
	}
}

// ADR-0084 D9 adds migration 0003 (the conversation policy column).
func TestLoadMigrations_Includes0003Policy(t *testing.T) {
	ms, err := loadMigrations(1024)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range ms {
		if m.version == 3 && m.sql != "" {
			return
		}
	}
	t.Fatal("migration 0003 not discovered")
}

// Session Phase 4 ships migration 0004 (sessions, runs, run_checkpoints). A filename the
// parser rejects would be silently absent, and the session tables would never be created —
// the store would then fall back to bbolt with no retention, quietly.
func TestLoadMigrations_Includes0004SessionsRuns(t *testing.T) {
	ms, err := loadMigrations(1024)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range ms {
		if m.version == 4 {
			if m.sql == "" {
				t.Fatal("migration 0004 loaded with empty SQL")
			}
			for _, want := range []string{"sessions", "runs", "run_checkpoints", "ON DELETE CASCADE"} {
				if !strings.Contains(m.sql, want) {
					t.Errorf("migration 0004 is missing %q — the cascade is what bounds growth", want)
				}
			}
			return
		}
	}
	t.Fatal("migration 0004 not discovered")
}

// Session Phase 5 ships migration 0005 (the conversation link, ADR-0084 D2).
func TestLoadMigrations_Includes0005ConversationLink(t *testing.T) {
	ms, err := loadMigrations(1024)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range ms {
		if m.version == 5 {
			for _, want := range []string{"conversation_id", "origin_message_id"} {
				if !strings.Contains(m.sql, want) {
					t.Errorf("migration 0005 is missing %q", want)
				}
			}
			return
		}
	}
	t.Fatal("migration 0005 not discovered")
}
