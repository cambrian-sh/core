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
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
)

// The security-equivalence proof of interned fence classes (ADR-0149 D6;
// T-20261113-unfiltered-listobjects-group-list-under-a).
//
// The objects reader decides ADR-0128 D12 existence — "the caller may read the
// evidence that minted the entity, or sees at least one of its property
// values" — no longer per property per row, but as: one of the row's fence
// classes (canonical (tags, parties) pairs of its properties and of its minted
// evidence) is in V, the ids of the dictionary pairs that pass
// sqlfence.FenceClass, computed once. This test generates random predicates
// (drawPredicate: bypass, maintenance, owners, required / forbidden / any-of incl.
// empty clauses, party scoping with and without identities, own-rows) and random
// objects (properties with NULL, empty, unsorted and repeated tags and parties;
// minted evidence or none), and requires three verdicts to agree per object:
//
//  1. Go: TagPredicate.CheckRow over each classified property and the minted
//     evidence (the authoritative form);
//  2. the per-property SQL rule P4 shipped: EvidenceExists over the minted
//     evidence OR sqlfence.ObjectProperty per property, over the RAW companions;
//  3. the class rule: V through sqlfence.FenceClass over a dictionary of
//     CANONICAL pairs, and the row's class ids tested against V as the planner
//     renders it.
//
// (3) == (2) is the equivalence; (2) == (1) re-proves the old rule against Go.
//
// ADR-0158 D2 (stage 4): a property value also carries the row classification
// R of the observation it came from (<prop>__rowtags: NULL, empty or a tag set
// — including tags that would WIDEN access under a plain union), so the
// dictionary holds canonical (tags, parties, row_tags) triples. (1) becomes
// CheckRow(E,P) AND CheckRow(E∪R,P), (2) ObjectProperty(...).WithRowTags,
// (3) V through FenceClassRowTags. And, because the objects planner renders the
// plain FenceClass when no triple carries row tags (rowTagged), (4): over the
// triples with empty row tags, FenceClass and FenceClassRowTags admit the same
// ids — every iteration.

// classObjectProps is how many properties each generated object has.
const classObjectProps = 3

type classObject struct {
	id      string
	props   [classObjectProps]*fenceRow // nil = NULL tags (no classification)
	rowTags [classObjectProps][]string  // the value's row classification (ADR-0158)
	minted  string                      // evidence id, "" = not minted with evidence
}

// classTyped is ADR-0158 D2 over the Go predicate: CheckRow(E,P) AND
// CheckRow(E∪R,P).
func classTyped(p *domain.TagPredicate, tags, parties, rowTags []string) bool {
	if _, ok := p.CheckRow(tags, parties); !ok {
		return false
	}
	if len(rowTags) == 0 {
		return true
	}
	_, ok := p.CheckRow(append(append([]string{}, tags...), rowTags...), parties)
	return ok
}

// shuffledDup draws xs in a random order with a random element repeated: the
// shape a source can deliver, which canonicalisation must not change.
func shuffledDup(t *rapid.T, xs []string, label string) []string {
	if xs == nil {
		return nil
	}
	out := append([]string{}, xs...)
	if len(out) > 0 && rapid.Bool().Draw(t, label+":dup") {
		out = append(out, out[rapid.IntRange(0, len(out)-1).Draw(t, label+":dupi")])
	}
	for i := len(out) - 1; i > 0; i-- {
		j := rapid.IntRange(0, i).Draw(t, fmt.Sprintf("%s:sh%d", label, i))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// canonicalPair is the maintainer's canonical form, written out independently:
// sorted, de-duplicated, absent parties empty.
func canonicalPair(tags, parties []string) ([]string, []string) {
	set := func(xs []string) []string {
		m := map[string]bool{}
		for _, x := range xs {
			m[x] = true
		}
		out := make([]string, 0, len(m))
		for x := range m {
			out = append(out, x)
		}
		sort.Strings(out)
		return out
	}
	return set(tags), set(parties)
}

func TestPg_FenceClassDifferential(t *testing.T) {
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
	// Session-local TEMP tables; TEMP evidence shadows the migrated one, which
	// EvidenceExists names literally.
	cols := ""
	for i := 0; i < classObjectProps; i++ {
		cols += fmt.Sprintf(", p%d__tags text[], p%d__parties text[], p%d__rowtags text[]", i, i, i)
	}
	for _, ddl := range []string{
		`CREATE TEMP TABLE evidence (id text PRIMARY KEY, classification text[] NOT NULL, parties text[] NOT NULL)`,
		`CREATE TEMP TABLE class_dict (id integer PRIMARY KEY, tags text[] NOT NULL, parties text[] NOT NULL, row_tags text[] NOT NULL DEFAULT '{}', UNIQUE (tags, parties, row_tags))`,
		`CREATE TEMP TABLE class_objects (id text PRIMARY KEY, minted text, vis_classes integer[] NOT NULL` + cols + `)`,
	} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	defer func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS pg_temp.evidence, pg_temp.class_dict, pg_temp.class_objects`) // best-effort: the session ends with the test
	}()
	ids := func(q string, args []any) ([]string, error) {
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

	// The would-widen case, fixed (the random generator reaches it rarely): a
	// value on tags [a] whose row adds [hr], read by a caller who requires hr.
	// A plain union over (tags || row_tags) would admit it; D2 must not — per
	// property, through the dictionary, and in Go.
	{
		widen := &domain.TagPredicate{RequiredTags: []string{"hr"}}
		if classTyped(widen, []string{"a"}, []string{}, []string{"hr"}) {
			t.Fatal("Go: a row tag widened access")
		}
		if _, err := conn.Exec(ctx, `INSERT INTO class_dict VALUES (1, '{a}', '{}', '{hr}')`); err != nil {
			t.Fatal(err)
		}
		body, args := sqlfence.FenceClassRowTags("c").Expr(widen, 0)
		v, err := ids(`SELECT c.id::text FROM class_dict c WHERE `+body, args)
		if err != nil || len(v) != 0 {
			t.Fatalf("dictionary: a row tag widened access: %v %v", v, err)
		}
		pbody, pargs := sqlfence.ObjectProperty("x.tags", "x.parties").WithRowTags("x.row_tags").Expr(widen, 0)
		v, err = ids(`SELECT 'x' FROM (SELECT '{a}'::text[] AS tags, '{}'::text[] AS parties, '{hr}'::text[] AS row_tags) x WHERE `+pbody, pargs)
		if err != nil || len(v) != 0 {
			t.Fatalf("property: a row tag widened access: %v %v", v, err)
		}
		if _, err := conn.Exec(ctx, `TRUNCATE pg_temp.class_dict`); err != nil {
			t.Fatal(err)
		}
	}

	// Coverage: the proof only means something if objects exist through the
	// minted evidence alone, through a property alone, and are denied.
	var viaMintOnly, viaPropOnly, viaParty, denied, restrictedByRow, wouldWiden int
	rapid.Check(t, func(rt *rapid.T) {
		if _, err := conn.Exec(ctx, `TRUNCATE pg_temp.evidence, pg_temp.class_dict, pg_temp.class_objects`); err != nil {
			rt.Fatalf("truncate: %v", err)
		}
		// Evidence rows the objects can be minted by.
		evs := map[string]fenceRow{}
		var evIDs []string
		for i, n := 0, rapid.IntRange(0, 4).Draw(rt, "evidence"); i < n; i++ {
			r := fenceRow{id: fmt.Sprintf("ev%d", i), tags: subset(rt, diffTags, fmt.Sprintf("ev%d:tags", i)),
				parties: subset(rt, diffParties, fmt.Sprintf("ev%d:parties", i))}
			raw := shuffledDup(rt, r.tags, "ev"+r.id)
			if _, err := conn.Exec(ctx, `INSERT INTO evidence VALUES ($1,$2,$3)`, r.id, nonNil(raw), nonNil(shuffledDup(rt, r.parties, "evp"+r.id))); err != nil {
				rt.Fatalf("seed evidence: %v", err)
			}
			evs[r.id] = r
			evIDs = append(evIDs, r.id)
		}
		// The dictionary: one id per canonical triple, in order of first sight.
		dict := map[string]int32{}
		classOf := func(tags, parties, rowTags []string) int32 {
			ct, cp := canonicalPair(tags, parties)
			cr, _ := canonicalPair(rowTags, nil)
			key := strings.Join(ct, "\x00") + "\x01" + strings.Join(cp, "\x00") + "\x01" + strings.Join(cr, "\x00")
			if id, ok := dict[key]; ok {
				return id
			}
			id := int32(len(dict) + 1)
			dict[key] = id
			if _, err := conn.Exec(ctx, `INSERT INTO class_dict VALUES ($1,$2,$3,$4)`, id, ct, cp, cr); err != nil {
				rt.Fatalf("seed class: %v", err)
			}
			return id
		}
		var objs []classObject
		for i, n := 0, rapid.IntRange(1, 8).Draw(rt, "objects"); i < n; i++ {
			o := classObject{id: fmt.Sprintf("o%d", i)}
			args := []any{o.id, nil, nil}
			set := map[int32]bool{}
			for p := 0; p < classObjectProps; p++ {
				label := fmt.Sprintf("o%d:p%d", i, p)
				if rapid.IntRange(0, 3).Draw(rt, label+":null") == 0 {
					args = append(args, nil, nil, nil)
					continue
				}
				r := fenceRow{tags: nonNil(subset(rt, diffTags, label+":tags")), parties: nonNil(subset(rt, diffParties, label+":parties"))}
				o.props[p] = &r
				// The row classification: mostly none (NULL, as the maintainer
				// writes it, or '{}'), else a subset of the vocabulary.
				var rowCol any
				switch rapid.IntRange(0, 3).Draw(rt, label+":rowkind") {
				case 0:
				case 1:
					rowCol = []string{}
				default:
					// diffPlain: the kernel refuses owner:/provenance:/ingress:
					// tags in a row classification (domain.NormalizeRowClassification).
					o.rowTags[p] = subset(rt, diffPlain, label+":rowtags")
					rowCol = nonNil(shuffledDup(rt, o.rowTags[p], label+":rr"))
				}
				set[classOf(r.tags, r.parties, o.rowTags[p])] = true
				// The companions carry the value's raw arrays, as a source sent them.
				args = append(args, nonNil(shuffledDup(rt, r.tags, label+":rt")), nonNil(shuffledDup(rt, r.parties, label+":rp")), rowCol)
			}
			if len(evIDs) > 0 && rapid.Bool().Draw(rt, fmt.Sprintf("o%d:minted", i)) {
				o.minted = rapid.SampledFrom(evIDs).Draw(rt, fmt.Sprintf("o%d:ev", i))
				ev := evs[o.minted]
				set[classOf(ev.tags, ev.parties, nil)] = true
				args[1] = o.minted
			}
			vis := make([]int32, 0, len(set))
			for id := range set {
				vis = append(vis, id)
			}
			sort.Slice(vis, func(a, b int) bool { return vis[a] < vis[b] })
			args[2] = vis
			ph := make([]string, len(args))
			for k := range args {
				ph[k] = fmt.Sprintf("$%d", k+1)
			}
			if _, err := conn.Exec(ctx, `INSERT INTO class_objects VALUES (`+strings.Join(ph, ",")+`)`, args...); err != nil {
				rt.Fatalf("seed object: %v", err)
			}
			objs = append(objs, o)
		}
		p := drawPredicate(rt, "pred", 0)
		// drawPredicate mostly denies (it is tuned for the shapes' edge cases).
		// Half the time keep only the owner fence, forbidden tags and party
		// scoping, so admissions — through a party-scoped pair in particular —
		// are common enough to tell the rules apart.
		if !p.Unfiltered() && rapid.Bool().Draw(rt, "admitting") {
			p.RequiredTags, p.AnyOfClauses = nil, nil
			// Sometimes require a tag a row classification can supply: the
			// case a plain union over (tags || row_tags) would WIDEN (ADR-0158
			// alternative 1), which D2 must not.
			if rapid.IntRange(0, 2).Draw(rt, "widenProbe") == 0 {
				p.RequiredTags = []string{"hr"}
			}
			if len(p.PartyScopedTags) == 0 {
				p.PartyScopedTags = []string{"deal:x"}
			}
			if len(p.PartyIdentities) == 0 {
				p.PartyIdentities = []string{rapid.SampledFrom(diffParties).Draw(rt, "identity")}
			}
		}

		// (1) Go, authoritative.
		want := []string{}
		for _, o := range objs {
			byMint, byProp := false, false
			if o.minted != "" {
				_, byMint = p.CheckRow(evs[o.minted].tags, nonNil(evs[o.minted].parties))
			}
			for pi, r := range o.props {
				if r == nil {
					continue
				}
				_, plain := p.CheckRow(r.tags, r.parties)
				if !p.Unfiltered() && len(o.rowTags[pi]) > 0 {
					union := append(append([]string{}, r.tags...), o.rowTags[pi]...)
					_, widened := p.CheckRow(union, r.parties)
					switch {
					case plain && !classTyped(p, r.tags, r.parties, o.rowTags[pi]):
						restrictedByRow++
					case !plain && widened:
						wouldWiden++ // a plain union would have admitted it; D2 must not
					}
				}
				if allowed := classTyped(p, r.tags, r.parties, o.rowTags[pi]); allowed {
					byProp = true
					for _, pt := range p.PartyScopedTags {
						for _, tg := range r.tags {
							if tg == pt && !p.Unfiltered() {
								viaParty++
							}
						}
					}
				}
			}
			if !p.Unfiltered() {
				switch {
				case byMint && !byProp:
					viaMintOnly++
				case byProp && !byMint:
					viaPropOnly++
				case !byMint && !byProp:
					denied++
				}
			}
			if p.Unfiltered() || byMint || byProp {
				want = append(want, o.id)
			}
		}
		sort.Strings(want)

		if p.Unfiltered() {
			// The existence rule is skipped for ScopeMaintenance (needsExistence),
			// and the dictionary's fence renders nothing for it.
			if body, args := sqlfence.FenceClass("c").Expr(p, 0); body != "TRUE" || args != nil {
				rt.Fatalf("maintenance fence over the dictionary: %s %v", body, args)
			}
			return
		}

		// (2) The per-property rule as P4 rendered it, over the raw companions.
		var ors []string
		var oargs []any
		ev, eargs := sqlfence.EvidenceExists(p, "o.minted", 0)
		ors = append(ors, strings.TrimPrefix(ev, " AND "))
		oargs = append(oargs, eargs...)
		for i := 0; i < classObjectProps; i++ {
			body, args := sqlfence.ObjectProperty(fmt.Sprintf("o.p%d__tags", i), fmt.Sprintf("o.p%d__parties", i)).
				WithRowTags(fmt.Sprintf("o.p%d__rowtags", i)).Expr(p, len(oargs))
			ors = append(ors, body)
			oargs = append(oargs, args...)
		}
		q2 := `SELECT o.id FROM class_objects o WHERE (` + strings.Join(ors, " OR ") + `)`
		perProp, err := ids(q2, oargs)
		if err != nil {
			rt.Fatalf("per-property rule: %v\n%s", err, q2)
		}

		// (3) The class rule: V once through FenceClassRowTags, then the row test.
		body, vargs := sqlfence.FenceClassRowTags("c").Expr(p, 0)
		vq := `SELECT c.id FROM class_dict c WHERE ` + body
		vrows, err := conn.Query(ctx, vq, vargs...)
		if err != nil {
			rt.Fatalf("visible classes: %v\n%s", err, vq)
		}
		v := []int32{}
		for vrows.Next() {
			var id int32
			if err := vrows.Scan(&id); err != nil {
				rt.Fatal(err)
			}
			v = append(v, id)
		}
		vrows.Close()
		q3 := `SELECT o.id FROM class_objects o WHERE EXISTS (SELECT 1 FROM unnest(o.vis_classes) AS vc(id) WHERE vc.id = ANY($1::integer[]))`
		byClass, err := ids(q3, []any{v})
		if err != nil {
			rt.Fatalf("class rule: %v", err)
		}

		// (4) Without row tags the plain class fence is the same decision.
		plainBody, pargs := sqlfence.FenceClass("c").Expr(p, 0)
		v0, err := ids(`SELECT c.id::text FROM class_dict c WHERE cardinality(c.row_tags) = 0 AND `+plainBody, pargs)
		if err != nil {
			rt.Fatalf("plain visible classes: %v", err)
		}
		v1, err := ids(`SELECT c.id::text FROM class_dict c WHERE cardinality(c.row_tags) = 0 AND `+body, vargs)
		if err != nil {
			rt.Fatalf("row-tags visible classes: %v", err)
		}
		if strings.Join(v0, ",") != strings.Join(v1, ",") {
			rt.Fatalf("over triples with empty row tags FenceClass %v != FenceClassRowTags %v\npredicate: %+v", v0, v1, *p)
		}

		if strings.Join(perProp, ",") != strings.Join(want, ",") || strings.Join(byClass, ",") != strings.Join(want, ",") {
			rt.Fatalf("D12 existence diverges\npredicate: %+v\nown-rows: %+v\nobjects: %+v\nGo:            %v\nper-property:  %v\nclasses (V=%v): %v\nV sql: %s %#v",
				*p, p.OwnRows, describe(objs, evs), want, perProp, v, byClass, vq, vargs)
		}
	})
	t.Logf("objects existing only through minted evidence %d, only through a property %d; admitting pairs carrying a party-scoped tag %d; denied %d; values hidden by their row tags %d; values a plain union would have widened %d",
		viaMintOnly, viaPropOnly, viaParty, denied, restrictedByRow, wouldWiden)
	// wouldWiden is logged, not required: the fixed case above covers it.
	if viaMintOnly == 0 || viaPropOnly == 0 || viaParty == 0 || denied == 0 || restrictedByRow == 0 {
		t.Fatalf("the generator stopped covering a case: minted-only %d, property-only %d, party %d, denied %d, row-restricted %d",
			viaMintOnly, viaPropOnly, viaParty, denied, restrictedByRow)
	}
}

func describe(objs []classObject, evs map[string]fenceRow) []string {
	var out []string
	for _, o := range objs {
		var ps []string
		for _, r := range o.props {
			if r == nil {
				ps = append(ps, "NULL")
				continue
			}
			ps = append(ps, fmt.Sprintf("%v/%v/R%v", r.tags, r.parties, o.rowTags[len(ps)]))
		}
		m := ""
		if o.minted != "" {
			m = fmt.Sprintf(" minted %v/%v", evs[o.minted].tags, evs[o.minted].parties)
		}
		out = append(out, o.id+": "+strings.Join(ps, " ")+m)
	}
	return out
}
