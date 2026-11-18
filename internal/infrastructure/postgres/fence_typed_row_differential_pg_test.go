package postgres

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/rowfence"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
)

// The narrow-only differential of ADR-0158 D2 (stage 2,
// T-20261117-adr-0158-stage-2-typedrowexists-renderer), the typed-row companion
// of TestPg_FenceDifferential (same predicate generator, same temp-table method).
//
// A typed row stands on its evidence classification E with parties P and carries
// its own row classification R. For predicates (a fixed matrix, then
// drawPredicate: bypass, maintenance, owners, own-rows, required, forbidden,
// empty clauses, party scoping) × E, P, R, every row-tags rendering —
// sqlfence.TypedRowExists, ObjectProperty(...).WithRowTags (incl. a NULL row-tags
// companion), ObjectExtraRowTags and FenceClassRowTags — must satisfy:
//
//	(i)   SQL == Go rowfence.FilterRows(TypedRow) == CheckRow(E,P) && CheckRow(E∪R,P);
//	(ii)  never wider: every admitted row is admitted by the same table's fence
//	      without row tags (EvidenceExists / ObjectProperty / ObjectExtra / FenceClass);
//	(iii) R = ∅ (or NULL) ⇒ admitted exactly as without row tags.
//
// Coverage counters require that each case actually happened: a row the
// evidence fence admits that R hides through a forbidden (closed) tag; one it
// hides through a party-scoped tag the reader is not a party to; one a plain
// union fence(E ∪ R) would have WIDENED (R satisfies a required or any-of term
// E does not) that stays hidden; and empty-R rows. A seeded phase guarantees
// every kind by construction (ENG-2142: relying on random draws alone flaked
// with union-would-widen=0 after 10 runs); the random runs then explore around
// it with the same assertions. The fixed matrix pins each verdict as well.

// typedRowTags is the pool R is drawn from: plain, closed-by-predicate and
// party-scoped material, and an owner tag (refused at write by
// domain.NormalizeRowClassification, but the renderer must still be exact).
var typedRowTags = []string{"a", "b", "sales", "hr", "deal:x", "deal:y", "owner:alice"}

type typedRow struct {
	id      string
	tags    []string // E
	parties []string // P
	row     []string // R
	rowNull bool     // object companion: NULL __rowtags (extras: no "row_tags" key)
	tagNull bool     // object companion: NULL __tags (extras: no "tags" key) — unclassified
}

// typedVerdict is the authoritative pair for one row: today's rule (E alone) and
// the D2 rule. nullable: the rendering's table can carry an unclassified value.
func typedVerdict(p *domain.TagPredicate, r typedRow, nullable bool) (today, typed bool) {
	if p.Unfiltered() {
		return true, true
	}
	if nullable && r.tagNull {
		return false, false
	}
	_, today = p.CheckRow(r.tags, r.parties)
	_, both := p.CheckRow(append(append([]string{}, r.tags...), r.row...), r.parties)
	return today, today && both
}

func TestPg_TypedRowFenceDifferential(t *testing.T) {
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
	// TEMP evidence shadows the migrated table, which TypedRowExists names literally.
	const tables = `pg_temp.evidence, pg_temp.typed_ref, pg_temp.typed_objects, pg_temp.typed_extras, pg_temp.typed_classes`
	for _, ddl := range []string{
		`CREATE TEMP TABLE evidence (id text PRIMARY KEY, classification text[] NOT NULL, parties text[] NOT NULL)`,
		`CREATE TEMP TABLE typed_ref (id text PRIMARY KEY, evidence_id text NOT NULL, classification text[] NOT NULL)`,
		`CREATE TEMP TABLE typed_objects (id text PRIMARY KEY, p__tags text[], p__parties text[], p__rowtags text[])`,
		`CREATE TEMP TABLE typed_extras (id text PRIMARY KEY, entry jsonb NOT NULL)`,
		`CREATE TEMP TABLE typed_classes (id text PRIMARY KEY, tags text[] NOT NULL, parties text[] NOT NULL, row_tags text[] NOT NULL)`,
	} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	defer func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS `+tables) // best-effort: the session ends with the test anyway
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

	type rendering struct {
		name     string
		nullable bool
		typed    func(p *domain.TagPredicate) (string, []any) // full query, row tags
		base     func(p *domain.TagPredicate) (string, []any) // same table, no row tags
	}
	renderings := []rendering{
		{"typed row exists", false,
			func(p *domain.TagPredicate) (string, []any) {
				f, a := sqlfence.TypedRowExists(p, "r.evidence_id", "r.classification", 0)
				return `SELECT r.id FROM typed_ref r WHERE true` + f, a
			},
			func(p *domain.TagPredicate) (string, []any) {
				f, a := sqlfence.EvidenceExists(p, "r.evidence_id", 0)
				return `SELECT r.id FROM typed_ref r WHERE true` + f, a
			}},
		{"object property with row tags", true,
			func(p *domain.TagPredicate) (string, []any) {
				b, a := sqlfence.ObjectProperty("o.p__tags", "o.p__parties").WithRowTags("o.p__rowtags").Expr(p, 0)
				return `SELECT o.id FROM typed_objects o WHERE ` + b, a
			},
			func(p *domain.TagPredicate) (string, []any) {
				b, a := sqlfence.ObjectProperty("o.p__tags", "o.p__parties").Expr(p, 0)
				return `SELECT o.id FROM typed_objects o WHERE ` + b, a
			}},
		{"object extra with row tags", true,
			func(p *domain.TagPredicate) (string, []any) {
				b, a := sqlfence.ObjectExtraRowTags("x.entry").Expr(p, 0)
				return `SELECT x.id FROM typed_extras x WHERE ` + b, a
			},
			func(p *domain.TagPredicate) (string, []any) {
				b, a := sqlfence.ObjectExtra("x.entry").Expr(p, 0)
				return `SELECT x.id FROM typed_extras x WHERE ` + b, a
			}},
		{"fence class triple", false,
			func(p *domain.TagPredicate) (string, []any) {
				b, a := sqlfence.FenceClassRowTags("c").Expr(p, 0)
				return `SELECT c.id FROM typed_classes c WHERE ` + b, a
			},
			func(p *domain.TagPredicate) (string, []any) {
				b, a := sqlfence.FenceClass("c").Expr(p, 0)
				return `SELECT c.id FROM typed_classes c WHERE ` + b, a
			}},
	}

	var admitted int
	// verify seeds rows, runs every rendering for p and asserts (i)-(iii); it
	// returns the ids each rendering admits. fatalf is t.Fatalf or rt.Fatalf.
	verify := func(fatalf func(string, ...any), p *domain.TagPredicate, rows []typedRow) map[string][]string {
		if _, err := conn.Exec(ctx, `TRUNCATE `+tables); err != nil {
			fatalf("truncate: %v", err)
		}
		for _, r := range rows {
			tags, parties, rowTags := nonNil(r.tags), nonNil(r.parties), nonNil(r.row)
			objArgs := []any{r.id, tags, parties, rowTags}
			if r.tagNull {
				objArgs[1], objArgs[2] = nil, nil
			}
			if r.rowNull {
				objArgs[3] = nil
			}
			entry := map[string]any{"t": "text", "v": "x", "id": r.id}
			if !r.tagNull {
				entry["tags"] = tags
			}
			if len(r.parties) > 0 {
				entry["parties"] = r.parties
			}
			if !r.rowNull {
				entry["row_tags"] = rowTags // an explicit [] is "no row classification" too
			}
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO evidence VALUES ($1,$2,$3)`, []any{r.id, tags, parties}},
				{`INSERT INTO typed_ref VALUES ($1,$1,$2)`, []any{r.id, rowTags}},
				{`INSERT INTO typed_objects VALUES ($1,$2,$3,$4)`, objArgs},
				{`INSERT INTO typed_extras VALUES ($1,$2)`, []any{r.id, entry}},
				{`INSERT INTO typed_classes VALUES ($1,$2,$3,$4)`, []any{r.id, tags, parties, rowTags}},
			} {
				if _, err := conn.Exec(ctx, q.sql, q.args...); err != nil {
					fatalf("seed %s: %v", r.id, err)
				}
			}
		}
		// Go: rowfence over TypedRow, the form the walks will use (stage 3).
		goAdmits := func(nullable bool) []string {
			kept := rowfence.FilterRows(p, rows, func(r typedRow) ([]rowfence.Classification, bool) {
				if nullable && r.tagNull {
					return nil, false
				}
				return rowfence.TypedRow(rowfence.Classification{Tags: r.tags, Parties: r.parties}, r.row), true
			})
			out := []string{}
			for _, r := range kept {
				out = append(out, r.id)
			}
			sort.Strings(out)
			return out
		}
		out := map[string][]string{}
		for _, c := range renderings {
			tq, ta := c.typed(p)
			got, err := query(tq, ta)
			if err != nil {
				fatalf("%s: %v\n%s", c.name, err, tq)
			}
			bq, ba := c.base(p)
			base, err := query(bq, ba)
			if err != nil {
				fatalf("%s (base): %v\n%s", c.name, err, bq)
			}
			want, wantToday := []string{}, []string{}
			for _, r := range rows {
				today, typed := typedVerdict(p, r, c.nullable)
				if typed {
					want = append(want, r.id)
				}
				if today {
					wantToday = append(wantToday, r.id)
				}
			}
			sort.Strings(want)
			sort.Strings(wantToday)
			fail := func(what string) {
				fatalf("%s: %s\npredicate: %+v\nown-rows: %+v\nrows: %+v\nSQL typed: %v\nSQL base:  %v\nGo typed:  %v\nGo today:  %v\nsql: %s\nargs: %#v",
					c.name, what, *p, p.OwnRows, rows, got, base, want, wantToday, tq, ta)
			}
			// (i) SQL == CheckRow conjunction == rowfence(TypedRow).
			if strings.Join(got, ",") != strings.Join(want, ",") {
				fail("SQL diverges from CheckRow(E,P) && CheckRow(E∪R,P)")
			}
			if g := goAdmits(c.nullable); strings.Join(g, ",") != strings.Join(want, ",") {
				fail(fmt.Sprintf("rowfence.FilterRows(TypedRow) diverges: %v", g))
			}
			// The base rendering is today's rule (re-proved, so (ii)/(iii) compare to truth).
			if strings.Join(base, ",") != strings.Join(wantToday, ",") {
				fail("base rendering diverges from CheckRow(E,P)")
			}
			inBase := map[string]bool{}
			for _, id := range base {
				inBase[id] = true
			}
			inGot := map[string]bool{}
			for _, id := range got {
				inGot[id] = true
				// (ii) never wider.
				if !inBase[id] {
					fail("row tags WIDENED: " + id + " admitted with R but not without")
				}
			}
			for _, r := range rows {
				// (iii) R = ∅ (or NULL) reads exactly as today.
				if len(r.row) == 0 && inGot[r.id] != inBase[r.id] {
					fail("R = ∅ changed the verdict for " + r.id)
				}
			}
			out[c.name] = got
			admitted += len(got)
		}
		return out
	}

	// Phase 1 — the fixed matrix: each case pinned on every rendering.
	matrixRows := []typedRow{
		{id: "plain", tags: []string{"internal"}},
		{id: "plain-null-r", tags: []string{"internal"}, rowNull: true},
		{id: "closed", tags: []string{"internal"}, row: []string{"hr"}},
		{id: "party-mine", tags: []string{"internal"}, parties: []string{"p1"}, row: []string{"deal:x"}},
		{id: "party-other", tags: []string{"internal"}, parties: []string{"p2"}, row: []string{"deal:x"}},
		{id: "widen-anyof", tags: []string{"internal"}, row: []string{"finance"}},
		{id: "widen-required", tags: []string{"internal"}, row: []string{"sales"}},
		{id: "inert-open", tags: []string{"internal", "finance", "sales"}, row: []string{"misc"}},
	}
	for _, m := range []struct {
		name string
		p    *domain.TagPredicate
		want string // ids the typed renderings admit, sorted
	}{
		{"closed hr, party deal:x for p1",
			&domain.TagPredicate{ForbiddenTags: []string{"hr"}, PartyScopedTags: []string{"deal:x"}, PartyIdentities: []string{"p1"}},
			"inert-open,party-mine,plain,plain-null-r,widen-anyof,widen-required"},
		{"any-of finance: R must not satisfy it",
			&domain.TagPredicate{AnyOfClauses: [][]string{{"finance"}}}, "inert-open"},
		{"required sales: R must not satisfy it",
			&domain.TagPredicate{RequiredTags: []string{"sales"}}, "inert-open"},
		{"party deal:x, no identities",
			&domain.TagPredicate{PartyScopedTags: []string{"deal:x"}},
			"closed,inert-open,plain,plain-null-r,widen-anyof,widen-required"},
		{"bypass sees R-restricted rows unless the owner fence denies",
			domain.ScopeSystem,
			"closed,inert-open,party-mine,party-other,plain,plain-null-r,widen-anyof,widen-required"},
		{"maintenance",
			domain.ScopeMaintenance,
			"closed,inert-open,party-mine,party-other,plain,plain-null-r,widen-anyof,widen-required"},
	} {
		got := verify(t.Fatalf, m.p, matrixRows)
		for _, c := range renderings {
			if strings.Join(got[c.name], ",") != m.want {
				t.Fatalf("matrix %q on %s: got %v, want %s", m.name, c.name, got[c.name], m.want)
			}
		}
	}

	// coverageKinds classifies one row against p over the evidence rendering's
	// verdicts — the same rule for the seeded phase and the random phase.
	type coverageKinds struct{ closed, party, widen, empty bool }
	classify := func(p *domain.TagPredicate, r typedRow) coverageKinds {
		var k coverageKinds
		if p.Unfiltered() {
			return k
		}
		if len(r.row) == 0 {
			k.empty = true
			return k
		}
		today, typed := typedVerdict(p, r, false)
		if _, union := p.CheckRow(append(append([]string{}, r.tags...), r.row...), r.parties); union && !today {
			k.widen = true
		}
		if !today || typed {
			return k
		}
		for _, tag := range r.row {
			if containsString(p.ForbiddenTags, tag) {
				k.closed = true
				break
			}
		}
		for _, tag := range r.row {
			if containsString(p.PartyScopedTags, tag) && !overlaps(r.parties, p.PartyIdentities) {
				k.party = true
				break
			}
		}
		return k
	}
	var byClosed, byParty, widenTrap, emptyR int
	count := func(p *domain.TagPredicate, rows []typedRow) {
		for _, r := range rows {
			k := classify(p, r)
			if k.closed {
				byClosed++
			}
			if k.party {
				byParty++
			}
			if k.widen {
				widenTrap++
			}
			if k.empty {
				emptyR++
			}
		}
	}

	// Phase 2 — seeded coverage (ENG-2142). Every coverage kind is guaranteed by
	// construction, so the differential can never be vacuous by bad luck: the
	// random phase alone sometimes drew no union-would-widen row in 10 runs
	// (rows must line up with the predicate's required / any-of terms). Each
	// seed row declares the kind it must be and is checked with the same
	// classifier, so a seed that stops being its kind fails loudly instead of
	// silently thinning coverage. The predicates carry owner, forbidden,
	// required / any-of and party terms together, as drawn ones do.
	type seedRow struct {
		row  typedRow
		kind string // closed | party | widen | empty | control
	}
	for _, s := range []struct {
		name string
		p    *domain.TagPredicate
		rows []seedRow
	}{
		{"owner alice, required sales, closed hr, party deal:x for p1",
			&domain.TagPredicate{Owners: []string{"alice"}, RequiredTags: []string{"sales"}, ForbiddenTags: []string{"hr"},
				PartyScopedTags: []string{"deal:x"}, PartyIdentities: []string{"p1"}},
			[]seedRow{
				{typedRow{id: "s-widen", tags: []string{"a", "owner:alice"}, row: []string{"sales"}}, "widen"},
				{typedRow{id: "s-closed", tags: []string{"sales"}, row: []string{"hr"}}, "closed"},
				{typedRow{id: "s-party", tags: []string{"sales", "owner:alice"}, parties: []string{"p2"}, row: []string{"deal:x"}}, "party"},
				{typedRow{id: "s-empty", tags: []string{"sales"}}, "empty"},
				{typedRow{id: "s-empty-null", tags: []string{"sales", "owner:alice"}, rowNull: true}, "empty"},
				{typedRow{id: "s-party-mine", tags: []string{"sales"}, parties: []string{"p1"}, row: []string{"deal:x"}}, "control"},
				{typedRow{id: "s-foreign", tags: []string{"sales", "owner:bob"}, row: []string{"a"}}, "control"},
			}},
		{"any-of {b, deal:y}, closed a, party deal:y for p2",
			&domain.TagPredicate{AnyOfClauses: [][]string{{"b", "deal:y"}}, ForbiddenTags: []string{"a"},
				PartyScopedTags: []string{"deal:y"}, PartyIdentities: []string{"p2"}},
			[]seedRow{
				{typedRow{id: "s-widen", tags: []string{"sales"}, row: []string{"b"}}, "widen"},
				{typedRow{id: "s-closed", tags: []string{"b"}, row: []string{"a"}}, "closed"},
				{typedRow{id: "s-party", tags: []string{"b"}, parties: []string{"p1"}, row: []string{"deal:y"}}, "party"},
				{typedRow{id: "s-empty", tags: []string{"b"}, row: []string{}}, "empty"},
				{typedRow{id: "s-inert", tags: []string{"b", "sales"}, row: []string{"hr"}}, "control"},
			}},
	} {
		rows := make([]typedRow, 0, len(s.rows))
		for _, sr := range s.rows {
			rows = append(rows, sr.row)
		}
		verify(t.Fatalf, s.p, rows)
		for _, sr := range s.rows {
			k := classify(s.p, sr.row)
			got := map[string]bool{"closed": k.closed, "party": k.party, "widen": k.widen, "empty": k.empty,
				"control": !k.closed && !k.party && !k.widen && !k.empty}
			if !got[sr.kind] {
				t.Fatalf("seed %q row %s: not a %s row (classified %+v)", s.name, sr.row.id, sr.kind, k)
			}
		}
		count(s.p, rows)
	}
	seeded := [4]int{byClosed, byParty, widenTrap, emptyR}

	// Phase 3 — random predicates and rows.
	prop := func(rt *rapid.T) {
		// The predicate first, so R can be drawn from its own material: a forbidden
		// tag (the closed-tag restriction), a party-scoped tag (the party
		// restriction) and a required / any-of tag (the widening trap a plain
		// union would fall into — the rule must NOT admit through it).
		p := drawPredicate(rt, "pred", 0)
		material := append(append(append([]string{}, p.ForbiddenTags...), p.PartyScopedTags...), p.RequiredTags...)
		for _, clause := range p.AnyOfClauses {
			material = append(material, clause...)
		}
		var rows []typedRow
		for i, n := 0, rapid.IntRange(1, 10).Draw(rt, "rows"); i < n; i++ {
			r := typedRow{
				id:      fmt.Sprintf("r%d", i),
				tags:    subset(rt, diffTags, fmt.Sprintf("row%d:tags", i)),
				parties: subset(rt, diffParties, fmt.Sprintf("row%d:parties", i)),
			}
			if rapid.IntRange(0, 2).Draw(rt, fmt.Sprintf("row%d:has-r", i)) != 0 {
				r.row = subset(rt, typedRowTags, fmt.Sprintf("row%d:row", i))
				if len(material) > 0 && rapid.Bool().Draw(rt, fmt.Sprintf("row%d:row-from-pred", i)) {
					r.row = append(r.row, rapid.SampledFrom(material).Draw(rt, fmt.Sprintf("row%d:row-pred-tag", i)))
				}
			}
			r.rowNull = len(r.row) == 0 && rapid.Bool().Draw(rt, fmt.Sprintf("row%d:row-null", i))
			r.tagNull = rapid.IntRange(0, 5).Draw(rt, fmt.Sprintf("row%d:tag-null", i)) == 0
			rows = append(rows, r)
		}
		verify(rt.Fatalf, p, rows)
		count(p, rows)
	}
	// One rapid run always; further runs (bounded) while the random phase has not
	// yet met every coverage kind on its own — more exploration, never a failure
	// condition: the seeded phase already guarantees each kind. Every run asserts
	// everything (i)-(iii).
	randomSeen := func() bool {
		return byClosed > seeded[0] && byParty > seeded[1] && widenTrap > seeded[2] && emptyR > seeded[3]
	}
	rounds := 0
	for rounds < 10 && (rounds == 0 || !randomSeen()) {
		rapid.Check(t, prop)
		rounds++
	}
	if admitted == 0 || byClosed == 0 || byParty == 0 || widenTrap == 0 || emptyR == 0 {
		t.Fatalf("vacuous differential after %d runs: admitted=%d restricted-by-closed-tag=%d restricted-by-party-tag=%d union-would-widen=%d empty-R rows=%d",
			rounds, admitted, byClosed, byParty, widenTrap, emptyR)
	}
	t.Logf("typed-row differential (seeded + %d random runs): %d admissions; R restricted %d rows by a closed tag (%d random), %d by a party-scoped tag (%d random); %d rows a plain union would have widened stayed hidden (%d random); %d empty-R rows read as today (%d random)",
		rounds, admitted, byClosed, byClosed-seeded[0], byParty, byParty-seeded[1], widenTrap, widenTrap-seeded[2], emptyR, emptyR-seeded[3])
}

func containsString(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if containsString(b, x) {
			return true
		}
	}
	return false
}

// fenceTypedSQLGolden pins the exact SQL and args of every ADR-0158 row-tags
// rendering per characterization predicate — the text stage 3-5 read sites will
// send. Each typed rendering embeds its base rendering's terms first and
// unchanged (TestFence_WithRowTagsIsBaseAndNarrowed); the base renderings
// themselves stay pinned, byte-identical, by fence_render_sql.golden.
// Regenerate only with FENCE_UPDATE=1.
const fenceTypedSQLGolden = "testdata/fence_render_typed_sql.golden"

func TestFenceRender_TypedSQLGolden(t *testing.T) {
	t.Parallel()
	renders := []struct {
		name   string
		render func(p *domain.TagPredicate) (string, []any)
	}{
		{"typed row exists (o.evidence_id, o.classification)", func(p *domain.TagPredicate) (string, []any) {
			return sqlfence.TypedRowExists(p, "o.evidence_id", "o.classification", 2)
		}},
		{"object property with row tags", func(p *domain.TagPredicate) (string, []any) {
			return sqlfence.ObjectProperty("o.p__tags", "o.p__parties").WithRowTags("o.p__rowtags").Expr(p, 2)
		}},
		{"object extra with row tags", func(p *domain.TagPredicate) (string, []any) {
			return sqlfence.ObjectExtraRowTags("x.value").Expr(p, 2)
		}},
		{"fence class triple", func(p *domain.TagPredicate) (string, []any) {
			return sqlfence.FenceClassRowTags("c").Expr(p, 2)
		}},
		// ENG-2049: the link fence — typed row exists over the cited evidence,
		// then every supporting evidence readable.
		{"link exists (l)", func(p *domain.TagPredicate) (string, []any) {
			return sqlfence.LinkExists(p, "l", 2)
		}},
	}
	var b strings.Builder
	for _, fp := range fencePredicates() {
		fmt.Fprintf(&b, "## %s\n", fp.name)
		for _, r := range renders {
			fmt.Fprintf(&b, "- %s\n", r.name)
			sql, args, panicked := func() (sql string, args []any, panicked bool) {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				sql, args = r.render(fp.p)
				return sql, args, false
			}()
			if panicked {
				b.WriteString("  PANIC\n")
				continue
			}
			fmt.Fprintf(&b, "  sql:  %q\n", sql)
			fmt.Fprintf(&b, "  args: %#v\n", args)
		}
		b.WriteString("\n")
	}
	compareGolden(t, fenceTypedSQLGolden, b.String())
}
