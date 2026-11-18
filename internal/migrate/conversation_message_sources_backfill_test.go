package migrate

import (
	"strings"
	"testing"
)

// 0061_conversation_message_sources_backfill.sql (ENG-2155): best-effort backfill of
// the sources of agent replies stored before 0060. Not destructive: it fills only the
// empty refs 0060 defaulted and adds a report table.
func TestLoadMigrations_Includes0061ReplySourcesBackfill(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 61 {
			continue
		}
		if m.name != "conversation_message_sources_backfill" {
			t.Fatalf("migration 61 is %q", m.name)
		}
		if IsDestructive(m.sql) {
			t.Error("0061 deletes nothing: it must not carry the destructive marker")
		}
		if !strings.Contains(m.sql, NotDestructiveMarker) {
			t.Error("0061 must carry the not-destructive marker")
		}
		if !strings.Contains(m.sql, "m.source_refs = '[]'::jsonb") {
			t.Error("0061 must fill only replies whose refs are still the empty default")
		}
		return
	}
	t.Fatal("0061_conversation_message_sources_backfill is not embedded")
}

// On a database holding pre-0060 replies, 0061 writes each reply the union of its
// turn's retained ledger refs, reports every reply it considered (backfilled /
// no_sources / unresolved), leaves user messages, redacted replies and replies stored
// after 0060 alone, and is idempotent.
func TestReplySourcesBackfill0061_FillsFromRetainedLedger_Pg(t *testing.T) {
	ctx, pool, _ := guardPool(t)
	applyThrough(ctx, t, pool, 60)

	for _, q := range []string{
		`INSERT INTO conversations (id, owner_id, status, profile, next_seq, created_at, updated_at)
		 VALUES ('c1', 'u1', 'open', 'employee', 10, now() - interval '2 days', now())`,
		// Pre-0060 rows (created a day before 0060 was applied).
		`INSERT INTO conversation_messages (id, conversation_id, seq, role, content, created_at) VALUES
		   ('u-1', 'c1', 1, 'user',  'agreement number?',      now() - interval '1 day'),
		   ('a-1', 'c1', 2, 'agent', 'It is HERON-7.',         now() - interval '1 day'),
		   ('a-1b','c1', 3, 'agent', 'Also: HERON-7 again.',   now() - interval '1 day'),
		   ('u-2', 'c1', 4, 'user',  'hi',                     now() - interval '1 day'),
		   ('a-2', 'c1', 5, 'agent', 'Hello.',                 now() - interval '1 day'),
		   ('u-3', 'c1', 6, 'user',  'old question',           now() - interval '1 day'),
		   ('a-3', 'c1', 7, 'agent', 'old answer, ledger gone', now() - interval '1 day'),
		   ('a-0', 'c1', 0, 'agent', 'Welcome (no turn).',     now() - interval '1 day')`,
		// A reply an erase already redacted stays as it is.
		`INSERT INTO conversation_messages (id, conversation_id, seq, role, content, created_at, redacted_at) VALUES
		   ('a-r', 'c1', 8, 'agent', '[removed: forgotten source]', now() - interval '1 day', now())`,
		// A reply stored after 0060 with no sources was right to have none.
		`INSERT INTO conversation_messages (id, conversation_id, seq, role, content, created_at) VALUES
		   ('a-new', 'c1', 9, 'agent', 'fresh', now() + interval '1 minute')`,
		// Turn u-1 recalled the note (two entries, overlapping refs, one empty id);
		// turn u-2 has an entry naming no record; turn u-3's entries were swept.
		`INSERT INTO ledger_entries (id, kind, conversation_id, turn_id, created_at, expires_at, source_refs) VALUES
		   ('led_1', 'tool_result', 'c1', 'u-1', now(), now() + interval '1 day', '["doc-1","chunk-1"]'),
		   ('led_2', 'step_output', 'c1', 'u-1', now(), now() - interval '1 hour', '["chunk-1","exp-9",""]'),
		   ('led_3', 'tool_result', 'c1', 'u-2', now(), now() + interval '1 day', '[]'),
		   ('led_4', 'tool_result', 'c2', 'u-3', now(), now() + interval '1 day', '["doc-other"]')`,
		`UPDATE schema_migrations SET applied_at = now() - interval '1 hour' WHERE version = 60`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	applyThrough(ctx, t, pool, 61)

	refs := func(id string) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT source_refs::text FROM conversation_messages WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for id, want := range map[string]string{
		"a-1":   `["chunk-1", "doc-1", "exp-9"]`,
		"a-1b":  `["chunk-1", "doc-1", "exp-9"]`, // a second message of the same turn
		"a-2":   `[]`,
		"a-3":   `[]`,
		"a-0":   `[]`,
		"a-r":   `[]`,
		"a-new": `[]`,
		"u-1":   `[]`,
	} {
		if got := refs(id); got != want {
			t.Errorf("%s source_refs = %s, want %s", id, got, want)
		}
	}

	report := map[string]string{}
	rows, err := pool.Query(ctx, `SELECT message_id, outcome || ':' || refs || ':' || turn_id FROM conversation_message_source_backfill`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		report[id] = v
	}
	rows.Close()
	want := map[string]string{
		"a-1":  "backfilled:3:u-1",
		"a-1b": "backfilled:3:u-1",
		"a-2":  "no_sources:0:u-2",
		"a-3":  "unresolved:0:u-3",
		"a-0":  "unresolved:0:",
	}
	if len(report) != len(want) {
		t.Errorf("report = %v, want %v", report, want)
	}
	for id, w := range want {
		if report[id] != w {
			t.Errorf("report[%s] = %q, want %q", id, report[id], w)
		}
	}

	// Idempotent: running the backfill again changes nothing and reports nothing new.
	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs {
		if m.version == 61 {
			if _, err := pool.Exec(ctx, m.sql); err != nil {
				t.Fatalf("re-run 0061: %v", err)
			}
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message_source_backfill`).Scan(&n); err != nil || n != len(want) {
		t.Errorf("report rows after a re-run = %d (%v), want %d", n, err, len(want))
	}
	if got := refs("a-1"); got != `["chunk-1", "doc-1", "exp-9"]` {
		t.Errorf("a-1 after a re-run = %s", got)
	}

	// The report goes with its conversation.
	if _, err := pool.Exec(ctx, `DELETE FROM conversations WHERE id = 'c1'`); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message_source_backfill`).Scan(&n); err != nil || n != 0 {
		t.Errorf("report rows after the conversation is deleted = %d (%v)", n, err)
	}
}
