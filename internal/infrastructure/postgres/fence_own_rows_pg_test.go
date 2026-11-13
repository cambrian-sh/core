package postgres

import (
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/doug-martin/goqu/v9"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
)

// ADR-0146 acceptance test 7: the own-rows rule's Go form (TagPredicate.Check /
// CheckRow) and every SQL mirror — sqlfence.JSONBTags (jsonb, chunks and the other
// metadata tables), sqlfence.TextArray (knowledge items, the fact
// reader, the catalog) and sqlfence.Evidence (evidence, the query plane, the
// catalog) — admit EXACTLY the same rows, over one matrix of predicates × rows,
// against a real migrated Postgres (PG_TEST_DSN, as in CI). The SQL mirrors
// are exercised by the database, not by asserting rendered strings.
//
// It also covers tests 1, 3, 4 and 5 on the SQL side (the own row, mixed/foreign
// owners, the owner's closed tag, the agent's floor) and test 1's memory lanes
// end to end (dense search and by-id through EnforcingVectorStore).
//
// File name sorts before migration_guard_test.go (see fence_owner_scope_pg_test.go).

const orA = "vantis-ops-lkovacs"

// orPredicates are the readers: the shapes premium's resolveParts builds, plus
// malformed and party-scoped variants.
func orPredicates() map[string]*domain.TagPredicate {
	agent := []string{"provenance:source=composed_worker", "ops"}
	owner := []string{"provenance:source=" + orA, "shared:vantis-medical", "engineering"}
	agentForA := func() *domain.TagPredicate {
		return &domain.TagPredicate{
			AnyOfClauses:  [][]string{agent, owner},
			ForbiddenTags: []string{"hr"},
			Owners:        []string{orA},
			OwnRows:       &domain.TagPredicate{AnyOfClauses: [][]string{agent}, ForbiddenTags: []string{"hr"}, Owners: []string{orA}},
		}
	}
	noTerm := agentForA()
	noTerm.OwnRows = nil
	malformed := agentForA()
	malformed.OwnRows.Owners = []string{"bob"}
	party := agentForA()
	party.PartyScopedTags = []string{"case"}
	party.PartyIdentities = []string{"ent:a"}
	party.OwnRows.PartyScopedTags = []string{"case"}
	party.OwnRows.PartyIdentities = []string{"ent:a"}
	required := agentForA()
	required.RequiredTags = []string{"tenant:t1"}
	required.OwnRows.RequiredTags = []string{"tenant:t1"}
	return map[string]*domain.TagPredicate{
		"agent for A (own-rows term)":  agentForA(),
		"agent for A, no term (today)": noTerm,
		"agent for A, malformed term":  malformed,
		"agent for A, party-scoped":    party,
		"agent for A, agent required":  required,
		"person A": {
			AnyOfClauses: [][]string{owner}, ForbiddenTags: []string{"hr"}, Owners: []string{orA},
			OwnRows: &domain.TagPredicate{ForbiddenTags: []string{"hr"}, Owners: []string{orA}},
		},
		"agent for nobody": {AnyOfClauses: [][]string{agent}},
		"operator plane":   domain.ScopeSystem,
	}
}

// orRows: name → tags, parties (parties apply to evidence only).
type orRow struct {
	tags    []string
	parties []string
}

func orRows() map[string]orRow {
	cw := "provenance:source=composed_worker"
	oa, ob := domain.OwnerTag(orA), domain.OwnerTag("bob")
	return map[string]orRow{
		"own":            {tags: []string{cw, oa}},
		"own-eng":        {tags: []string{cw, "engineering", oa}},
		"own-dup":        {tags: []string{cw, oa, oa}},
		"own-tenant":     {tags: []string{cw, "tenant:t1", oa}},
		"mixed":          {tags: []string{cw, oa, ob}},
		"empty-owner":    {tags: []string{cw, oa, domain.OwnerTagPrefix}},
		"bob":            {tags: []string{cw, ob}},
		"own-hr":         {tags: []string{cw, oa, "hr"}},
		"own-no-agent":   {tags: []string{"engineering", oa}},
		"shared":         {tags: []string{"ops", "engineering"}},
		"ops":            {tags: []string{"ops"}},
		"own-case-party": {tags: []string{cw, oa, "case"}, parties: []string{"ent:a"}},
		"own-case-other": {tags: []string{cw, oa, "case"}, parties: []string{"ent:b"}},
		"untagged":       {tags: nil},
	}
}

func orWant(pred *domain.TagPredicate, rows map[string]orRow, rowAware bool) []string {
	var out []string
	for name, r := range rows {
		var ok bool
		if rowAware {
			_, ok = pred.CheckRow(r.tags, r.parties)
		} else {
			_, ok = pred.Check(r.tags)
		}
		if ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

func orCollect(t *testing.T, rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}, prefix string) []string {
	t.Helper()
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.TrimPrefix(id, prefix))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestPg_OwnRows_GoSQLParity(t *testing.T) {
	pool, ctx := newOwnerFencePool(t)
	rows := orRows()
	p := &PgVectorAdapter{pool: pool, dim: eventTestDim}
	now := time.Now().UTC()
	const chunkPrefix, evPrefix = "ownfence-or-", "ownfence-ev-or-"
	for name, r := range rows {
		d := chunkDoc(chunkPrefix+name, domain.EventStamp{}, now, r.tags...)
		if err := p.Save(ctx, d); err != nil {
			t.Fatalf("save chunk %s: %v", name, err)
		}
		parties := r.parties
		if parties == nil {
			parties = []string{}
		}
		tags := r.tags
		if tags == nil {
			tags = []string{}
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification, parties)
			VALUES ($1,'default','ownfence-or-source',$1,'sha256:test',$2,$3)`, evPrefix+name, tags, parties); err != nil {
			t.Fatalf("seed evidence %s: %v", name, err)
		}
	}
	// Knowledge items: the TEXT[] mirror, on the knowledge_items table itself.
	ks, _, fctx := newFactSubstrate(t)
	itemName := map[string]string{}
	i := 0
	for name, r := range rows {
		i++
		it := a1Fact("ownrows", name, "or-c"+name, a1Day("2024-01-01"), r.tags...)
		if _, _, err := ks.PutItem(fctx, it); err != nil {
			t.Fatalf("PutItem %s: %v", name, err)
		}
		itemName[string(it.ID)] = name
	}

	for pname, pred := range orPredicates() {
		t.Run(pname, func(t *testing.T) {
			// jsonb mirror (chunks): Check, no parties.
			exprs := append([]goqu.Expression{goqu.L("id LIKE ?", chunkPrefix+"%")}, sqlfence.JSONBTags("").Goqu(pred)...)
			q, args, err := dialect.From(TableChunks).Select("id").Where(exprs...).Prepared(true).ToSQL()
			if err != nil {
				t.Fatal(err)
			}
			res, err := pool.Query(ctx, q, args...)
			if err != nil {
				t.Fatalf("chunks query: %v\n%s", err, q)
			}
			if got, want := orCollect(t, res, chunkPrefix), orWant(pred, rows, false); !ofEqual(got, want) {
				t.Errorf("sqlfence.JSONBTags ≠ Check:\n got  %v\n want %v", got, want)
			}

			// TEXT[] mirror over evidence with parties: CheckRow.
			conds, cargs := sqlfence.Evidence("e").Where(pred, 1)
			res, err = pool.Query(ctx, `SELECT e.id FROM evidence e WHERE e.id LIKE $1`+conds,
				append([]any{evPrefix + "%"}, cargs...)...)
			if err != nil {
				t.Fatalf("evidence query: %v\n%s", err, conds)
			}
			if got, want := orCollect(t, res, evPrefix), orWant(pred, rows, true); !ofEqual(got, want) {
				t.Errorf("sqlfence.Evidence ≠ CheckRow:\n got  %v\n want %v", got, want)
			}

			// TEXT[] mirror over knowledge items (no parties column): Check.
			iconds, iargs := sqlfence.TextArray("k.classification").Where(pred, 1)
			res, err = ks.pool.Query(fctx, `SELECT k.id FROM knowledge_items k WHERE k.namespace_id = $1`+iconds,
				append([]any{a1NS}, iargs...)...)
			if err != nil {
				t.Fatalf("items query: %v\n%s", err, iconds)
			}
			var got []string
			for _, id := range orCollect(t, res, "") {
				if n, ok := itemName[id]; ok {
					got = append(got, n)
				}
			}
			sort.Strings(got)
			if got == nil {
				got = []string{}
			}
			if want := orWant(pred, rows, false); !ofEqual(got, want) {
				t.Errorf("sqlfence.TextArray ≠ Check:\n got  %v\n want %v", got, want)
			}
		})
	}

	// The rule itself, stated once on the SQL side (tests 1, 3, 4, 5): under the
	// own-rows term the own row is admitted, and nothing else changes.
	pred := orPredicates()["agent for A (own-rows term)"]
	want := orWant(pred, rows, false)
	for _, must := range []string{"own", "own-dup"} {
		if !slices.Contains(want, must) {
			t.Errorf("own row %q not admitted: %v", must, want)
		}
	}
	for _, mustNot := range []string{"mixed", "empty-owner", "bob", "own-hr", "own-no-agent", "ops", "untagged"} {
		if slices.Contains(want, mustNot) {
			t.Errorf("%q admitted under the own-rows rule: %v", mustNot, want)
		}
	}
}

// Test 1 end to end on the memory lanes: dense search and by-id reads through the
// enforcing decorator, under the agent-for-A predicate and without the term.
func TestPg_OwnRows_MemoryLanes(t *testing.T) {
	pool, ctx := newOwnerFencePool(t)
	p := &PgVectorAdapter{pool: pool, dim: eventTestDim}
	now := time.Now().UTC()
	rows := map[string][]string{
		"own":   {"provenance:source=composed_worker", domain.OwnerTag(orA)},
		"mixed": {"provenance:source=composed_worker", domain.OwnerTag(orA), domain.OwnerTag("bob")},
		"bob":   {"provenance:source=composed_worker", domain.OwnerTag("bob")},
	}
	for name, tags := range rows {
		d := chunkDoc(ofPrefix+"lane-"+name, domain.EventStamp{}, now, tags...)
		d.Embedding = domain.Embedding{Vector: unitVec(11)}
		if err := p.Save(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	store := authz.NewEnforcingVectorStore(p, nil)
	for name, tc := range map[string]struct {
		pred *domain.TagPredicate
		want []string
	}{
		"agent for A":          {orPredicates()["agent for A (own-rows term)"], []string{"lane-own"}},
		"agent for A, no term": {orPredicates()["agent for A, no term (today)"], []string{}},
		"agent for nobody":     {orPredicates()["agent for nobody"], []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			sctx := domain.WithScope(ctx, tc.pred)
			hits, err := store.Search(sctx, unitVec(11), domain.SearchOptions{TopK: 20, Scope: tc.pred})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, id := range ofHitIDs(t, hits) {
				if strings.HasPrefix(id, "lane-") {
					got = append(got, id)
				}
			}
			if got == nil {
				got = []string{}
			}
			if !ofEqual(got, tc.want) {
				t.Errorf("dense Search: got %v, want %v", got, tc.want)
			}
			var byID []string
			for n := range rows {
				d, err := store.GetByID(sctx, ofPrefix+"lane-"+n)
				if err != nil {
					t.Fatal(err)
				}
				if d != nil {
					byID = append(byID, strings.TrimPrefix(d.ID, ofPrefix))
				}
			}
			sort.Strings(byID)
			if byID == nil {
				byID = []string{}
			}
			if !ofEqual(byID, tc.want) {
				t.Errorf("GetByID: got %v, want %v", byID, tc.want)
			}
		})
	}
}
