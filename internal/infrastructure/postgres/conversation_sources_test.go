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
