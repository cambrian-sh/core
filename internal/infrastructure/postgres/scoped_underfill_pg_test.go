package postgres

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/doug-martin/goqu/v9"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/cambrian-sh/core/domain"
)

// Scoped-recall underfill measurement (ADR-0134 P2,
// T-20261101-scoped-recall-may-underfill-top-k-on-a-large).
//
// A MEASUREMENT, not a regression test: it seeds a store holding many principals'
// chunks, runs the adapter's own dense Search under a per-principal read scope
// (alone, and with an event-time window), and reports retrieved_count against
// min(top_k, rows the predicate admits). Opt-in because it inserts ~10k vectors:
//
//	ADR0134_UNDERFILL=1 PG_TEST_DSN=postgres://…/<a migrated *_test db> \
//	  go test -run TestPgScopedUnderfill_Measure -v ./internal/infrastructure/postgres/
//
// ADR0134_ITERATIVE=relaxed_order (or strict_order) runs the same measurement with
// pgvector's iterative index scan on, through the adapter's own config path
// (execution.retrieval.hnsw_iterative_scan).
const (
	ufPrincipals = 200
	ufPerPrinc   = 50
	ufTopK       = 100 // execution.retrieval.recall_over_fetch default (config.go)
	ufEfSearch   = 120 // execution.retrieval.hnsw_ef_search default
	ufProbes     = 20
)

func TestPgScopedUnderfill_Measure(t *testing.T) {
	if os.Getenv("ADR0134_UNDERFILL") != "1" {
		t.Skip("measurement; set ADR0134_UNDERFILL=1 (and PG_TEST_DSN) to run")
	}
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set")
	}
	ctx := context.Background()
	mode := os.Getenv("ADR0134_ITERATIVE")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if err := pgxvector.RegisterTypes(ctx, conn); err != nil {
			return err
		}
		stmts := hnswSessionSettings(ufEfSearch, mode, 0)
		// ADR0134_FORCE_HNSW=1 takes the plan choice away from the planner (no bitmap, no sort,
		// no sequential scan), which is what a larger store gets once its row estimates
		// price the exact plans out: it shows the failure mode and the remedy on a
		// store small enough to seed in minutes.
		if os.Getenv("ADR0134_FORCE_HNSW") == "1" {
			stmts = append(stmts, "SET enable_bitmapscan = off", "SET enable_seqscan = off", "SET enable_sort = off")
		}
		for _, stmt := range stmts {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	p := &PgVectorAdapter{pool: pool, dim: eventTestDim}

	rng := rand.New(rand.NewPCG(1, 2))
	centroids := make([][]float32, ufPrincipals)
	for i := range centroids {
		centroids[i] = randUnit(rng, nil, 0)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM chunks WHERE id LIKE 'uf-%'`).Scan(&n)
	if n != ufPrincipals*ufPerPrinc {
		_, _ = pool.Exec(ctx, `DELETE FROM chunks WHERE id LIKE 'uf-%'`)
		start := time.Now()
		base := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
		for pi := 0; pi < ufPrincipals; pi++ {
			batch := make([]*domain.Document, 0, ufPerPrinc)
			for j := 0; j < ufPerPrinc; j++ {
				at := base.Add(time.Duration(j) * 7 * 24 * time.Hour) // weekly over ~a year
				batch = append(batch, &domain.Document{
					ID: fmt.Sprintf("uf-%03d-%02d", pi, j), DocumentType: domain.DocTypeMnemonicFact,
					Text: "uf", ActivationStrength: 0.5,
					Embedding:  domain.Embedding{Vector: randUnit(rng, centroids[pi], 0.6)},
					Metadata:   map[string]any{"tags": []string{fmt.Sprintf("uf-principal-%03d", pi)}},
					CreatedAt:  time.Now().UTC(),
					EventStamp: domain.EventStamp{EventTime: at, TimeSource: domain.TimeSourceExplicit},
				})
			}
			if err := p.SaveBatch(ctx, batch); err != nil {
				t.Fatal(err)
			}
		}
		_, _ = pool.Exec(ctx, `ANALYZE chunks`)
		t.Logf("seeded %d chunks (%d principals x %d) in %s", ufPrincipals*ufPerPrinc, ufPrincipals, ufPerPrinc, time.Since(start))
	}
	var total int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM chunks`).Scan(&total)

	// Window: weeks 10..26 of each principal's year (17 of its 50 chunks).
	after := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC).Add(10 * 7 * 24 * time.Hour)
	before := after.Add(17 * 7 * 24 * time.Hour)

	// A one-week window holds one chunk per principal (200 rows of 10k, 2%).
	weekAfter := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC).Add(20 * 7 * 24 * time.Hour)
	weekBefore := weekAfter.Add(7 * 24 * time.Hour)

	type arm struct {
		name      string
		window    bool
		clustered bool
		want      int
		system    bool // no principal scope (a kernel/operator system read), window only
		narrow    bool // the one-week window instead of the 17-week one
	}
	for _, a := range []arm{
		{name: "scope, query near the principal's cluster", clustered: true, want: ufPerPrinc},
		{name: "scope+window, query near the cluster", window: true, clustered: true, want: 17},
		{name: "scope, query unrelated to the principal", want: ufPerPrinc},
		{name: "scope+window, query unrelated", window: true, want: 17},
		{name: "system read + 17-week window (34%)", window: true, system: true, want: ufTopK},
		{name: "system read + 1-week window (2%)", window: true, system: true, narrow: true, want: ufTopK},
	} {
		var got, under int
		var lat time.Duration
		for k := 0; k < ufProbes; k++ {
			pi := (k * 37) % ufPrincipals
			var qv []float32
			if a.clustered {
				qv = randUnit(rng, centroids[pi], 0.6)
			} else {
				qv = randUnit(rng, nil, 0)
			}
			opts := domain.SearchOptions{
				DocumentType: domain.DocTypeMnemonicFact, TopK: ufTopK,
				Scope: &domain.TagPredicate{RequiredTags: []string{fmt.Sprintf("uf-principal-%03d", pi)}},
			}
			if a.window {
				opts.Since, opts.Until = after, before
			}
			if a.narrow {
				opts.Since, opts.Until = weekAfter, weekBefore
			}
			if a.system {
				opts.Scope = &domain.TagPredicate{Bypass: true}
			}
			t0 := time.Now()
			res, err := p.Search(ctx, qv, opts)
			lat += time.Since(t0)
			if err != nil {
				t.Fatal(err)
			}
			got += len(res)
			if len(res) < a.want {
				under++
			}
		}
		t.Logf("MEASURE iterative=%q store=%d | %-44s retrieved_mean=%.1f want=%d underfilled=%d/%d mean_latency=%s",
			mode, total, a.name, float64(got)/ufProbes, a.want, under, ufProbes, lat/ufProbes)
	}

	// The plans: scoped + window, and a system read + the narrow window.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, pc := range []struct {
		name          string
		scope         *domain.TagPredicate
		since, before time.Time
	}{
		{"scope+window, principal 000", &domain.TagPredicate{RequiredTags: []string{"uf-principal-000"}}, after, before},
		{"system read + 1-week window", nil, weekAfter, weekBefore},
	} {
		ds := dialect.From(TableChunks).Select("id").
			Where(goqu.L("embedding IS NOT NULL")).
			Order(goqu.L("embedding <=> ?", pgvector.NewVector(randUnit(rng, centroids[0], 0.6))).Asc()).Limit(ufTopK * 20)
		for _, e := range scopeExpressions(pc.scope) {
			ds = ds.Where(e)
		}
		for _, e := range eventWindowExpressions(pc.since, pc.before, "") {
			ds = ds.Where(e)
		}
		sql, args, _ := ds.ToSQL()
		rows, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS OFF, TIMING OFF) "+sql, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var l string
			_ = rows.Scan(&l)
			if i := strings.Index(l, "'["); i >= 0 {
				l = l[:i] + "'[…]'"
			}
			plan = append(plan, l)
		}
		rows.Close()
		t.Logf("PLAN iterative=%q (%s):\n%s", mode, pc.name, strings.Join(plan, "\n"))
	}
}

// randUnit returns a random unit vector; with a centre, centre + noise*gaussian.
func randUnit(rng *rand.Rand, centre []float32, noise float64) []float32 {
	v := make([]float32, eventTestDim)
	var norm float64
	for i := range v {
		x := rng.NormFloat64()
		if centre != nil {
			x = float64(centre[i])*math.Sqrt(eventTestDim) + noise*x
		}
		v[i] = float32(x)
		norm += x * x
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}
