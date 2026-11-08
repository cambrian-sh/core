package postgres

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory"
)

// The owner fence (domain/owner_scope.go, ADR-0141 position 3,
// T-20261106-owner-scope-tag-owner-principal-from-personal) against a real,
// migrated Postgres (PG_TEST_DSN; `orchestrator migrate up` at 1536 dims, as in
// CI). Every SQL mirror is exercised by the database itself: a rendered-string
// assertion cannot vouch for jsonb_path_exists or a NOT EXISTS over unnest.
//
// The file name sorts before migration_guard_test.go on purpose: that test drops
// and recreates the public schema, and every Pg test that runs after it in the
// same package binary meets a schema without `chunks`.
//
// The cast, on every lane:
//   - alice and bob each have a personal-ingest row (owner:alice / owner:bob);
//   - a third row is the unowned corpus;
//   - readers: alice, bob, an agent acting for nobody, the operator plane
//     (ScopeSystem), a policy that names bob's owner tag and every label
//     (grant-everything), and kernel maintenance (ScopeMaintenance).

const ofPrefix = "ownfence-"

var (
	ofAlice   = &domain.TagPredicate{Owners: []string{"alice"}}
	ofBob     = &domain.TagPredicate{Owners: []string{"bob"}}
	ofNobody  = &domain.TagPredicate{}
	ofGrabAll = &domain.TagPredicate{
		// A policy that tries every route to bob's row — his owner tag as a
		// required-any label, the shared label — while the READER is alice.
		AnyOfClauses: [][]string{{"mail", "owner:bob"}},
		Owners:       []string{"alice"},
	}
)

// ofReaders is every reader with the ids it must see among {a, b, pub}.
func ofReaders() []struct {
	name string
	pred *domain.TagPredicate
	want []string
} {
	return []struct {
		name string
		pred *domain.TagPredicate
		want []string
	}{
		{"alice", ofAlice, []string{"a", "pub"}},
		{"bob", ofBob, []string{"b", "pub"}},
		{"agent acting for nobody", ofNobody, []string{"pub"}},
		{"operator plane (ScopeSystem)", domain.ScopeSystem, []string{"pub"}},
		{"policy granting everything, read by alice", ofGrabAll, []string{"a", "pub"}},
		{"kernel maintenance", domain.ScopeMaintenance, []string{"a", "b", "pub"}},
	}
}

func newOwnerFencePool(t *testing.T) (*pgxpool.Pool, context.Context) {
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
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error { return pgxvector.RegisterTypes(ctx, conn) }
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
		c := context.Background()
		for _, q := range []string{
			`DELETE FROM chunk_triplets WHERE chunk_id LIKE 'ownfence-%'`,
			`DELETE FROM chunks WHERE id LIKE 'ownfence-%'`,
			`DELETE FROM documents WHERE id LIKE 'ownfence-%'`,
			`DELETE FROM observations WHERE entity_id LIKE 'ownfence/%'`,
			`DELETE FROM entities WHERE id LIKE 'ownfence/%'`,
			`DELETE FROM evidence WHERE id LIKE 'ownfence-ev-%'`,
		} {
			_, _ = pool.Exec(c, q)
		}
	}
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

func ofIDs(t *testing.T, docs []domain.Document) []string {
	t.Helper()
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		if id, ok := strings.CutPrefix(d.ID, ofPrefix); ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func ofHitIDs(t *testing.T, hits []domain.SearchResult) []string {
	t.Helper()
	docs := make([]domain.Document, 0, len(hits))
	for _, h := range hits {
		docs = append(docs, h.Document)
	}
	return ofIDs(t, docs)
}

func ofStripIDs(ids []string) []string {
	out := []string{}
	for _, id := range ids {
		if s, ok := strings.CutPrefix(id, ofPrefix); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func ofEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Every chunk lane: dense search, lexical search, the two entity-seeding lanes
// and the four by-id reads through the enforcing decorator (which is the only
// thing guarding them — the raw adapter's by-id reads filter nothing).
func TestPg_OwnerFence_EveryChunkLane(t *testing.T) {
	pool, ctx := newOwnerFencePool(t)
	p := &PgVectorAdapter{pool: pool, dim: eventTestDim}
	now := time.Now().UTC()
	for id, tags := range map[string][]string{
		"a":   {"mail", "owner:alice"},
		"b":   {"mail", "owner:bob"},
		"pub": {"mail"},
	} {
		d := chunkDoc(ofPrefix+id, domain.EventStamp{}, now, tags...)
		d.Text = "quarterly invoice from acme " + id
		d.Embedding = domain.Embedding{Vector: unitVec(7)}
		d.Metadata["ownfence"] = "1"
		if err := p.Save(ctx, d); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO chunk_triplets (chunk_id, h, r, t) VALUES ($1, 'ownfence-acme', 'sent', 'invoice')`, ofPrefix+id); err != nil {
			t.Fatalf("triplet %s: %v", id, err)
		}
	}
	store := authz.NewEnforcingVectorStore(p, nil)
	all := []string{ofPrefix + "a", ofPrefix + "b", ofPrefix + "pub"}

	for _, r := range ofReaders() {
		t.Run(r.name, func(t *testing.T) {
			sctx := domain.WithScope(ctx, r.pred)
			check := func(lane string, got []string, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s: %v", lane, err)
				}
				if !ofEqual(got, r.want) {
					t.Errorf("%s: got %v, want %v", lane, got, r.want)
				}
			}

			hits, err := store.Search(sctx, unitVec(7), domain.SearchOptions{TopK: 50, Scope: r.pred})
			check("dense Search", ofHitIDs(t, hits), err)

			lex, err := p.LexicalSearch(sctx, "quarterly invoice acme", domain.SearchOptions{TopK: 50, Scope: r.pred})
			check("LexicalSearch", ofHitIDs(t, lex), err)

			ment, err := p.ChunksMentioningEntity(sctx, "ownfence-acme", 50, r.pred)
			check("ChunksMentioningEntity (entity seeding)", ofStripIDs(ment), err)

			ranked, err := p.ChunksMentioningEntityRanked(sctx, "ownfence-acme", 50, r.pred, unitVec(7))
			check("ChunksMentioningEntityRanked", ofStripIDs(ranked), err)

			ents, err := p.ChunksForEntities(sctx, []string{"ownfence-acme"}, 50, r.pred, unitVec(7))
			var entIDs []string
			for _, h := range ents {
				entIDs = append(entIDs, h.ChunkID)
			}
			check("ChunksForEntities (KG expansion seeds)", ofStripIDs(entIDs), err)

			// By-id: one GetByID per row, then the batch forms.
			var byID []string
			for _, id := range all {
				d, err := store.GetByID(sctx, id)
				if err != nil {
					t.Fatalf("GetByID %s: %v", id, err)
				}
				if d != nil {
					byID = append(byID, d.ID)
				}
			}
			check("GetByID", ofStripIDs(byID), nil)

			batch, err := store.GetBatch(sctx, all)
			check("GetBatch", ofIDs(t, batch), err)

			cb, err := store.ChunksByIDs(sctx, all)
			check("ChunksByIDs (neighbour windows / anchors)", ofIDs(t, cb), err)

			qm, err := store.QueryByMetadata(sctx, map[string]string{"ownfence": "1"}, 50)
			check("QueryByMetadata", ofIDs(t, qm), err)
		})
	}

	// An unresolved principal reaches the store with no predicate at all: refused
	// outright, so it sees no owner row (nor anything else).
	if _, err := store.GetByID(ctx, ofPrefix+"a"); err == nil {
		t.Fatal("a by-id read with no predicate must be refused (fail closed)")
	}
	if _, err := store.Search(ctx, unitVec(7), domain.SearchOptions{TopK: 5}); err == nil {
		t.Fatal("a search with no predicate must be refused (fail closed)")
	}
}

// Facts / knowledge items: the TEXT[] mirror (itemScopeSQL) on search, by-id,
// the chain and the similar-fact read.
func TestPg_OwnerFence_FactItems(t *testing.T) {
	ks, r, ctx := newFactSubstrate(t)
	a := a1Fact("diagnosis", "flu", "a1-c1", a1Day("2024-01-01"), "mail", "owner:alice")
	b := a1Fact("diagnosis", "cold", "a1-c2", a1Day("2024-01-01"), "mail", "owner:bob")
	pub := a1Fact("diagnosis", "none", "a1-c3", a1Day("2024-01-01"), "mail")
	putAll(t, ks, ctx, a, b, pub)
	if err := ks.PutItemEmbeddings(ctx, []domain.ItemEmbedding{
		{ItemID: a.ID, ModelID: "a1-model", Vector: unitVec(9)},
		{ItemID: b.ID, ModelID: "a1-model", Vector: unitVec(9)},
		{ItemID: pub.ID, ModelID: "a1-model", Vector: unitVec(9)},
	}); err != nil {
		t.Fatal(err)
	}
	name := map[string]string{string(a.ID): "a", string(b.ID): "b", string(pub.ID): "pub"}
	for _, rd := range ofReaders() {
		t.Run(rd.name, func(t *testing.T) {
			hits, err := r.SearchFacts(ctx, unitVec(9), domain.FactSearchOptions{TopK: 10, Scope: rd.pred})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, h := range hits {
				got = append(got, name[h.Fact.ID])
			}
			sort.Strings(got)
			if !ofEqual(got, rd.want) {
				t.Errorf("SearchFacts: got %v, want %v", got, rd.want)
			}
			byID, err := r.FactsByID(ctx, []domain.KnowledgeItemID{a.ID, b.ID, pub.ID}, rd.pred)
			if err != nil {
				t.Fatal(err)
			}
			got = got[:0]
			for _, f := range byID {
				got = append(got, name[f.ID])
			}
			sort.Strings(got)
			if !ofEqual(got, rd.want) {
				t.Errorf("FactsByID: got %v, want %v", got, rd.want)
			}
			// The chain of alice's own key: hers to read, nobody else's.
			chain, err := r.FactChain(ctx, domain.FactFromItem(a), rd.pred)
			if err != nil {
				t.Fatal(err)
			}
			wantChain := 0
			for _, w := range rd.want {
				if w == "a" {
					wantChain = 1
				}
			}
			if len(chain) != wantChain {
				t.Errorf("FactChain(alice's key): %d rows, want %d", len(chain), wantChain)
			}
		})
	}
}

// The substrate query plane: evidence-backed observations, both the row-filter
// path (point) and the in-SQL path (aggregate, evidenceScopeSQL).
func TestPg_OwnerFence_QueryPlane(t *testing.T) {
	pool, ctx := newOwnerFencePool(t)
	reg, err := domain.NewRelationRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	events := NewPgEventStore(pool, nil)
	plane := NewPgQueryPlane(pool, events, nil, nil, NewPgLinkStore(pool, reg), NewPgEntityStore(pool), reg)
	now := time.Now().UTC()
	for id, tags := range map[string][]string{
		"a": {"mail", "owner:alice"}, "b": {"mail", "owner:bob"}, "pub": {"mail"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification)
			VALUES ($1,'default','ownfence-source',$1,'sha256:test',$2)`, "ownfence-ev-"+id, tags); err != nil {
			t.Fatalf("seed evidence %s: %v", id, err)
		}
		if _, err := events.RecordObservation(ctx, domain.Observation{
			NamespaceID: "default", EntityID: "ownfence/patient", Predicate: "visits",
			Value:      domain.StatementValue{Type: "number", Number: 1},
			OccurredAt: now, Confidence: 1, EvidenceID: domain.EvidenceID("ownfence-ev-" + id), SourceRef: "ownfence-" + id,
		}); err != nil {
			t.Fatalf("observation %s: %v", id, err)
		}
	}
	for _, rd := range ofReaders() {
		t.Run(rd.name, func(t *testing.T) {
			res, err := plane.Query(ctx, domain.KnowledgeQuery{
				Kind: domain.QueryHistory, EntityID: "ownfence/patient", Predicate: "visits", Limit: 50,
				From: now.Add(-time.Hour), To: now.Add(time.Hour),
			}, rd.pred)
			if err != nil {
				t.Fatalf("history: %v", err)
			}
			if len(res.Rows) != len(rd.want) {
				t.Errorf("history: %d rows, want %d (%v)", len(res.Rows), len(rd.want), res.Rows)
			}
			agg, err := plane.Query(ctx, domain.KnowledgeQuery{
				Kind: domain.QueryAggregate, Aggregate: "count", EntityID: "ownfence/patient", Predicate: "visits",
			}, rd.pred)
			if err != nil {
				t.Fatalf("aggregate: %v", err)
			}
			if len(agg.Rows) != 1 || toInt(agg.Rows[0]["value"]) != len(rd.want) {
				t.Errorf("aggregate count: %v, want %d", agg.Rows, len(rd.want))
			}
		})
	}
}

// The operator/MCP document listing, and the two writes that could otherwise
// move the fence: an operator retag, and a re-ingest upsert.
func TestPg_OwnerFence_DocumentListingRetagAndUpsert(t *testing.T) {
	pool, ctx := newOwnerFencePool(t)
	p := &PgVectorAdapter{pool: pool, dim: eventTestDim}
	for id, tags := range map[string][]string{
		"a": {"mail", "owner:alice"}, "b": {"mail", "owner:bob"}, "pub": {"mail"},
	} {
		if _, err := p.SaveDocument(ctx, domain.SourceDocument{ID: ofPrefix + id, Title: "Re: diagnosis " + id, Tags: tags}); err != nil {
			t.Fatalf("save document %s: %v", id, err)
		}
	}
	list := func(owners ...string) []string {
		t.Helper()
		page, _, total, err := p.ListDocuments(ctx, memory.DocumentFilter{IDPrefix: ofPrefix, Limit: 50, Owners: owners})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, d := range page {
			ids = append(ids, d.ID)
		}
		got := ofStripIDs(ids)
		if total != len(got) {
			t.Errorf("total %d disagrees with the page %v", total, got)
		}
		return got
	}
	if got := list(); !ofEqual(got, []string{"pub"}) {
		t.Errorf("operator listing (no owner): %v, want only the unowned document", got)
	}
	if got := list("alice"); !ofEqual(got, []string{"a", "pub"}) {
		t.Errorf("alice's listing: %v", got)
	}

	// An operator retag that drops alice's owner tag (and tries to coin bob's)
	// keeps hers and ignores his — on the document row AND the chunk cache.
	if err := p.Save(ctx, chunkDoc(ofPrefix+"a-c1", domain.EventStamp{}, time.Now().UTC(), "mail", "owner:alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE chunks SET document_id = $1 WHERE id = $2`, ofPrefix+"a", ofPrefix+"a-c1"); err != nil {
		t.Fatal(err)
	}
	if err := p.RetagDocument(ctx, ofPrefix+"a", []string{"public", "owner:bob"}); err != nil {
		t.Fatal(err)
	}
	tags, _, err := p.DocumentTags(ctx, ofPrefix+"a")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(tags)
	if !ofEqual(tags, []string{"owner:alice", "public"}) {
		t.Errorf("retag moved the fence: document tags %v", tags)
	}
	chunk := mustGet(t, p, ctx, ofPrefix+"a-c1")
	ct := authz.DocTags(chunk)
	sort.Strings(ct)
	if !ofEqual(ct, []string{"owner:alice", "public"}) {
		t.Errorf("retag moved the fence on the chunk cache: %v", ct)
	}

	// A re-ingest that arrives without the owner tag keeps it, and says so.
	stored, err := p.SaveDocument(ctx, domain.SourceDocument{ID: ofPrefix + "b", Title: "Re: diagnosis b", Tags: []string{"mail"}})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(stored)
	if !ofEqual(stored, []string{"mail", "owner:bob"}) {
		t.Errorf("re-ingest dropped the owner tag: stored %v", stored)
	}
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case *float64:
		if n != nil {
			return int(*n)
		}
	}
	return -1
}
