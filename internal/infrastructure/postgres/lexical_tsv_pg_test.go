package postgres

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-111 against a migrated Postgres, through NewPgVectorAdapter.
//   Part A: the stored chunks.text_tsv column ranks exactly like the old per-row
//   to_tsvector expression (same rows, same order, same scores).
//   Part B: and_first matches every term and falls back to any-term when that
//   finds fewer than top_k rows; or_only is the old any-term match.

var lexCorpus = []string{
	"Melanie painted a sunset for the charity auction.",
	"The charity auction raised money for the shelter in summer.",
	"Caroline went hiking last summer and painted the mountains.",
	"Quarterly invoice from Acme for the summer campaign.",
	"Acme sent the quarterly invoice late again.",
	"The invoice from Acme covers the quarterly retainer.",
	"Melanie and Caroline talked about the auction.",
	"A sunset over the lake, painted in oils.",
	"Acme ordered new chairs for the office.",
	"The quarterly report is due on Friday.",
}

type lexHit struct {
	id    string
	score float64
}

func lexAdapter(t *testing.T, mode string) (*PgVectorAdapter, context.Context) {
	t.Helper()
	dsn, dim := pgscratch.Migrated(t)
	ctx := context.Background()
	cfg := config.DefaultConfig()
	pgscratch.ApplyToConfig(t, cfg, dsn, dim)
	cfg.Storage.AutoMigrate = false
	cfg.Execution.Retrieval.LexicalMatchMode = mode
	p, err := NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(p.Close)
	return p, ctx
}

// seedLexCorpus writes the corpus under one tag and returns the scope that sees
// only it (other tests' rows share the database).
func seedLexCorpus(t *testing.T, p *PgVectorAdapter, ctx context.Context, tag string) *domain.TagPredicate {
	t.Helper()
	clean := func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM chunks WHERE id LIKE 'eng111-%'`)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM documents WHERE id LIKE 'eng111-%'`)
	}
	clean()
	t.Cleanup(clean)
	v := make([]float32, p.dim)
	v[0] = 1
	for i, text := range lexCorpus {
		if err := p.Save(ctx, &domain.Document{ID: fmt.Sprintf("eng111-%02d", i), DocumentType: domain.DocTypeMnemonicFact,
			Text: text, Embedding: domain.Embedding{Vector: v}, ActivationStrength: 0.5,
			Metadata: map[string]any{"tags": []string{tag}}}); err != nil {
			t.Fatal(err)
		}
	}
	return &domain.TagPredicate{RequiredTags: []string{tag}}
}

func sortedHits(h []lexHit) []lexHit {
	sort.SliceStable(h, func(i, j int) bool {
		if h[i].score != h[j].score {
			return h[i].score > h[j].score
		}
		return h[i].id < h[j].id
	})
	return h
}

func adapterHits(t *testing.T, p *PgVectorAdapter, ctx context.Context, q string, scope *domain.TagPredicate, topK int) []lexHit {
	t.Helper()
	res, err := p.LexicalSearch(domain.WithScope(ctx, scope), q, domain.SearchOptions{
		DocumentType: domain.DocTypeMnemonicFact, TopK: topK, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]lexHit, len(res))
	for i, r := range res {
		out[i] = lexHit{r.Document.ID, r.Score}
		if i > 0 && r.Score > res[i-1].Score {
			t.Fatalf("results not in descending score order: %v", out)
		}
	}
	return sortedHits(out)
}

// legacyHits runs the pre-ENG-111 lexical SQL: the per-row expression, OR or AND.
func legacyHits(t *testing.T, p *PgVectorAdapter, ctx context.Context, q, tag string, anyTerm bool) []lexHit {
	t.Helper()
	tsq := "plainto_tsquery('english', $1)"
	if anyTerm {
		tsq = "replace(plainto_tsquery('english', $1)::text, '&', '|')::tsquery"
	}
	rows, err := p.pool.Query(ctx, `SELECT id, ts_rank_cd(to_tsvector('english', text), `+tsq+`)
		FROM chunks WHERE to_tsvector('english', text) @@ `+tsq+` AND metadata->'tags' ? $2`, q, tag)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []lexHit
	for rows.Next() {
		var h lexHit
		var s float32
		if err := rows.Scan(&h.id, &s); err != nil {
			t.Fatal(err)
		}
		h.score = float64(s)
		out = append(out, h)
	}
	return sortedHits(out)
}

func sameHits(a, b []lexHit) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].id != b[i].id || math.Abs(a[i].score-b[i].score) > 1e-6 {
			return false
		}
	}
	return true
}

func TestPgLexical_StoredColumnRanksLikeTheExpression(t *testing.T) {
	p, ctx := lexAdapter(t, config.LexicalMatchOrOnly)
	var n int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'chunks' AND indexname = 'idx_chunks_text_tsv'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("idx_chunks_text_tsv missing: %d %v", n, err)
	}
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'chunks' AND indexname = 'idx_chunks_fts'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("idx_chunks_fts must be dropped: %d %v", n, err)
	}
	const tag = "eng111-diff"
	scope := seedLexCorpus(t, p, ctx, tag)
	for _, q := range []string{"quarterly invoice acme", "painted sunset", "charity auction summer", "the", "zebra"} {
		got, want := adapterHits(t, p, ctx, q, scope, 50), legacyHits(t, p, ctx, q, tag, true)
		if !sameHits(got, want) {
			t.Errorf("%q: stored column %v, expression %v", q, got, want)
		}
	}
	// EXPLAIN (ANALYZE, BUFFERS) of the old and the new ranking query, for the record.
	for _, c := range []struct{ name, sql string }{
		{"expression", `SELECT id, ts_rank_cd(to_tsvector('english', text), q) r FROM chunks, CAST(replace(plainto_tsquery('english', 'quarterly invoice acme')::text, '&', '|') AS tsquery) AS q WHERE to_tsvector('english', text) @@ q ORDER BY r DESC LIMIT 50`},
		{"column", `SELECT id, ts_rank_cd(text_tsv, q) r FROM chunks, CAST(replace(plainto_tsquery('english', 'quarterly invoice acme')::text, '&', '|') AS tsquery) AS q WHERE text_tsv @@ q ORDER BY r DESC LIMIT 50`},
	} {
		rows, err := p.pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+c.sql)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			plan = append(plan, line)
		}
		rows.Close()
		t.Logf("EXPLAIN %s:\n%s", c.name, strings.Join(plan, "\n"))
	}
}

func TestPgLexical_AndFirstFallsBackToAnyTerm(t *testing.T) {
	p, ctx := lexAdapter(t, "") // the default: and_first
	const tag = "eng111-and"
	scope := seedLexCorpus(t, p, ctx, tag)

	// Three rows carry quarterly, invoice and acme: with top_k 3 the AND match
	// is enough, and no row with only some of the words is ranked.
	got := adapterHits(t, p, ctx, "quarterly invoice acme", scope, 3)
	if want := legacyHits(t, p, ctx, "quarterly invoice acme", tag, false); len(want) != 3 || !sameHits(got, want) {
		t.Fatalf("AND match: %v, want %v", got, want)
	}
	// With top_k 5 the AND rows are too few: the any-term match is returned.
	got = adapterHits(t, p, ctx, "quarterly invoice acme", scope, 5)
	if want := legacyHits(t, p, ctx, "quarterly invoice acme", tag, true); len(want) < 5 || !sameHits(got, want[:5]) {
		t.Fatalf("fallback: %v, want %v", got, want)
	}
	// A QA-style question whose answer chunk lacks one of its words ("summer"):
	// no row has every term, the fallback finds the answer first.
	got = adapterHits(t, p, ctx, "What did Melanie paint for the charity auction last summer?", scope, 3)
	if len(got) == 0 || got[0].id != "eng111-00" {
		t.Fatalf("QA fallback: %v, want eng111-00 first", got)
	}
}
