package postgres

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// Integration tests for the session/run/checkpoint store (session Phase 4). Skips without
// PG_TEST_DSN.
//
// The schema is applied by reading the REAL migration files rather than an inline copy, so
// this test cannot silently drift from the DDL that ships (0004 is authoritative, ADR-0064).
// They are applied into the test's OWN schema (pgscratch, isolated): it used to TRUNCATE the
// migrated sessions/runs/run_checkpoints, wiping rows of anything else using the database
// (T-20261114-postgres-adapter-tests-drop-migrated-tables).
func newSessionStore(t *testing.T) (*PgSessionStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := pgscratch.Pool(t, "session_store_test", pgscratch.Options{
		Migrations: []string{"0004_sessions_runs.sql", "0005_session_conversation_link.sql"},
	})

	store, err := NewPgSessionStore(ctx, pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return store, pool, ctx
}

// The constructor's two guards probe the relation the store will use (search_path), so an
// unmigrated or half-migrated schema is refused with the migration to run — even when a
// migrated public.sessions exists on the same database. A shadowing scratch schema keeps
// public on the search_path to prove exactly that.
func TestNewPgSessionStore_GuardsProbeTheResolvedRelation(t *testing.T) {
	pool, ctx := pgscratch.Pool(t, "session_guard_test", pgscratch.Options{Shadow: true})
	var publicSessions bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.sessions') IS NOT NULL`).Scan(&publicSessions); err != nil {
		t.Fatal(err)
	}
	if !publicSessions {
		// Unmigrated database: nothing to shadow; the empty schema case is the plain one.
		if _, err := NewPgSessionStore(ctx, pool); err == nil || !contains(err.Error(), "0004_sessions_runs.sql") {
			t.Fatalf("unmigrated: want the 0004 refusal, got %v", err)
		}
		return
	}

	// 0004 only, in the scratch schema: it shadows the fully migrated public.sessions,
	// which HAS conversation_id — the column guard must look at the shadowing table.
	ddl, err := os.ReadFile(filepath.Join("..", "..", "migrate", "migrations", "0004_sessions_runs.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(ddl)); err != nil {
		t.Fatalf("apply 0004: %v", err)
	}
	if _, err := NewPgSessionStore(ctx, pool); err == nil || !contains(err.Error(), "0005_session_conversation_link.sql") {
		t.Fatalf("half-migrated: want the 0005 refusal, got %v", err)
	}
}

func mkSession(id string, status domain.SessionStatus) domain.Session {
	now := time.Now().UTC()
	return domain.Session{
		ID: domain.SessionID(id), Goal: "goal " + id, Status: status,
		CreatedAt: now, UpdatedAt: now,
	}
}

func TestPgSessionStore_SaveGetRoundTrip(t *testing.T) {
	store, _, ctx := newSessionStore(t)
	want := mkSession("s1", domain.SessionActive)
	want.CallerScope = domain.TagSet{ForbiddenTags: []string{"secrets"}}

	if err := store.SaveSession(ctx, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.GetSession(ctx, "s1")
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%t)", err, got == nil)
	}
	if got.Goal != want.Goal || got.Status != domain.SessionActive {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if len(got.CallerScope.ForbiddenTags) != 1 || got.CallerScope.ForbiddenTags[0] != "secrets" {
		t.Errorf("caller_scope must round-trip, got %+v", got.CallerScope)
	}
}

func TestPgSessionStore_GetUnknownIsNilNotError(t *testing.T) {
	store, _, ctx := newSessionStore(t)
	got, err := store.GetSession(ctx, "nope")
	if err != nil {
		t.Fatalf("unknown session should not error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// SaveSession is an upsert: the manager re-saves the same row on every transition.
func TestPgSessionStore_SaveIsUpsert(t *testing.T) {
	store, pool, ctx := newSessionStore(t)
	s := mkSession("s1", domain.SessionActive)
	if err := store.SaveSession(ctx, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	s.Status = domain.SessionPaused
	s.UpdatedAt = time.Now().UTC()
	if err := store.SaveSession(ctx, s); err != nil {
		t.Fatalf("re-save: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE id='s1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("expected one row after upsert, got %d", n)
	}
	got, _ := store.GetSession(ctx, "s1")
	if got.Status != domain.SessionPaused {
		t.Errorf("status = %q, want paused", got.Status)
	}
}

func TestPgSessionStore_ListFiltersByStatus(t *testing.T) {
	store, _, ctx := newSessionStore(t)
	for id, st := range map[string]domain.SessionStatus{
		"a": domain.SessionActive, "b": domain.SessionActive, "c": domain.SessionDormant,
	} {
		if err := store.SaveSession(ctx, mkSession(id, st)); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	active, err := store.ListSessions(ctx, domain.SessionActive)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("expected 2 active, got %d", len(active))
	}
	all, err := store.ListSessions(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 total, got %d", len(all))
	}
}

// A run carries its plan across persistence — the property resume depends on.
func TestPgSessionStore_RunPersistsPlan(t *testing.T) {
	store, _, ctx := newSessionStore(t)
	if err := store.SaveSession(ctx, mkSession("s1", domain.SessionActive)); err != nil {
		t.Fatalf("save session: %v", err)
	}
	run := domain.Run{
		ID: "r1", SessionID: "s1", Status: domain.RunRunning, StartedAt: time.Now().UTC(),
		Plan: &domain.ExecutionPlan{Subject: "x", Steps: []domain.Step{{Query: "a"}, {Query: "b", DependsOn: []int{0}}}},
	}
	if err := store.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save run: %v", err)
	}
	got, err := store.GetRun(context.Background(), "r1")
	if err != nil || got == nil {
		t.Fatalf("get run: %v (nil=%t)", err, got == nil)
	}
	if got.Plan == nil || len(got.Plan.Steps) != 2 || got.Plan.Steps[1].DependsOn[0] != 0 {
		t.Fatalf("plan must round-trip intact, got %+v", got.Plan)
	}
	if !got.Resumable() {
		t.Error("a running run with a plan must be resumable")
	}
}

// step_index is an INTEGER column, so ordering is numeric — the bug the bbolt string key had.
func TestPgSessionStore_CheckpointsOrderNumerically(t *testing.T) {
	store, _, ctx := newSessionStore(t)
	_ = store.SaveSession(ctx, mkSession("s1", domain.SessionActive))
	_ = store.SaveRun(context.Background(), domain.Run{ID: "r1", SessionID: "s1", Status: domain.RunRunning, StartedAt: time.Now().UTC()})

	for _, step := range []int{2, 10, 0, 9, 1} {
		if err := store.SaveCheckpoint(context.Background(), "r1", step, map[string]string{"s": "x"}); err != nil {
			t.Fatalf("save step %d: %v", step, err)
		}
	}
	metas, err := store.ListCheckpoints(context.Background(), "r1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []int{0, 1, 2, 9, 10}
	if len(metas) != len(want) {
		t.Fatalf("expected %d checkpoints, got %d", len(want), len(metas))
	}
	for i, w := range want {
		if metas[i].StepIndex != w {
			t.Fatalf("checkpoint order wrong: got %d at %d, want %d", metas[i].StepIndex, i, w)
		}
	}
}

func TestPgSessionStore_CheckpointRoundTrip(t *testing.T) {
	store, _, ctx := newSessionStore(t)
	_ = store.SaveSession(ctx, mkSession("s1", domain.SessionActive))
	_ = store.SaveRun(context.Background(), domain.Run{ID: "r1", SessionID: "s1", Status: domain.RunRunning, StartedAt: time.Now().UTC()})

	want := map[string]string{"step_0": "done", "note": "keep"}
	if err := store.SaveCheckpoint(context.Background(), "r1", 0, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.LoadCheckpoint(context.Background(), "r1", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got["step_0"] != "done" || got["note"] != "keep" {
		t.Errorf("round-trip mismatch: %v", got)
	}
	missing, err := store.LoadCheckpoint(context.Background(), "r1", 99)
	if err != nil || missing != nil {
		t.Errorf("absent checkpoint should be (nil,nil), got %v %v", missing, err)
	}
}

// The cascade is the reason this moved out of bbolt: reclaiming a session must reclaim its
// runs and their checkpoints, in one statement, without three sweeps that can drift apart.
func TestPgSessionStore_PurgeCascadesToRunsAndCheckpoints(t *testing.T) {
	store, pool, ctx := newSessionStore(t)

	old := mkSession("old", domain.SessionCompleted)
	old.CompletedAt = time.Now().UTC().Add(-48 * time.Hour)
	if err := store.SaveSession(ctx, old); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.SaveRun(context.Background(), domain.Run{ID: "r-old", SessionID: "old", Status: domain.RunCompleted, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := store.SaveCheckpoint(context.Background(), "r-old", 0, map[string]string{"k": "v"}); err != nil {
		t.Fatalf("save cp: %v", err)
	}

	n, err := store.PurgeCompletedBefore(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 session purged, got %d", n)
	}
	for _, q := range []string{
		`SELECT count(*) FROM sessions WHERE id='old'`,
		`SELECT count(*) FROM runs WHERE id='r-old'`,
		`SELECT count(*) FROM run_checkpoints WHERE run_id='r-old'`,
	} {
		var c int
		if err := pool.QueryRow(ctx, q).Scan(&c); err != nil {
			t.Fatalf("count: %v", err)
		}
		if c != 0 {
			t.Errorf("cascade incomplete for %q: %d rows remain", q, c)
		}
	}
}

// Retention must never touch live state: only COMPLETED sessions are eligible, and a
// dormant session is explicitly resumable.
func TestPgSessionStore_PurgeSpareLiveAndRecentSessions(t *testing.T) {
	store, _, ctx := newSessionStore(t)
	stale := time.Now().UTC().Add(-48 * time.Hour)

	for id, st := range map[string]domain.SessionStatus{
		"active": domain.SessionActive, "paused": domain.SessionPaused, "dormant": domain.SessionDormant,
	} {
		s := mkSession(id, st)
		s.UpdatedAt = stale
		if err := store.SaveSession(ctx, s); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	recent := mkSession("recent-done", domain.SessionCompleted)
	recent.CompletedAt = time.Now().UTC()
	if err := store.SaveSession(ctx, recent); err != nil {
		t.Fatalf("save: %v", err)
	}

	n, err := store.PurgeCompletedBefore(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 0 {
		t.Fatalf("retention purged live or recent sessions (%d)", n)
	}
	all, _ := store.ListSessions(ctx, "")
	if len(all) != 4 {
		t.Errorf("expected all 4 sessions intact, got %d", len(all))
	}
}

// ADR-0084 D2 from the read side: what did this conversation set in motion?
func TestPgSessionStore_ListSessionsForConversation(t *testing.T) {
	store, _, ctx := newSessionStore(t)

	linked1 := mkSession("s1", domain.SessionActive)
	linked1.ConversationID, linked1.OriginMessageID = "conv-1", "msg-1"
	linked2 := mkSession("s2", domain.SessionCompleted)
	linked2.ConversationID, linked2.OriginMessageID = "conv-1", "msg-4"
	other := mkSession("s3", domain.SessionActive)
	other.ConversationID = "conv-OTHER"
	unlinked := mkSession("s4", domain.SessionActive) // ordinary, non-chat work

	for _, s := range []domain.Session{linked1, linked2, other, unlinked} {
		if err := store.SaveSession(ctx, s); err != nil {
			t.Fatalf("save %s: %v", s.ID, err)
		}
	}

	got, err := store.ListSessionsForConversation(ctx, "conv-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions for conv-1, got %d", len(got))
	}
	for _, s := range got {
		if s.ConversationID != "conv-1" {
			t.Errorf("leaked a session from %q", s.ConversationID)
		}
	}
	// The causation half must survive persistence, not just the correlation half.
	if got[0].OriginMessageID == "" && got[1].OriginMessageID == "" {
		t.Error("origin_message_id must round-trip — it is what makes the link auditable")
	}
}

func TestPgSessionStore_ConversationLinkRoundTrips(t *testing.T) {
	store, _, ctx := newSessionStore(t)
	s := mkSession("s1", domain.SessionActive)
	s.ConversationID, s.OriginMessageID = "conv-9", "msg-42"
	if err := store.SaveSession(ctx, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.GetSession(ctx, "s1")
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.ConversationID != "conv-9" || got.OriginMessageID != "msg-42" {
		t.Errorf("link lost in persistence: %q/%q", got.ConversationID, got.OriginMessageID)
	}
}

// A session with no conversation is the common case and must not appear in any
// conversation's list.
func TestPgSessionStore_UnlinkedSessionsAreNotListed(t *testing.T) {
	store, _, ctx := newSessionStore(t)
	if err := store.SaveSession(ctx, mkSession("plain", domain.SessionActive)); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.ListSessionsForConversation(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an empty conversation id must match nothing, got %d", len(got))
	}
}

// The request's ctx reaches the query: a cancelled caller gets context.Canceled
// instead of a query run under context.Background()
// (T-20261113-25-i-o-methods-on-domain-ports-take-no-context, AGENTS.md §2.7).
func TestPgSessionStore_RunAndCheckpointQueriesHonourTheCallersContext(t *testing.T) {
	store, _, ctx := newSessionStore(t)
	if err := store.SaveSession(ctx, mkSession("s1", domain.SessionActive)); err != nil {
		t.Fatalf("save session: %v", err)
	}
	if err := store.SaveRun(ctx, domain.Run{ID: "rc", SessionID: "s1", Status: domain.RunRunning, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("save run: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.GetRun(cancelled, "rc"); !errors.Is(err, context.Canceled) {
		t.Errorf("GetRun under a cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := store.ListRunsForSession(cancelled, "s1"); !errors.Is(err, context.Canceled) {
		t.Errorf("ListRunsForSession under a cancelled ctx = %v, want context.Canceled", err)
	}
	if err := store.SaveRun(cancelled, domain.Run{ID: "rc2", SessionID: "s1"}); !errors.Is(err, context.Canceled) {
		t.Errorf("SaveRun under a cancelled ctx = %v, want context.Canceled", err)
	}
	if err := store.SaveCheckpoint(cancelled, "rc", 0, map[string]string{"k": "v"}); !errors.Is(err, context.Canceled) {
		t.Errorf("SaveCheckpoint under a cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := store.ListCheckpoints(cancelled, "rc"); !errors.Is(err, context.Canceled) {
		t.Errorf("ListCheckpoints under a cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := store.LoadCheckpoint(cancelled, "rc", 0); !errors.Is(err, context.Canceled) {
		t.Errorf("LoadCheckpoint under a cancelled ctx = %v, want context.Canceled", err)
	}
}
