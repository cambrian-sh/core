package migrate

import "testing"

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
