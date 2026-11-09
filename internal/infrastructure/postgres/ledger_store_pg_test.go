package postgres

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory/vault"
)

// The run ledger store (ADR-0145 D1) against a real, migrated Postgres
// (PG_TEST_DSN; 0034_ledger_entries) with a temp-dir artifact vault. Rows use the
// "pgled-" conversation prefix.
func newLedgerStore(t *testing.T) (*PgLedgerStore, *vault.ArtifactVault, context.Context) {
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
	v := vault.NewArtifactVault(t.TempDir())
	st := NewPgLedgerStore(pool, v)
	if !st.Ready(ctx) {
		t.Skip("ledger_entries missing (run `orchestrator migrate up` first)")
	}
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM ledger_entries WHERE conversation_id LIKE 'pgled-%'`)
	}
	clean()
	t.Cleanup(clean)
	return st, v, ctx
}

func pgEntry(id string, kind domain.LedgerKind, run, turn string, step int, owner string, body string, expires time.Time) domain.LedgerEntry {
	tags := []string{}
	if owner != "" {
		tags = append(tags, domain.OwnerTag(owner))
	}
	return domain.LedgerEntry{
		ID: id, Kind: kind, ConversationID: "pgled-conv", TurnID: turn, RunID: run, StepIndex: step,
		ToolName: "crm_get", Args: []byte(`{"id":"C-4"}`), Writer: "agent:worker", Owner: owner, Tags: tags,
		ContentType: "application/json", SizeBytes: int64(len(body)), Preview: body,
		CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: expires, Body: []byte(body),
	}
}

func TestPg_LedgerPutGetFindIdempotent(t *testing.T) {
	st, _, ctx := newLedgerStore(t)
	later := time.Now().Add(time.Hour)
	e := pgEntry("led_pgled_1", domain.LedgerToolResult, "pgled-run", "pgled-turn", 2, "alice", `{"amount":1250.5}`, later)
	if err := st.Put(ctx, e); err != nil {
		t.Fatal(err)
	}
	// A retried write with the same id changes nothing.
	dup := e
	dup.Preview = "changed"
	if err := st.Put(ctx, dup); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, e.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if string(got.Body) != `{"amount":1250.5}` || got.Preview != e.Preview || got.StepIndex != 2 ||
		got.Owner != "alice" || len(got.Tags) != 1 || got.Tags[0] != domain.OwnerTag("alice") || got.BodyHash == "" {
		t.Fatalf("round trip: %+v", got)
	}
	if err := st.Put(ctx, pgEntry("led_pgled_2", domain.LedgerStepOutput, "pgled-run", "pgled-turn", 3, "alice", "the total is 1330.50", later)); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Find(ctx, domain.LedgerQuery{RunID: "pgled-run"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("find by run: %d %v", len(rows), err)
	}
	rows, _ = st.Find(ctx, domain.LedgerQuery{ConversationID: "pgled-conv", Kind: domain.LedgerStepOutput, Text: "1330"})
	if len(rows) != 1 || rows[0].ID != "led_pgled_2" || rows[0].Body != nil {
		t.Fatalf("find by kind+text: %+v", rows)
	}
	rows, _ = st.Find(ctx, domain.LedgerQuery{RunID: "pgled-run", StepIndex: 2, HasStep: true, ToolName: "crm_get"})
	if len(rows) != 1 || rows[0].ID != "led_pgled_1" {
		t.Fatalf("find by step+tool: %+v", rows)
	}
	if _, err := st.Find(ctx, domain.LedgerQuery{Kind: domain.LedgerToolResult}); err == nil {
		t.Fatal("an unscoped find must be refused")
	}
}

// Scope enforcement end to end over the real store: Alice's entry is absent for
// Bob by id and by query.
func TestPg_LedgerEnforcedCrossPrincipal(t *testing.T) {
	st, _, ctx := newLedgerStore(t)
	later := time.Now().Add(time.Hour)
	_ = st.Put(ctx, pgEntry("led_pgled_a", domain.LedgerToolResult, "pgled-run", "t", 0, "alice", "alice data", later))
	_ = st.Put(ctx, pgEntry("led_pgled_b", domain.LedgerToolResult, "pgled-run", "t", 1, "bob", "bob data", later))
	l := authz.NewEnforcingLedger(st, nil)
	bob := domain.WithScope(ctx, &domain.TagPredicate{Bypass: true, Owners: []string{"bob"}})
	if e, err := l.Get(bob, "led_pgled_a"); err != nil || e != nil {
		t.Fatalf("bob read alice's entry: %+v %v", e, err)
	}
	rows, err := l.Find(bob, domain.LedgerQuery{RunID: "pgled-run"})
	if err != nil || len(rows) != 1 || rows[0].ID != "led_pgled_b" {
		t.Fatalf("bob's find = %+v %v", rows, err)
	}
}

// Retention: expired entries are purged with their vault bytes, unless another
// entry or an artifact still names them; scratch goes at turn end.
func TestPg_LedgerRetentionAndScratch(t *testing.T) {
	st, v, ctx := newLedgerStore(t)
	past := time.Now().Add(-time.Second)
	later := time.Now().Add(time.Hour)
	shared := "shared with an artifact"
	sharedHash, _ := v.Store([]byte(shared))
	st.ArtifactReferenced = func(h string) bool { return h == sharedHash }

	_ = st.Put(ctx, pgEntry("led_pgled_old", domain.LedgerToolResult, "pgled-run", "t1", 0, "", "old result", past))
	_ = st.Put(ctx, pgEntry("led_pgled_art", domain.LedgerArtifact, "pgled-run", "t1", 0, "", shared, past))
	_ = st.Put(ctx, pgEntry("led_pgled_live", domain.LedgerStepOutput, "pgled-run", "t1", 1, "", "live", later))
	_ = st.Put(ctx, pgEntry("led_pgled_scr", domain.LedgerScratch, "pgled-run", "t1", 1, "", "scratch", later))
	old, _ := st.Get(ctx, "led_pgled_live")
	if old == nil {
		t.Fatal("live entry missing")
	}
	if got, _ := st.Get(ctx, "led_pgled_old"); got != nil {
		t.Fatal("an expired entry is still readable before the purge")
	}

	n, err := st.DeleteScratch(ctx, "t1")
	if err != nil || n != 1 {
		t.Fatalf("scratch: %d %v", n, err)
	}
	if n, err = st.PurgeExpired(ctx, time.Now()); err != nil || n < 2 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(v.VaultPath, sharedHash)); err != nil {
		t.Fatalf("vault bytes an artifact still uses were deleted: %v", err)
	}
	oldHash := hashOf(t, "old result")
	if _, err := os.Stat(filepath.Join(v.VaultPath, oldHash)); !os.IsNotExist(err) {
		t.Fatalf("unreferenced vault bytes of a purged entry survive: %v", err)
	}
	if got, _ := st.Get(ctx, "led_pgled_live"); got == nil || string(got.Body) != "live" {
		t.Fatal("an entry inside its retention was purged")
	}
}

// hashOf is the vault name of content (the vault is content-addressed).
func hashOf(t *testing.T, content string) string {
	t.Helper()
	tmp := vault.NewArtifactVault(t.TempDir())
	h, err := tmp.Store([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Migration 0035: a derived entry's inherited classifications round-trip, and the
// enforced reader refuses a reader any source set excludes, by id and by query
// (T-20261108-ledger-step-outputs-do-not-inherit-the).
func TestPg_LedgerSourceTagsRoundTripAndGateReads(t *testing.T) {
	st, _, ctx := newLedgerStore(t)
	later := time.Now().Add(time.Hour)
	e := pgEntry("led_pg_src_1", domain.LedgerStepOutput, "pgled-run-src", "pgled-turn-src", 0, "", "derived", later)
	e.SourceTags = [][]string{{"hr"}, {"finance", "legal"}}
	if err := st.Put(ctx, e); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, e.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if len(got.SourceTags) != 2 || got.SourceTags[0][0] != "hr" || got.SourceTags[1][1] != "legal" {
		t.Fatalf("source_tags did not round-trip: %v", got.SourceTags)
	}
	reader := authz.NewEnforcingLedger(st, nil)
	noHR := domain.WithScope(ctx, &domain.TagPredicate{ForbiddenTags: []string{"hr"}})
	if g, err := reader.Get(noHR, e.ID); err != nil || g != nil {
		t.Fatalf("a reader without hr read a derived entry by id (err %v)", err)
	}
	if rows, err := reader.Find(noHR, domain.LedgerQuery{RunID: "pgled-run-src"}); err != nil || len(rows) != 0 {
		t.Fatalf("a reader without hr found %d derived entries (err %v)", len(rows), err)
	}
	if g, err := reader.Get(domain.WithScope(ctx, &domain.TagPredicate{}), e.ID); err != nil || g == nil {
		t.Fatalf("an unrestricted reader could not read it (err %v)", err)
	}
}
