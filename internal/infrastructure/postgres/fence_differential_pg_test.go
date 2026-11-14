package postgres

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/doug-martin/goqu/v9"
	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
)

// The differential guard against Go/SQL drift in the access predicate
// (T-20261113-sql-fence-mirrors-skip-an-empty-any-of-clause; ADR-0149 D8).
//
// TagPredicate.Check — CheckRow where the row carries parties — is the
// authoritative form; internal/authz/sqlfence is its one SQL mirror. This test
// generates random predicates (bypass, maintenance, AllOwners, the owner fence,
// required / forbidden / any-of incl. EMPTY clauses, party scoping with and
// without identities, own-rows terms valid and malformed) and random rows, runs
// every rendering the characterization test pins against real Postgres temp
// tables, and asserts each admits exactly the rows Go admits.
//
// The characterization goldens pin a fixed matrix; this explores around it. A
// failure here is a real divergence: rapid prints the minimal predicate and rows.
//
// nil is not generated: Go denies every row for nil while the non-evidence shapes
// render no fence, by design — the fail-closed nil decision belongs to the
// chokepoint (EnforcingVectorStore refuses with ErrScopeMissing before SQL), and
// the evidence shape panics. TestFence_UnfilteredAndNil pins that.

var (
	diffTags     = []string{"a", "b", "sales", "hr", "deal:x", "deal:y", "owner:alice", "owner:bob", domain.OwnerTagPrefix}
	diffPlain    = []string{"a", "b", "sales", "hr", "deal:x", "deal:y"}
	diffParties  = []string{"p1", "p2"}
	diffOwnerIDs = []string{"alice", "bob", ""}
)

// subset draws a subset of from (order as drawn; may be empty, and nil when empty
// half the time, so nil-vs-empty slices are both exercised).
func subset(t *rapid.T, from []string, label string) []string {
	var out []string
	for _, x := range from {
		if rapid.Bool().Draw(t, label+":"+x) {
			out = append(out, x)
		}
	}
	if out == nil && rapid.Bool().Draw(t, label+":empty-not-nil") {
		out = []string{}
	}
	return out
}

func drawPredicate(t *rapid.T, label string, depth int) *domain.TagPredicate {
	p := &domain.TagPredicate{}
	switch rapid.IntRange(0, 9).Draw(t, label+":kind") {
	case 0:
		return &domain.TagPredicate{Bypass: true, AllOwners: true} // ScopeMaintenance
	case 1:
		p.Bypass = true
	}
	p.AllOwners = rapid.IntRange(0, 9).Draw(t, label+":allowners") == 0
	p.Owners = subset(t, diffOwnerIDs, label+":owners")
	p.RequiredTags = subset(t, diffPlain, label+":required")
	if rapid.IntRange(0, 2).Draw(t, label+":required-rare") != 0 {
		p.RequiredTags = nil // keep most predicates satisfiable enough to admit rows
	}
	p.ForbiddenTags = subset(t, diffPlain, label+":forbidden")
	nClauses := rapid.IntRange(0, 3).Draw(t, label+":clauses")
	for i := 0; i < nClauses; i++ {
		// An empty clause — the case this test was written for — is drawn on purpose
		// a quarter of the time; a subset that happens to come out empty is rarer
		// than that and would leave the case mostly unexplored.
		clause := []string{}
		if rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("%s:clause%d:empty", label, i)) != 0 {
			clause = append(clause, subset(t, diffPlain, fmt.Sprintf("%s:clause%d", label, i))...)
		}
		p.AnyOfClauses = append(p.AnyOfClauses, clause)
	}
	p.PartyScopedTags = subset(t, []string{"deal:x", "deal:y", "a"}, label+":partyscoped")
	p.PartyIdentities = subset(t, diffParties, label+":identities")
	if depth == 0 && rapid.Bool().Draw(t, label+":has-ownrows") {
		in := drawPredicate(t, label+":own", depth+1)
		// Mostly a well-formed term (its owners = the outer's single owner), so the
		// own-rows disjunction is actually exercised; otherwise as drawn (malformed
		// ones must fall back to the main predicate in both forms).
		if rapid.IntRange(0, 3).Draw(t, label+":own-wellformed") != 0 {
			if len(p.Owners) != 1 {
				p.Owners = []string{rapid.SampledFrom(diffOwnerIDs[:2]).Draw(t, label+":own-owner")}
			}
			in.Owners = []string{p.Owners[0]}
			in.Bypass, in.AllOwners = false, false
		}
		if rapid.IntRange(0, 9).Draw(t, label+":own-ledger") == 0 {
			in.LedgerRuns = &domain.LedgerRunGrant{Roots: []string{"r"}}
		}
		p.OwnRows = in
	}
	return p
}

func TestPg_FenceDifferential(t *testing.T) {
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
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	// Same session-local TEMP tables as the characterization test (TEMP shadows the
	// migrated `evidence`, which the EXISTS rendering names literally).
	for _, ddl := range []string{
		`CREATE TEMP TABLE evidence (id text PRIMARY KEY, classification text[] NOT NULL, parties text[] NOT NULL)`,
		`CREATE TEMP TABLE fence_ref (id text PRIMARY KEY, evidence_id text NOT NULL)`,
		`CREATE TEMP TABLE fence_items (id text PRIMARY KEY, classification text[] NOT NULL)`,
		`CREATE TEMP TABLE fence_docs (id text PRIMARY KEY, metadata jsonb NOT NULL)`,
		// ADR-0149 P4: an object table property's companions; tags may be NULL.
		`CREATE TEMP TABLE fence_objects (id text PRIMARY KEY, p__tags text[], p__parties text[])`,
		// An open kind's undeclared property: one extra jsonb entry per row, with its
		// own tags/parties; no "tags" key = unclassified (ENG-1109).
		`CREATE TEMP TABLE fence_extras (id text PRIMARY KEY, entry jsonb NOT NULL)`,
	} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	defer func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS pg_temp.evidence, pg_temp.fence_ref, pg_temp.fence_items, pg_temp.fence_docs, pg_temp.fence_objects, pg_temp.fence_extras`) // best-effort: the session ends with the test anyway
	}()

	query := func(q string, args []any) ([]string, error) {
		rows, err := conn.Query(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		sort.Strings(out)
		return out, rows.Err()
	}

	rapid.Check(t, func(rt *rapid.T) {
		if _, err := conn.Exec(ctx, `TRUNCATE pg_temp.evidence, pg_temp.fence_ref, pg_temp.fence_items, pg_temp.fence_docs, pg_temp.fence_objects, pg_temp.fence_extras`); err != nil {
			rt.Fatalf("truncate: %v", err)
		}
		var rows []fenceRow
		objectNull := map[string]bool{}
		for i, n := 0, rapid.IntRange(1, 10).Draw(rt, "rows"); i < n; i++ {
			r := fenceRow{
				id:      fmt.Sprintf("r%d", i),
				tags:    subset(rt, diffTags, fmt.Sprintf("row%d:tags", i)),
				parties: subset(rt, diffParties, fmt.Sprintf("row%d:parties", i)),
			}
			rows = append(rows, r)
			tags := nonNil(r.tags)
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO evidence VALUES ($1,$2,$3)`, []any{r.id, tags, nonNil(r.parties)}},
				{`INSERT INTO fence_ref VALUES ($1,$1)`, []any{r.id}},
				{`INSERT INTO fence_items VALUES ($1,$2)`, []any{r.id, tags}},
				{`INSERT INTO fence_docs VALUES ($1,$2)`, []any{r.id, map[string]any{"tags": tags}}},
				{`INSERT INTO fence_objects VALUES ($1,$2,$3)`, objectRowArgs(rt, r, objectNull)},
				{`INSERT INTO fence_extras VALUES ($1,$2)`, []any{r.id, extraEntry(r, objectNull)}},
			} {
				if _, err := conn.Exec(ctx, q.sql, q.args...); err != nil {
					rt.Fatalf("seed %s: %v", r.id, err)
				}
			}
		}
		p := drawPredicate(rt, "pred", 0)

		// ADR-0149 P4: the object-property shape is CheckRow's mirror over the
		// companions, and a NULL tags column is visible only to ScopeMaintenance.
		{
			body, fargs := sqlfence.ObjectProperty("p__tags", "p__parties").Expr(p, 0)
			q := `SELECT id FROM fence_objects WHERE ` + body
			got, err := query(q, fargs)
			if err != nil {
				rt.Fatalf("object property: %v\n%s", err, q)
			}
			want := []string{}
			for _, row := range rows {
				ok := p.Unfiltered()
				if !ok && !objectNull[row.id] {
					_, ok = p.CheckRow(row.tags, row.parties)
				}
				if ok {
					want = append(want, row.id)
				}
			}
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				rt.Fatalf("Go/SQL divergence on object property\npredicate: %+v\nrows: %+v nulls: %v\nSQL admits: %v\nGo admits:  %v\nsql: %s\nargs: %#v",
					*p, rows, objectNull, got, want, q, fargs)
			}
		}

		// ENG-1109: an extra entry is fenced exactly like a declared property over its
		// own tags/parties; an entry without tags (the null rows) is unclassified.
		{
			body, fargs := sqlfence.ObjectExtra("entry").Expr(p, 0)
			q := `SELECT id FROM fence_extras WHERE ` + body
			got, err := query(q, fargs)
			if err != nil {
				rt.Fatalf("object extra: %v\n%s", err, q)
			}
			want := []string{}
			for _, row := range rows {
				ok := p.Unfiltered()
				if !ok && !objectNull[row.id] {
					_, ok = p.CheckRow(row.tags, row.parties)
				}
				if ok {
					want = append(want, row.id)
				}
			}
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				rt.Fatalf("Go/SQL divergence on object extra\npredicate: %+v\nrows: %+v nulls: %v\nSQL admits: %v\nGo admits:  %v\nsql: %s\nargs: %#v",
					*p, rows, objectNull, got, want, q, fargs)
			}
		}

		for _, r := range fenceRenderers() {
			var q string
			var args []any
			if r.rowsSQL == nil { // a goqu rendering, as the characterization test builds it
				var err error
				if strings.Contains(r.name, "alias c") {
					q, args, err = dialect.From(goqu.T("fence_docs").As("c")).Select(goqu.I("c.id")).
						Where(jsonbFenceForTest(p, "c")...).Prepared(true).ToSQL()
				} else {
					q, args, err = dialect.From("fence_docs").Select("id").
						Where(jsonbFenceForTest(p, "")...).Prepared(true).ToSQL()
				}
				if err != nil {
					rt.Fatalf("ToSQL: %v", err)
				}
			} else {
				frag, fargs := r.render(p, 0)
				q, args = r.rowsSQL(frag), fargs
			}
			got, err := query(q, args)
			if err != nil {
				rt.Fatalf("%s: %v\n%s", r.name, err, q)
			}
			want := []string{}
			for _, row := range rows {
				var ok bool
				if r.rowAware {
					_, ok = p.CheckRow(row.tags, row.parties)
				} else {
					_, ok = p.Check(row.tags)
				}
				if ok {
					want = append(want, row.id)
				}
			}
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				rt.Fatalf("Go/SQL divergence on %s\npredicate: %+v\nown-rows: %+v\nrows: %+v\nSQL admits: %v\nGo admits:  %v\nsql: %s\nargs: %#v",
					r.name, *p, p.OwnRows, rows, got, want, q, args)
			}
		}
	})
}

// objectRowArgs seeds one fence_objects row for r: its tags and parties, or —
// drawn — NULL tags (a value with no classification), recorded in null.
// extraEntry is the row as an extra jsonb entry, the shape objects.upsertRow
// writes: tags omitted when unclassified (the null rows), parties omitted when none.
func extraEntry(r fenceRow, null map[string]bool) map[string]any {
	e := map[string]any{"t": "text", "v": "x", "src": "s", "at": "", "id": r.id, "conflict": false}
	if !null[r.id] {
		e["tags"] = nonNil(r.tags)
	}
	if len(r.parties) > 0 {
		e["parties"] = r.parties
	}
	return e
}

func objectRowArgs(t *rapid.T, r fenceRow, null map[string]bool) []any {
	if rapid.Bool().Draw(t, r.id+":null-tags") {
		null[r.id] = true
		return []any{r.id, nil, nil}
	}
	return []any{r.id, nonNil(r.tags), nonNil(r.parties)}
}
