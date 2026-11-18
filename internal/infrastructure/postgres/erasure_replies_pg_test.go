package postgres

import (
	"reflect"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2109 (owner decision 2026-11-18, option B) against real Postgres: erasing a
// document redacts, in the same transaction, exactly the agent replies whose recorded
// sources name the document or one of its chunks — text replaced with
// domain.RedactedReplyText, row, role, seq and created_at kept — and nothing else: a
// reply citing another document, a reply with no sources, the person's own message
// and an interruption marker stay as they were. The receipt counts and lists the
// redacted replies (also when read back), each has a content-free tombstone, and a
// replay redacts nothing more.
func TestPg_Erase_RedactsRepliesThatCitedTheDocument(t *testing.T) {
	a, ks, er, ctx := newErasureStore(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-red", "agent:a", erasureSecret, "dg-red", 2)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-keep", "agent:b", "keep text", "dg-keep", 1)

	conv, err := NewPgConversationStore(ctx, a.pool)
	if err != nil {
		t.Fatal(err)
	}
	clean := func() { _, _ = a.pool.Exec(ctx, `DELETE FROM conversations WHERE id LIKE 'lctest-%'`) }
	clean()
	t.Cleanup(clean)
	if err := conv.CreateConversation(ctx, domain.Conversation{ID: "lctest-conv", OwnerID: "lctest-emp",
		Status: domain.ConversationOpen, Profile: domain.ProfileEmployee}); err != nil {
		t.Fatal(err)
	}
	add := func(id string, role domain.MessageRole, text string, refs ...string) domain.Message {
		t.Helper()
		m, err := conv.AppendMessage(ctx, domain.Message{ID: id, ConversationID: "lctest-conv", Role: role, Content: text, SourceRefs: refs})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	add("lctest-m1", domain.MessageRoleUser, "What is the agreement number? "+erasureSecret)
	byChunk := add("lctest-m2", domain.MessageRoleAgent, "It is "+erasureSecret+".", "lctest-red-chunk-1", "lctest-red")
	add("lctest-m3", domain.MessageRoleUser, "And the other one?")
	add("lctest-m4", domain.MessageRoleAgent, "That one says keep text.", "lctest-keep", "lctest-keep-chunk-1")
	add("lctest-m5", domain.MessageRoleAgent, "Hello, no memory here.")
	byDocOnly := add("lctest-m6", domain.MessageRoleAgent, "Again: "+erasureSecret, "lctest-red")
	add("lctest-m7", domain.MessageRoleInterruption, "The turn did not finish.")
	// An interruption row that somehow carries refs is still never redacted (role guard).
	execSQL(t, a, ctx, `UPDATE conversation_messages SET source_refs = '["lctest-red"]' WHERE id = 'lctest-m7'`)

	rec, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-red"}}))
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	want := []string{"lctest-m2", "lctest-m6"}
	if !reflect.DeepEqual(rec.RedactedMessages, want) {
		t.Fatalf("receipt RedactedMessages = %v, want %v", rec.RedactedMessages, want)
	}
	if rec.Counts[eraseConversationMessages] != 2 {
		t.Fatalf("counts = %v, want conversation_messages 2", rec.Counts)
	}

	msgs, err := conv.ListMessages(ctx, "lctest-conv", 0, 0)
	if err != nil || len(msgs) != 7 {
		t.Fatalf("transcript = %d rows, %v; want all 7 kept", len(msgs), err)
	}
	byID := map[string]domain.Message{}
	for _, m := range msgs {
		byID[m.ID] = m
	}
	for _, red := range []domain.Message{byChunk, byDocOnly} {
		got := byID[red.ID]
		if got.Content != domain.RedactedReplyText || got.RedactedAt == nil {
			t.Errorf("%s = %q (redacted_at %v), want the redaction marker", red.ID, got.Content, got.RedactedAt)
		}
		if got.Role != domain.MessageRoleAgent || got.Seq != red.Seq || !got.CreatedAt.Equal(red.CreatedAt.Truncate(time.Microsecond)) {
			t.Errorf("%s lost its place: %+v vs %+v", red.ID, got, red)
		}
	}
	for id, text := range map[string]string{
		"lctest-m1": "What is the agreement number? " + erasureSecret, // the person's own words
		"lctest-m4": "That one says keep text.",
		"lctest-m5": "Hello, no memory here.",
		"lctest-m7": "The turn did not finish.",
	} {
		if got := byID[id]; got.Content != text || got.RedactedAt != nil {
			t.Errorf("%s must be untouched: %q (redacted_at %v)", id, got.Content, got.RedactedAt)
		}
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM memory_erasure_targets WHERE request_id = $1 AND target_kind = $2`,
		rec.RequestID, domain.ErasureTargetConversationMessage); n != 2 {
		t.Errorf("reply tombstones = %d, want 2", n)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM conversation_messages WHERE role = 'agent' AND content LIKE '%'||$1||'%'`, erasureSecret); n != 0 {
		t.Errorf("%d agent replies still hold the erased value", n)
	}

	got, found, err := er.Receipt(ctx, rec.RequestID)
	if err != nil || !found || !reflect.DeepEqual(got.RedactedMessages, want) {
		t.Fatalf("Receipt read back = %+v, %v, %v", got.RedactedMessages, found, err)
	}
	replay, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-red"}}))
	if err != nil || !replay.Replayed || !reflect.DeepEqual(replay.RedactedMessages, want) {
		t.Fatalf("replay = %+v, %v", replay, err)
	}

	// Erasing the other document now reaches only its own reply; an already redacted
	// reply is not counted again.
	rec2, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-keep"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec2.RedactedMessages, []string{"lctest-m4"}) || rec2.Counts[eraseConversationMessages] != 1 {
		t.Fatalf("second erase redacted %v (counts %v), want only lctest-m4", rec2.RedactedMessages, rec2.Counts)
	}
}

// A document-less chunk (a mnemonic fact) is erased alone; a reply citing it is
// redacted as well (the eraseChunks path).
func TestPg_Erase_RedactsRepliesCitingADocumentlessChunk(t *testing.T) {
	a, _, er, ctx := newErasureStore(t)
	execSQL(t, a, ctx, `INSERT INTO chunks (id, text, metadata) VALUES ('lctest-fact-1', $1, '{}'::jsonb)`, erasureSecret)
	conv, err := NewPgConversationStore(ctx, a.pool)
	if err != nil {
		t.Fatal(err)
	}
	clean := func() {
		_, _ = a.pool.Exec(ctx, `DELETE FROM conversations WHERE id LIKE 'lctest-%'`)
		_, _ = a.pool.Exec(ctx, `DELETE FROM chunks WHERE id = 'lctest-fact-1'`)
	}
	t.Cleanup(clean)
	if err := conv.CreateConversation(ctx, domain.Conversation{ID: "lctest-conv2", OwnerID: "lctest-emp",
		Status: domain.ConversationOpen, Profile: domain.ProfileEmployee}); err != nil {
		t.Fatal(err)
	}
	if _, err := conv.AppendMessage(ctx, domain.Message{ID: "lctest-f1", ConversationID: "lctest-conv2",
		Role: domain.MessageRoleAgent, Content: erasureSecret, SourceRefs: []string{"lctest-fact-1"}}); err != nil {
		t.Fatal(err)
	}
	rec, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{ChunkIDs: []string{"lctest-fact-1"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec.RedactedMessages, []string{"lctest-f1"}) {
		t.Fatalf("chunk erase redacted %v", rec.RedactedMessages)
	}
}
