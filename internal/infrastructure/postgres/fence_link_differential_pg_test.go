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

// The link fence differential (ENG-2049, owner decision 2026-11-18, option a),
// the companion of TestPg_TypedRowFenceDifferential: for random predicates
// (drawPredicate) × links that cite evidence E (parties P) with row
// classification R and rest on 0-3 supporting evidence rows (each with its own
// classification and parties, or missing), sqlfence.LinkExists must satisfy
//
//	(i)   SQL == Go rowfence.FilterRows over linkRefs' classifications (the walk
//	      guard) == CheckRow(E,P) && CheckRow(E∪R,P) && every supporting row
//	      exists and CheckRow(E_s,P_s);
//	(ii)  never wider than TypedRowExists (the link fence before ENG-2049);
//	(iii) an empty supporting set ⇒ admitted exactly as TypedRowExists admits.
//
// Coverage counters require that a link the cited fence admits was hidden by a
// supporting row's closed tag, by its party list, and by a missing row.

type supportEv struct {
	id      string
	tags    []string
	parties []string
	missing bool
}

type diffLink struct {
	id      string
	cited   supportEv
	row     []string
	support []supportEv
}

func linkVerdict(p *domain.TagPredicate, l diffLink) (typed, link bool) {
	if p.Unfiltered() {
		return true, true
	}
	_, a := p.CheckRow(l.cited.tags, l.cited.parties)
	_, b := p.CheckRow(append(append([]string{}, l.cited.tags...), l.row...), l.cited.parties)
	typed = a && b
	link = typed
	for _, s := range l.support {
		if s.missing {
			link = false
			continue
		}
		if _, ok := p.CheckRow(s.tags, s.parties); !ok {
			link = false
		}
	}
	return typed, link
}

func TestPg_LinkFenceDifferential(t *testing.T) {
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
	// TEMP evidence shadows the migrated table, which LinkExists names literally.
	const tables = `pg_temp.evidence, pg_temp.diff_links`
	for _, ddl := range []string{
		`CREATE TEMP TABLE evidence (id text PRIMARY KEY, classification text[] NOT NULL, parties text[] NOT NULL)`,
		`CREATE TEMP TABLE diff_links (id text PRIMARY KEY, evidence_id text NOT NULL, classification text[] NOT NULL,
			supporting_evidence text[] NOT NULL)`,
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

	var bySupportClosed, bySupportParty, bySupportMissing, emptySupport int
	verify := func(fatalf func(string, ...any), p *domain.TagPredicate, links []diffLink) {
		if _, err := conn.Exec(ctx, `TRUNCATE `+tables); err != nil {
			fatalf("truncate: %v", err)
		}
		tags := map[string]rowfence.Classification{}
		for _, l := range links {
			evs := append([]supportEv{l.cited}, l.support...)
			var supp []string
			for i, s := range evs {
				if i > 0 {
					supp = append(supp, s.id)
				}
				if s.missing {
					continue
				}
				if _, dup := tags[s.id]; dup {
					continue
				}
				tags[s.id] = rowfence.Classification{Tags: s.tags, Parties: s.parties}
				if _, err := conn.Exec(ctx, `INSERT INTO evidence VALUES ($1,$2,$3)`, s.id, nonNil(s.tags), nonNil(s.parties)); err != nil {
					fatalf("seed evidence %s: %v", s.id, err)
				}
			}
			if _, err := conn.Exec(ctx, `INSERT INTO diff_links VALUES ($1,$2,$3,$4)`,
				l.id, l.cited.id, nonNil(l.row), nonNil(supp)); err != nil {
				fatalf("seed link %s: %v", l.id, err)
			}
		}
		lf, la := sqlfence.LinkExists(p, "l", 0)
		got, err := query(`SELECT l.id FROM diff_links l WHERE true`+lf, la)
		if err != nil {
			fatalf("link exists: %v\n%s", err, lf)
		}
		tf, ta := sqlfence.TypedRowExists(p, "l.evidence_id", "l.classification", 0)
		typedGot, err := query(`SELECT l.id FROM diff_links l WHERE true`+tf, ta)
		if err != nil {
			fatalf("typed row exists: %v\n%s", err, tf)
		}
		// Go: the walk guard's decision over linkRefs (guardNodes' body without
		// the evidence lookup).
		goKept := rowfence.FilterRows(p, links, func(l diffLink) ([]rowfence.Classification, bool) {
			dl := domain.Link{EvidenceID: domain.EvidenceID(l.cited.id), Classification: l.row}
			for _, s := range l.support {
				dl.SupportingEvidence = append(dl.SupportingEvidence, domain.EvidenceID(s.id))
			}
			var cs []rowfence.Classification
			for _, r := range linkRefs(dl) {
				c, ok := tags[r.evidence]
				if !ok {
					return nil, false
				}
				cs = append(cs, rowfence.TypedRow(c, r.rowTags)...)
			}
			return cs, true
		})
		goGot := []string{}
		for _, l := range goKept {
			goGot = append(goGot, l.id)
		}
		sort.Strings(goGot)
		want, wantTyped := []string{}, []string{}
		inGot, inTyped := map[string]bool{}, map[string]bool{}
		for _, id := range got {
			inGot[id] = true
		}
		for _, id := range typedGot {
			inTyped[id] = true
		}
		for _, l := range links {
			typed, link := linkVerdict(p, l)
			if typed {
				wantTyped = append(wantTyped, l.id)
			}
			if link {
				want = append(want, l.id)
			}
			if len(l.support) == 0 {
				emptySupport++
				if inGot[l.id] != inTyped[l.id] {
					fatalf("(iii) empty supporting set changed the verdict for %s\npredicate: %+v\nsql: %s", l.id, *p, lf)
				}
			}
			if typed && !link && !p.Unfiltered() {
				for _, s := range l.support {
					if s.missing {
						bySupportMissing++
						continue
					}
					if _, ok := p.CheckRow(s.tags, s.parties); ok {
						continue
					}
					// A party-scoped tag the reader is not a party to, or a
					// closed one (forbidden, required, any-of, owner).
					if overlaps(s.tags, p.PartyScopedTags) && !overlaps(s.parties, p.PartyIdentities) {
						bySupportParty++
					} else {
						bySupportClosed++
					}
				}
			}
		}
		sort.Strings(want)
		sort.Strings(wantTyped)
		fail := func(what string) {
			fatalf("%s\npredicate: %+v\nlinks: %+v\nSQL link:  %v\nSQL typed: %v\nGo walk:   %v\nwant:      %v\nsql: %s\nargs: %#v",
				what, *p, links, got, typedGot, goGot, want, lf, la)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			fail("(i) LinkExists diverges from the CheckRow conjunction")
		}
		if strings.Join(goGot, ",") != strings.Join(want, ",") {
			fail("(i) the walk guard over linkRefs diverges from the CheckRow conjunction")
		}
		if strings.Join(typedGot, ",") != strings.Join(wantTyped, ",") {
			fail("TypedRowExists diverges from CheckRow(E,P) && CheckRow(E∪R,P)")
		}
		for id := range inGot {
			if !inTyped[id] {
				fail("(ii) LinkExists WIDENED: " + id)
			}
		}
	}

	// Phase 1 — a fixed matrix.
	pub := supportEv{id: "ev-pub", tags: []string{"a"}}
	matrix := []diffLink{
		{id: "plain", cited: pub},
		{id: "support-open", cited: pub, support: []supportEv{{id: "ev-open", tags: []string{"a", "b"}}}},
		{id: "support-closed", cited: pub, support: []supportEv{{id: "ev-hr", tags: []string{"a", "hr"}}}},
		{id: "support-party", cited: pub, support: []supportEv{{id: "ev-deal", tags: []string{"a", "deal:x"}, parties: []string{"p2"}}}},
		{id: "support-missing", cited: pub, support: []supportEv{{id: "ev-gone", missing: true}}},
		{id: "support-owner", cited: pub, support: []supportEv{{id: "ev-alice", tags: []string{"a", "owner:alice"}}}},
	}
	for _, m := range []struct {
		name string
		p    *domain.TagPredicate
		want string
	}{
		{"closed hr, deal:x party-scoped for p1",
			&domain.TagPredicate{ForbiddenTags: []string{"hr"}, PartyScopedTags: []string{"deal:x"}, PartyIdentities: []string{"p1"}},
			"plain,support-open"},
		{"party p2", &domain.TagPredicate{PartyScopedTags: []string{"deal:x"}, PartyIdentities: []string{"p2"}},
			"plain,support-closed,support-open,support-party"},
		{"alice", &domain.TagPredicate{Owners: []string{"alice"}},
			"plain,support-closed,support-open,support-owner,support-party"},
		{"system (owner fence kept)", domain.ScopeSystem, "plain,support-closed,support-open,support-party"},
		{"maintenance", domain.ScopeMaintenance, "plain,support-closed,support-missing,support-open,support-owner,support-party"},
	} {
		verify(t.Fatalf, m.p, matrix)
		lf, la := sqlfence.LinkExists(m.p, "l", 0)
		got, err := query(`SELECT l.id FROM diff_links l WHERE true`+lf, la)
		if err != nil || strings.Join(got, ",") != m.want {
			t.Fatalf("matrix %q: got %v err=%v, want %s", m.name, got, err, m.want)
		}
	}

	// Phase 2 — random predicates and links.
	bySupportClosed, bySupportParty, bySupportMissing, emptySupport = 0, 0, 0, 0
	rapid.Check(t, func(rt *rapid.T) {
		p := drawPredicate(rt, "p", 0)
		material := append(append([]string{}, typedRowTags...), p.ForbiddenTags...)
		material = append(material, p.PartyScopedTags...)
		// Most cited evidence satisfies the predicate's positive terms, so the
		// supporting rows are what decides (a link the cited fence already hides
		// says nothing about them).
		satisfying := append([]string{}, p.RequiredTags...)
		for _, c := range p.AnyOfClauses {
			if len(c) > 0 {
				satisfying = append(satisfying, c[0])
			}
		}
		n := rapid.IntRange(1, 6).Draw(rt, "links")
		var links []diffLink
		for i := 0; i < n; i++ {
			l := diffLink{id: fmt.Sprintf("l%d", i)}
			l.cited = supportEv{id: fmt.Sprintf("ev%d", i), tags: subset(rt, diffTags, fmt.Sprintf("l%d:tags", i)),
				parties: subset(rt, diffParties, fmt.Sprintf("l%d:parties", i))}
			if rapid.IntRange(0, 3).Draw(rt, fmt.Sprintf("l%d:satisfying", i)) != 0 {
				l.cited.tags = append([]string{}, satisfying...)
				l.cited.parties = nil
			}
			if rapid.IntRange(0, 3).Draw(rt, fmt.Sprintf("l%d:has-row", i)) == 0 {
				l.row = subset(rt, typedRowTags, fmt.Sprintf("l%d:row", i))
			}
			ns := rapid.IntRange(0, 3).Draw(rt, fmt.Sprintf("l%d:nsupport", i))
			if l.row != nil && rapid.Bool().Draw(rt, fmt.Sprintf("l%d:plain-row", i)) {
				l.row = nil
			}
			for j := 0; j < ns; j++ {
				s := supportEv{id: fmt.Sprintf("ev%d-s%d", i, j)}
				if rapid.IntRange(0, 7).Draw(rt, fmt.Sprintf("l%d:s%d:missing", i, j)) == 0 {
					s.missing = true
				} else {
					s.tags = append(append([]string{}, l.cited.tags...), subset(rt, material, fmt.Sprintf("l%d:s%d:tags", i, j))...)
					// Draw the predicate's own restricting material on purpose half
					// the time, so supporting rows the predicate closes or
					// party-scopes are common, not lucky.
					if len(p.PartyScopedTags) > 0 && rapid.Bool().Draw(rt, fmt.Sprintf("l%d:s%d:party-tag", i, j)) {
						s.tags = append(s.tags, rapid.SampledFrom(p.PartyScopedTags).Draw(rt, fmt.Sprintf("l%d:s%d:pt", i, j)))
					}
					if len(p.ForbiddenTags) > 0 && rapid.IntRange(0, 3).Draw(rt, fmt.Sprintf("l%d:s%d:closed-tag", i, j)) == 0 {
						s.tags = append(s.tags, rapid.SampledFrom(p.ForbiddenTags).Draw(rt, fmt.Sprintf("l%d:s%d:ft", i, j)))
					}
					s.parties = subset(rt, diffParties, fmt.Sprintf("l%d:s%d:parties", i, j))
				}
				l.support = append(l.support, s)
			}
			links = append(links, l)
		}
		verify(rt.Fatalf, p, links)
	})
	if bySupportClosed == 0 || bySupportParty == 0 || bySupportMissing == 0 || emptySupport == 0 {
		t.Fatalf("coverage: hidden by a closed supporting row %d, by its parties %d, by a missing row %d; empty sets %d — each must be > 0",
			bySupportClosed, bySupportParty, bySupportMissing, emptySupport)
	}
	t.Logf("coverage: closed %d, party %d, missing %d, empty sets %d", bySupportClosed, bySupportParty, bySupportMissing, emptySupport)
}
