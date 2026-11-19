package postgres

import (
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2109: an agent reply's sources round-trip (sorted, deduplicated), a replay of
// the same ClientID returns them, and the person's own message never carries any.
func TestConversationStore_AgentReplySourceRefs(t *testing.T) {
	s, pool, ctx := newConvStore(t)
	seedConv(t, s, ctx, "c-src")

	user, err := s.AppendMessage(ctx, domain.Message{ID: "m-user", ConversationID: "c-src", Role: domain.MessageRoleUser,
		Content: "what is it?", SourceRefs: []string{"doc-should-not-stick"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(user.SourceRefs) != 0 {
		t.Errorf("user message refs = %v, want none", user.SourceRefs)
	}
	reply, err := s.AppendMessage(ctx, domain.Message{ID: "m-agent", ConversationID: "c-src", Role: domain.MessageRoleAgent,
		Content: "it is X", ClientID: "k1", SourceRefs: []string{"doc-1", "chunk-b", "doc-1", "chunk-a"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"chunk-a", "chunk-b", "doc-1"}
	if !reflect.DeepEqual(reply.SourceRefs, want) {
		t.Errorf("returned refs = %v, want %v", reply.SourceRefs, want)
	}
	replay, err := s.AppendMessage(ctx, domain.Message{ID: "m-agent-2", ConversationID: "c-src", Role: domain.MessageRoleAgent,
		Content: "it is X", ClientID: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if replay.ID != "m-agent" || !reflect.DeepEqual(replay.SourceRefs, want) {
		t.Errorf("replay = %+v", replay)
	}

	msgs, err := s.ListMessages(ctx, "c-src", 0, 0)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("ListMessages = %d, %v", len(msgs), err)
	}
	if len(msgs[0].SourceRefs) != 0 || msgs[0].RedactedAt != nil {
		t.Errorf("user row = %+v", msgs[0])
	}
	if !reflect.DeepEqual(msgs[1].SourceRefs, want) || msgs[1].RedactedAt != nil {
		t.Errorf("reply row = %+v", msgs[1])
	}
	var raw string
	if err := pool.QueryRow(ctx, `SELECT source_refs::text FROM conversation_messages WHERE id = 'm-user'`).Scan(&raw); err != nil || raw != "[]" {
		t.Errorf("user row source_refs = %q, %v; want []", raw, err)
	}
}

// ENG-2165: a reply's own refs round-trip beside the union (and are folded into it); a
// row stored before migration 0063 (own_source_refs NULL) reads every ref as its own.
func TestConversationStore_AgentReplyOwnSourceRefs(t *testing.T) {
	s, pool, ctx := newConvStore(t)
	seedConv(t, s, ctx, "c-own")

	reply, err := s.AppendMessage(ctx, domain.Message{ID: "m-own", ConversationID: "c-own", Role: domain.MessageRoleAgent,
		Content: "it is X", SourceRefs: []string{"doc-inherited"}, OwnSourceRefs: []string{"doc-own", "chunk-own"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reply.OwnSourceRefs, []string{"chunk-own", "doc-own"}) ||
		!reflect.DeepEqual(reply.SourceRefs, []string{"chunk-own", "doc-inherited", "doc-own"}) {
		t.Fatalf("returned = own %v refs %v; the own refs belong to the union too", reply.OwnSourceRefs, reply.SourceRefs)
	}
	if _, err := s.AppendMessage(ctx, domain.Message{ID: "m-own-user", ConversationID: "c-own", Role: domain.MessageRoleUser,
		Content: "ok", OwnSourceRefs: []string{"doc-x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, domain.Message{ID: "m-legacy", ConversationID: "c-own", Role: domain.MessageRoleAgent,
		Content: "older", SourceRefs: []string{"doc-a", "doc-b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_messages SET own_source_refs = NULL WHERE id = 'm-legacy'`); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ListMessages(ctx, "c-own", 0, 0)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("ListMessages = %d, %v", len(msgs), err)
	}
	if !reflect.DeepEqual(msgs[0].OwnSourceRefs, []string{"chunk-own", "doc-own"}) {
		t.Errorf("reply own = %v", msgs[0].OwnSourceRefs)
	}
	if len(msgs[1].OwnSourceRefs) != 0 {
		t.Errorf("user message own = %v, want none", msgs[1].OwnSourceRefs)
	}
	if !reflect.DeepEqual(msgs[2].OwnSourceRefs, []string{"doc-a", "doc-b"}) {
		t.Errorf("legacy row own = %v, want every ref (pre-0063 rows read as own)", msgs[2].OwnSourceRefs)
	}
}
