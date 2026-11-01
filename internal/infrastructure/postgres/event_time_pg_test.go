package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0134 against a real, migrated Postgres (PG_TEST_DSN; the schema comes from
// `orchestrator migrate up` with CAMBRIAN_EMBEDDER__DIMENSIONS=1536, as in CI).
// Rows use the id prefix "adr0134-" and are deleted before and after.

const eventTestDim = 1536

func newEventTimeAdapter(t *testing.T) (*PgVectorAdapter, context.Context) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse PG_TEST_DSN: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvector.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'chunks' AND column_name = 'event_time'`).Scan(&n); err != nil || n == 0 {
		t.Skipf("chunks.event_time missing (run `orchestrator migrate up` first): n=%d err=%v", n, err)
	}
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM chunks WHERE id LIKE 'adr0134-%'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM documents WHERE id LIKE 'adr0134-%'`)
	}
	clean()
	t.Cleanup(clean)
	return &PgVectorAdapter{pool: pool, dim: eventTestDim}, ctx
}

// unitVec is a one-hot vector; cosine(unitVec(i), unitVec(i)) = 1, orthogonal otherwise.
func unitVec(i int) []float32 {
	v := make([]float32, eventTestDim)
	v[i%eventTestDim] = 1
	return v
}

func stamp(t time.Time, src domain.TimeSource) domain.EventStamp {
	return domain.EventStamp{EventTime: t, TimeSource: src, TimePrecision: domain.TimePrecisionSecond}
}

func chunkDoc(id string, s domain.EventStamp, ingested time.Time, tags ...string) *domain.Document {
	meta := map[string]any{}
	if len(tags) > 0 {
		meta["tags"] = tags
	}
	return &domain.Document{
		ID: id, DocumentType: domain.DocTypeMnemonicFact, Text: "text " + id,
		Embedding: domain.Embedding{Vector: unitVec(0)}, Metadata: meta,
		ActivationStrength: 0.5, CreatedAt: ingested, EventStamp: s,
	}
}

// Owner decision Q6, end to end through the adapter upsert:
//   - an ingest default never overwrites a stored real (explicit) time;
//   - a restated time wins, with its new source;
//   - ingested_at (created_at) always moves on an ingest;
//   - a writer that states no stamp at all leaves both alone.
func TestPgEventTime_ReingestKeepsEventTimeUnlessRestated(t *testing.T) {
	p, ctx := newEventTimeAdapter(t)
	id := "adr0134-q6"
	t2023 := time.Date(2023, 5, 20, 7, 51, 0, 0, time.UTC)
	ing1 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	first := chunkDoc(id, stamp(t2023, domain.TimeSourceExplicit), ing1)
	first.EventTimeEnd = t2023.Add(time.Hour)
	if err := p.Save(ctx, first); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, p, ctx, id)
	if !got.EventTime.Equal(t2023) || !got.EventTimeEnd.Equal(t2023.Add(time.Hour)) ||
		got.TimeSource != domain.TimeSourceExplicit || !got.CreatedAt.Equal(ing1) {
		t.Fatalf("round trip: %+v created=%v", got.EventStamp, got.CreatedAt)
	}

	// Re-ingest with no stated time: the resolver stamps ingest_default = the ingest clock.
	ing2 := ing1.Add(24 * time.Hour)
	if err := p.Save(ctx, chunkDoc(id, stamp(ing2, domain.TimeSourceIngestDefault), ing2)); err != nil {
		t.Fatal(err)
	}
	got = mustGet(t, p, ctx, id)
	if !got.EventTime.Equal(t2023) || got.TimeSource != domain.TimeSourceExplicit || !got.EventTimeEnd.Equal(t2023.Add(time.Hour)) {
		t.Fatalf("an ingest default overwrote a stated time: %+v", got.EventStamp)
	}
	if !got.CreatedAt.Equal(ing2) {
		t.Fatalf("ingested_at must always update: created_at = %v, want %v", got.CreatedAt, ing2)
	}

	// A writer that states no stamp (e.g. a re-save of a loaded row) changes neither.
	plain := chunkDoc(id, domain.EventStamp{}, ing2.Add(time.Hour))
	if err := p.Save(ctx, plain); err != nil {
		t.Fatal(err)
	}
	got = mustGet(t, p, ctx, id)
	if !got.EventTime.Equal(t2023) || !got.CreatedAt.Equal(ing2) {
		t.Fatalf("an unstamped save moved the stamp or the ingest clock: %+v created=%v", got.EventStamp, got.CreatedAt)
	}

	// A restated time wins, source and all; the stale span end is replaced (NULL).
	t2024 := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	ing3 := ing2.Add(48 * time.Hour)
	if err := p.Save(ctx, chunkDoc(id, stamp(t2024, domain.TimeSourceSourceMetadata), ing3)); err != nil {
		t.Fatal(err)
	}
	got = mustGet(t, p, ctx, id)
	if !got.EventTime.Equal(t2024) || got.TimeSource != domain.TimeSourceSourceMetadata || !got.EventTimeEnd.IsZero() || !got.CreatedAt.Equal(ing3) {
		t.Fatalf("restated time did not win: %+v created=%v", got.EventStamp, got.CreatedAt)
	}

	// An ingest default over an ingest default moves with the ingest.
	id2 := "adr0134-q6-default"
	if err := p.Save(ctx, chunkDoc(id2, stamp(ing1, domain.TimeSourceIngestDefault), ing1)); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(ctx, chunkDoc(id2, stamp(ing2, domain.TimeSourceIngestDefault), ing2)); err != nil {
		t.Fatal(err)
	}
	if got = mustGet(t, p, ctx, id2); !got.EventTime.Equal(ing2) {
		t.Fatalf("ingest default over ingest default: %+v", got.EventStamp)
	}
}

// The document entity row follows the same rule (SaveDocument).
func TestPgEventTime_SaveDocumentKeepsStatedTime(t *testing.T) {
	p, ctx := newEventTimeAdapter(t)
	id := "adr0134-doc"
	t2023 := time.Date(2023, 5, 20, 0, 0, 0, 0, time.UTC)
	claimed := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	save := func(s domain.EventStamp) {
		t.Helper()
		if _, err := p.SaveDocument(ctx, domain.SourceDocument{ID: id, Title: "d", Text: "body", EventStamp: s}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() (et time.Time, src string, cl *time.Time) {
		t.Helper()
		if err := p.pool.QueryRow(ctx, `SELECT event_time, time_source, claimed_time FROM documents WHERE id = $1`, id).Scan(&et, &src, &cl); err != nil {
			t.Fatal(err)
		}
		return et.UTC(), src, cl
	}
	save(domain.EventStamp{EventTime: t2023, TimeSource: domain.TimeSourceClampedFuture, ClaimedTime: claimed, TimePrecision: domain.TimePrecisionDay})
	if et, src, cl := read(); !et.Equal(t2023) || src != "clamped_future" || cl == nil || !cl.UTC().Equal(claimed) {
		t.Fatalf("first save: %v %q %v", et, src, cl)
	}
	save(stamp(time.Now().UTC(), domain.TimeSourceIngestDefault))
	if et, src, cl := read(); !et.Equal(t2023) || src != "clamped_future" || cl == nil {
		t.Fatalf("ingest default overwrote the stored stamp: %v %q %v", et, src, cl)
	}
	t2024 := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	save(stamp(t2024, domain.TimeSourceExplicit))
	if et, src, cl := read(); !et.Equal(t2024) || src != "explicit" || cl != nil {
		t.Fatalf("restated: %v %q %v", et, src, cl)
	}
}

func mustGet(t *testing.T, p *PgVectorAdapter, ctx context.Context, id string) *domain.Document {
	t.Helper()
	d, err := p.GetByID(ctx, id)
	if err != nil || d == nil {
		t.Fatalf("GetByID(%s): %v %v", id, d, err)
	}
	return d
}

// windowFixture stores chunks around a [Feb 1, Jun 1) 2023 window. Every row carries
// the lexical token "zebra" and the same vector, so both lanes would return all of
// them without the predicates.
func windowFixture(t *testing.T, p *PgVectorAdapter, ctx context.Context) (after, before time.Time) {
	t.Helper()
	after = time.Date(2023, 2, 1, 0, 0, 0, 0, time.UTC)
	before = time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)
	ingest := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rows := []*domain.Document{
		chunkDoc("adr0134-w-in", stamp(time.Date(2023, 3, 1, 0, 0, 0, 0, time.UTC), domain.TimeSourceExplicit), ingest, "open"),
		chunkDoc("adr0134-w-at-after", stamp(after, domain.TimeSourceExplicit), ingest, "open"),
		chunkDoc("adr0134-w-at-before", stamp(before, domain.TimeSourceExplicit), ingest, "open"),
		chunkDoc("adr0134-w-early", stamp(time.Date(2022, 12, 1, 0, 0, 0, 0, time.UTC), domain.TimeSourceExplicit), ingest, "open"),
		chunkDoc("adr0134-w-secret-in", stamp(time.Date(2023, 3, 2, 0, 0, 0, 0, time.UTC), domain.TimeSourceExplicit), ingest, "secret"),
		// No stamp: windowed by created_at (its ingest clock), which is in the window.
		chunkDoc("adr0134-w-unstamped", domain.EventStamp{}, time.Date(2023, 4, 1, 0, 0, 0, 0, time.UTC), "open"),
	}
	straddle := chunkDoc("adr0134-w-span", stamp(time.Date(2023, 1, 20, 0, 0, 0, 0, time.UTC), domain.TimeSourceExplicit), ingest, "open")
	straddle.EventTimeEnd = time.Date(2023, 2, 10, 0, 0, 0, 0, time.UTC) // overlaps the window's start
	rows = append(rows, straddle)
	for _, d := range rows {
		d.Text = "zebra " + d.ID
		if err := p.Save(ctx, d); err != nil {
			t.Fatalf("save %s: %v", d.ID, err)
		}
	}
	return after, before
}

var windowWant = map[string]bool{
	"adr0134-w-in": true, "adr0134-w-at-after": true, "adr0134-w-unstamped": true, "adr0134-w-span": true,
}

func idsOf(rs []domain.SearchResult) map[string]bool {
	out := map[string]bool{}
	for _, r := range rs {
		if strings.HasPrefix(r.Document.ID, "adr0134-") {
			out[r.Document.ID] = true
		}
	}
	return out
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// ADR-0134 P2: the window is applied in SQL in the SAME query as the read scope,
// before the LIMIT, with span-overlap semantics (event_after inclusive, event_before
// exclusive), on both the dense and the lexical lane.
func TestPgEventWindow_PushedDownBesideScope(t *testing.T) {
	p, ctx := newEventTimeAdapter(t)
	after, before := windowFixture(t, p, ctx)
	opts := domain.SearchOptions{
		DocumentType: domain.DocTypeMnemonicFact, TopK: 50,
		Scope: &domain.TagPredicate{ForbiddenTags: []string{"secret"}},
		Since: after, Until: before,
	}
	dense, err := p.Search(ctx, unitVec(0), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(dense); !sameSet(got, windowWant) {
		t.Fatalf("dense lane returned %v, want %v", got, windowWant)
	}
	lex, err := p.LexicalSearch(ctx, "zebra", opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(lex); !sameSet(got, windowWant) {
		t.Fatalf("lexical lane returned %v, want %v", got, windowWant)
	}
	// Open-ended windows.
	opts.Since, opts.Until = time.Time{}, after
	dense, _ = p.Search(ctx, unitVec(0), opts)
	if got := idsOf(dense); !sameSet(got, map[string]bool{"adr0134-w-early": true, "adr0134-w-span": true}) {
		t.Fatalf("event_before only: %v", got)
	}
}

// The ADR-0107 projection read JOINs chunk_embeddings, which has its own
// created_at: the window must be qualified there or the query is ambiguous.
func TestPgEventWindow_ProjectionBranch(t *testing.T) {
	p, ctx := newEventTimeAdapter(t)
	p.projectionWrite, p.projectionRead, p.projModelID = true, true, "adr0134-test-model"
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM chunk_embeddings WHERE model_id = 'adr0134-test-model'`)
	})
	after, before := windowFixture(t, p, ctx)
	dense, err := p.Search(ctx, unitVec(0), domain.SearchOptions{
		DocumentType: domain.DocTypeMnemonicFact, TopK: 50,
		Scope: &domain.TagPredicate{ForbiddenTags: []string{"secret"}}, Since: after, Until: before,
	})
	if err != nil {
		t.Fatalf("projection search with a window: %v", err)
	}
	if got := idsOf(dense); !sameSet(got, windowWant) {
		t.Fatalf("projection lane returned %v, want %v", got, windowWant)
	}
}

// The predicate the adapter builds is the indexed expression: with sequential scans
// disabled the planner must reach for the out-of-band b-trees. If the two copies of
// the expression ever drift, this fails instead of every windowed read silently
// becoming a sequential scan.
func TestPgEventWindow_UsesTheEventTimeIndexes(t *testing.T) {
	p, ctx := newEventTimeAdapter(t)
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `RESET enable_seqscan`) //nolint:errcheck
	for name, w := range map[string][2]time.Time{
		"idx_chunks_event_start": {{}, time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)},
		"idx_chunks_event_end":   {time.Date(2023, 2, 1, 0, 0, 0, 0, time.UTC), {}},
	} {
		ds := dialect.From(TableChunks).Select("id")
		for _, e := range eventWindowExpressions(w[0], w[1], "") {
			ds = ds.Where(e)
		}
		sql, args, _ := ds.ToSQL()
		rows, err := conn.Query(ctx, "EXPLAIN "+sql, args...)
		if err != nil {
			t.Fatal(err)
		}
		plan := ""
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			plan += line + "\n"
		}
		rows.Close()
		if !strings.Contains(plan, name) {
			t.Fatalf("window predicate does not use %s:\n%s", name, plan)
		}
	}
}

// The per-connection pgvector settings follow execution.retrieval.hnsw_iterative_scan.
func TestHnswSessionSettings(t *testing.T) {
	t.Parallel()
	if got := hnswSessionSettings(120, "", 0); len(got) != 1 || got[0] != "SET hnsw.ef_search = 120" {
		t.Fatalf("default: %v", got)
	}
	if got := hnswSessionSettings(120, "off", 5000); len(got) != 1 {
		t.Fatalf("off must not touch iterative_scan: %v", got)
	}
	got := hnswSessionSettings(120, "relaxed_order", 5000)
	want := []string{"SET hnsw.ef_search = 120", "SET hnsw.iterative_scan = relaxed_order", "SET hnsw.max_scan_tuples = 5000"}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("relaxed: %v", got)
	}
}
