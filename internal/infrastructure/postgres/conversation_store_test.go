package postgres

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// Integration tests for the conversation store (ADR-0084 D1). Skips without PG_TEST_DSN.
//
// The schema is applied by reading the REAL migration file rather than an inline copy, so
// this test cannot silently drift from the DDL that ships (migration 0002 is authoritative
// per ADR-0064).
func newConvStore(t *testing.T) (*PgConversationStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS conversation_messages, conversations CASCADE`)

	// Apply the REAL migration files, in order, so the test schema cannot drift from what
	// ships (0002 base + 0003 policy column; ADR-0064).
	for _, name := range []string{"0002_conversations.sql", "0003_conversation_policy.sql"} {
		path := filepath.Join("..", "..", "migrate", "migrations", name)
		ddl, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration %s (the test applies the real migration on purpose): %v", path, err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}

	store, err := NewPgConversationStore(ctx, pool)
	if err != nil {
		t.Fatalf("NewPgConversationStore: %v", err)
	}
	return store, pool, ctx
}

func seedConv(t *testing.T, s *PgConversationStore, ctx context.Context, id string) {
	t.Helper()
	c := domain.Conversation{ID: id, OwnerID: "owner-1", Title: "t", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee}
	if err := s.CreateConversation(ctx, c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
}

// The constructor must refuse to run against an unmigrated database with an actionable
// error, rather than failing later with a raw SQL error on first use.
func TestNewPgConversationStore_UnmigratedIsActionable(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS conversation_messages, conversations CASCADE`)

	_, err = NewPgConversationStore(ctx, pool)
	if err == nil {
		t.Fatal("expected an error against an unmigrated DB")
	}
	if !contains(err.Error(), "migrate") {
		t.Errorf("error should tell the operator to migrate, got: %v", err)
	}
}

func TestConversationStore_CreateGetList(t *testing.T) {
	s, _, ctx := newConvStore(t)
	seedConv(t, s, ctx, "c1")

	got, err := s.GetConversation(ctx, "c1")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.OwnerID != "owner-1" || got.Profile != domain.ProfileEmployee || got.Status != domain.ConversationOpen {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	if _, err := s.GetConversation(ctx, "nope"); !errors.Is(err, domain.ErrConversationNotFound) {
		t.Fatalf("missing conversation: want ErrConversationNotFound, got %v", err)
	}

	seedConv(t, s, ctx, "c2")
	list, err := s.ListConversations(ctx, "owner-1", 10)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 conversations, got %d", len(list))
	}
	if other, _ := s.ListConversations(ctx, "someone-else", 10); len(other) != 0 {
		t.Errorf("owner filter leaked %d conversations", len(other))
	}
}

// Seq must start at 1 and increase by exactly one per append.
func TestConversationStore_AppendAssignsMonotonicSeq(t *testing.T) {
	s, _, ctx := newConvStore(t)
	seedConv(t, s, ctx, "c1")

	for i := int64(1); i <= 3; i++ {
		m, err := s.AppendMessage(ctx, domain.Message{
			ID: string(rune('a'+i)) + "-id", ConversationID: "c1",
			Role: domain.MessageRoleUser, Content: "hi",
		})
		if err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
		if m.Seq != i {
			t.Fatalf("append %d got Seq=%d, want %d", i, m.Seq, i)
		}
	}

	msgs, err := s.ListMessages(ctx, "c1", 0, 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	after, _ := s.ListMessages(ctx, "c1", 1, 0)
	if len(after) != 2 || after[0].Seq != 2 {
		t.Fatalf("afterSeq=1 should return seq 2,3; got %+v", after)
	}
}

// A retried turn must not duplicate a message, and must not burn a Seq.
func TestConversationStore_ClientIDIsIdempotent(t *testing.T) {
	s, _, ctx := newConvStore(t)
	seedConv(t, s, ctx, "c1")

	m := domain.Message{ID: "m1", ConversationID: "c1", Role: domain.MessageRoleUser, Content: "hello", ClientID: "turn-1"}
	first, err := s.AppendMessage(ctx, m)
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	retry := m
	retry.ID = "m2" // a retry may generate a fresh id; the ClientID is what dedups
	second, err := s.AppendMessage(ctx, retry)
	if err != nil {
		t.Fatalf("retry append: %v", err)
	}
	if second.ID != first.ID || second.Seq != first.Seq {
		t.Fatalf("retry created a new message: first=%+v second=%+v", first, second)
	}
	msgs, _ := s.ListMessages(ctx, "c1", 0, 0)
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 stored message, got %d", len(msgs))
	}

	// The next genuine append must still be Seq 2 — the retry must not have consumed one.
	next, err := s.AppendMessage(ctx, domain.Message{ID: "m3", ConversationID: "c1", Role: domain.MessageRoleAgent, Content: "reply"})
	if err != nil {
		t.Fatalf("next append: %v", err)
	}
	if next.Seq != 2 {
		t.Fatalf("retry burned a seq: next Seq=%d, want 2", next.Seq)
	}
}

func TestConversationStore_ClosedAndMissingAreDistinct(t *testing.T) {
	s, _, ctx := newConvStore(t)
	seedConv(t, s, ctx, "c1")

	if err := s.SetConversationStatus(ctx, "c1", domain.ConversationClosed); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err := s.AppendMessage(ctx, domain.Message{ID: "m1", ConversationID: "c1", Role: domain.MessageRoleUser, Content: "x"})
	if !errors.Is(err, domain.ErrConversationClosed) {
		t.Fatalf("append to closed: want ErrConversationClosed, got %v", err)
	}

	_, err = s.AppendMessage(ctx, domain.Message{ID: "m2", ConversationID: "ghost", Role: domain.MessageRoleUser, Content: "x"})
	if !errors.Is(err, domain.ErrConversationNotFound) {
		t.Fatalf("append to missing: want ErrConversationNotFound, got %v", err)
	}

	if err := s.SetConversationStatus(ctx, "ghost", domain.ConversationOpen); !errors.Is(err, domain.ErrConversationNotFound) {
		t.Fatalf("status on missing: want ErrConversationNotFound, got %v", err)
	}
}

// The design's central claim: Seq assignment is race-free because it happens as an
// UPDATE ... RETURNING under the conversation row lock. Concurrent appends must produce a
// contiguous, collision-free 1..N — a SELECT MAX(seq)+1 implementation fails this.
func TestConversationStore_ConcurrentAppendsDoNotCollide(t *testing.T) {
	s, _, ctx := newConvStore(t)
	seedConv(t, s, ctx, "c1")

	const n = 20
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seqs = map[int64]bool{}
		errs []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := s.AppendMessage(ctx, domain.Message{
				ID:             "msg-" + string(rune('A'+i)),
				ConversationID: "c1",
				Role:           domain.MessageRoleUser,
				Content:        "concurrent",
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if seqs[m.Seq] {
				errs = append(errs, errors.New("duplicate seq assigned"))
			}
			seqs[m.Seq] = true
		}(i)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d concurrent appends failed, first: %v", len(errs), errs[0])
	}
	for i := int64(1); i <= n; i++ {
		if !seqs[i] {
			t.Fatalf("seq %d missing — assignment is not contiguous", i)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
