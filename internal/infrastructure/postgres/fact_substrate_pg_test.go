package postgres

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0136 Amendment A1 against a real, migrated Postgres (PG_TEST_DSN; schema from
// `orchestrator migrate up` with CAMBRIAN_EMBEDDER__DIMENSIONS=1536, as in CI). Every row
// lives in the substrate namespace "a1test" (documents/chunks: the "a1test-" id prefix)
// and is deleted before and after.

const a1NS = "a1test"

func newFactSubstrate(t *testing.T) (*PgKnowledgeStore, *PgFactItemReader, context.Context) {
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'knowledge_assessments'`).Scan(&n); err != nil || n == 0 {
		t.Skipf("knowledge_assessments missing (run `orchestrator migrate up` first): n=%d err=%v", n, err)
	}
	clean := func() {
		c := context.Background()
		for _, q := range []string{
			`DELETE FROM resolutions WHERE namespace_id = 'a1test'`,
			`DELETE FROM knowledge_assessments WHERE namespace_id = 'a1test'`,
			`DELETE FROM predicate_profiles WHERE namespace_id = 'a1test'`,
			`DELETE FROM knowledge_items WHERE namespace_id = 'a1test'`,
			`DELETE FROM knowledge_extractions WHERE namespace_id = 'a1test' OR document_id LIKE 'a1test-%'`,
			`DELETE FROM chunks WHERE id LIKE 'a1test-%'`,
			`DELETE FROM documents WHERE id LIKE 'a1test-%'`,
		} {
			_, _ = pool.Exec(c, q)
		}
	}
	clean()
	t.Cleanup(clean)
	reg, err := domain.NewKindRegistry([]domain.KindSpec{domain.MemoryFactKindSpec()},
		[]domain.ResolutionAuthority{domain.ValidTimeSupersessionAuthority{}})
	if err != nil {
		t.Fatal(err)
	}
	ks := NewPgKnowledgeStore(pool, reg)
	return ks, NewPgFactItemReader(ks, a1NS, "a1-model", eventTestDim), ctx
}

func a1Day(s string) *time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return &t
}

// a1Fact is a fact item of key <scope(tags)>/user/<pred>, actor alice.
func a1Fact(pred, value, chunk string, from *time.Time, tags ...string) domain.KnowledgeItem {
	f := domain.Fact{
		Subject: "user", Attribute: pred, Value: value, SubjectKey: "user",
		AttributeKey: domain.NormalizeFactAttribute(pred), ValueNorm: domain.NormalizeFactText(value),
		ValidFrom: from, AssertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Actor: "alice",
		SourceChunkID: chunk, TurnIndex: 0, Quote: value, Tags: tags,
	}
	return f.ToItem(a1NS)
}

func putAll(t *testing.T, ks *PgKnowledgeStore, ctx context.Context, items ...domain.KnowledgeItem) {
	t.Helper()
	for _, it := range items {
		if _, _, err := ks.PutItem(ctx, it); err != nil {
			t.Fatalf("PutItem %s: %v", it.ID, err)
		}
	}
}

func currentRow(t *testing.T, ks *PgKnowledgeStore, ctx context.Context, entity, actor string) (item *string, reason string, from, to *time.Time) {
	t.Helper()
	err := ks.pool.QueryRow(ctx, `SELECT item_id, reason_code, valid_from, valid_to FROM resolutions
		WHERE namespace_id = $1 AND kind = $2 AND entity_id = $3 AND actor = $4 AND policy = $5 AND system_to IS NULL`,
		a1NS, domain.KindMemoryFact, entity, actor, domain.PolicyValidTimeSupersession).Scan(&item, &reason, &from, &to)
	if err != nil {
		t.Fatalf("current resolution of %s: %v", entity, err)
	}
	return
}

func chainByValue(t *testing.T, r *PgFactItemReader, ctx context.Context, it domain.KnowledgeItem, scope *domain.TagPredicate) map[string]domain.Fact {
	t.Helper()
	chain, err := r.FactChain(ctx, domain.FactFromItem(it), scope)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]domain.Fact{}
	for _, f := range chain {
		out[f.Value] = f
	}
	return out
}

// The A1.2 pipeline end to end: a declared single profile, two values by date; the
// resolution versions in system time (the older version closed), the derived chain is
// read back with intervals, and replaying the same items in the opposite order under
// another actor derives an identical chain.
func TestPg_FactItems_ChainDerivedVersionedAndOrderIndependent(t *testing.T) {
	ks, r, ctx := newFactSubstrate(t)
	if _, err := ks.SeedDeclaredProfiles(ctx, a1NS, domain.KindMemoryFact, map[string]string{"home_city": domain.CardinalitySingle}, 3, "test"); err != nil {
		t.Fatal(err)
	}
	paris := a1Fact("home_city", "Paris", "a1-c1", a1Day("2023-01-01"))
	london := a1Fact("home_city", "London", "a1-c2", a1Day("2023-06-01"))
	putAll(t, ks, ctx, paris, london)

	item, reason, from, to := currentRow(t, ks, ctx, london.EntityID, "alice")
	if item == nil || *item != string(london.ID) || reason != domain.ReasonFactCurrent || !from.Equal(*a1Day("2023-06-01")) || to != nil {
		t.Fatalf("current = %v %q %v %v, want london current from 2023-06-01", item, reason, from, to)
	}
	var versions int
	_ = ks.pool.QueryRow(ctx, `SELECT count(*) FROM resolutions WHERE namespace_id = $1 AND entity_id = $2`, a1NS, london.EntityID).Scan(&versions)
	if versions != 2 {
		t.Fatalf("resolution versions = %d, want 2 (paris closed in system time, london current)", versions)
	}
	c := chainByValue(t, r, ctx, paris, &domain.TagPredicate{})
	if p := c["Paris"]; p.SupersededBy != string(london.ID) || p.ValidTo == nil || !p.ValidTo.Equal(*a1Day("2023-06-01")) || p.Current || p.Cardinality != domain.FactSingle {
		t.Fatalf("paris = %+v", p)
	}

	// The same set, written in the opposite order, under another actor.
	rev := []domain.KnowledgeItem{london, paris}
	for i := range rev {
		f := domain.FactFromItem(rev[i])
		f.Actor, f.SourceChunkID = "bob", f.SourceChunkID+"-b"
		rev[i] = f.ToItem(a1NS)
	}
	putAll(t, ks, ctx, rev...)
	c2 := chainByValue(t, r, ctx, rev[0], &domain.TagPredicate{})
	for _, v := range []string{"Paris", "London"} {
		a, b := c[v], c2[v]
		if !reflect.DeepEqual([]any{a.ValidFrom, a.ValidTo, a.Current}, []any{b.ValidFrom, b.ValidTo, b.Current}) {
			t.Fatalf("%s: order changed the derivation: %+v vs %+v", v, a, b)
		}
	}
	// A replayed write is a no-op.
	if _, inserted, err := ks.PutItem(ctx, paris); err != nil || inserted {
		t.Fatalf("replayed put: inserted=%v err=%v", inserted, err)
	}
}

// A1.3: a stored assessment is keyed, idempotent, and re-derives the keys it cites in the
// same transaction; under unknown cardinality the contradictory verdict is what closes.
func TestPg_FactAssessment_StoredKeyedAndRederives(t *testing.T) {
	ks, r, ctx := newFactSubstrate(t)
	iphone := a1Fact("phone", "iPhone", "a1-c1", a1Day("2024-01-01"))
	pixel := a1Fact("phone", "Pixel", "a1-c2", a1Day("2024-03-01"))
	putAll(t, ks, ctx, iphone, pixel)
	if c := chainByValue(t, r, ctx, iphone, &domain.TagPredicate{}); !c["iPhone"].Current || !c["Pixel"].Current {
		t.Fatalf("unknown cardinality, no verdict: both accumulate, got %+v", c)
	}
	inputs := []domain.KnowledgeItemID{pixel.ID, iphone.ID}
	a := domain.Assessment{
		ID: domain.AssessmentID(domain.AssessmentAuthorityLLMContradiction, "m|v", "h", inputs), NamespaceID: a1NS,
		Kind: domain.KindMemoryFact, Authority: domain.AssessmentAuthorityLLMContradiction, AuthorityVersion: "m|v",
		PromptHash: "h", InputItemIDs: inputs, RawOutput: `{"verdicts":[...]}`,
		Verdicts: []domain.AssessmentVerdict{{A: pixel.ID, B: iphone.ID, Verdict: domain.VerdictContradictory}},
	}
	if inserted, err := ks.PutAssessment(ctx, a); err != nil || !inserted {
		t.Fatalf("PutAssessment: inserted=%v err=%v", inserted, err)
	}
	if inserted, err := ks.PutAssessment(ctx, a); err != nil || inserted {
		t.Fatalf("replayed PutAssessment: inserted=%v err=%v, want a no-op", inserted, err)
	}
	got, err := ks.GetAssessment(ctx, a.ID)
	if err != nil || got == nil || got.RawOutput != a.RawOutput || len(got.Verdicts) != 1 || got.Verdicts[0].Verdict != domain.VerdictContradictory {
		t.Fatalf("GetAssessment = %+v err %v", got, err)
	}
	if none, err := ks.GetAssessment(ctx, "asm:missing"); none != nil || err != nil {
		t.Fatalf("a missing assessment reads as (nil, nil): %v %v", none, err)
	}
	c := chainByValue(t, r, ctx, iphone, &domain.TagPredicate{})
	if c["iPhone"].SupersededBy != string(pixel.ID) || !c["Pixel"].Current {
		t.Fatalf("after the verdict: %+v", c)
	}
	item, _, _, _ := currentRow(t, ks, ctx, iphone.EntityID, "alice")
	if item == nil || *item != string(pixel.ID) {
		t.Fatalf("PutAssessment must have re-derived the resolution in its transaction: %v", item)
	}
}

// A1.4 on the store: votes count once per assessment, a learned cardinality flips at the
// threshold, a declared one wins, and RederivePredicate reaches every key of the predicate.
func TestPg_FactProfiles_VotesSeedAndRederive(t *testing.T) {
	ks, _, ctx := newFactSubstrate(t)
	for i, id := range []string{"a1", "a2", "a2", "a3"} {
		changed, err := ks.VoteProfile(ctx, a1NS, domain.KindMemoryFact, "employer", domain.ProfileVote{AssessmentID: id, Single: 1}, 3)
		if err != nil {
			t.Fatal(err)
		}
		if changed != (i == 3) {
			t.Fatalf("vote %d (%s): changed=%v", i, id, changed)
		}
	}
	p, _ := ks.PredicateProfile(ctx, a1NS, domain.KindMemoryFact, "employer")
	if p.Cardinality != domain.CardinalitySingle || p.SingleVotes != 3 || len(p.EvidenceAssessmentIDs) != 3 {
		t.Fatalf("learned profile = %+v", p)
	}
	changed, err := ks.SeedDeclaredProfiles(ctx, a1NS, domain.KindMemoryFact, map[string]string{"employer": domain.CardinalityMulti}, 3, "test")
	if err != nil || len(changed) != 1 {
		t.Fatalf("seed: %v %v", changed, err)
	}
	if _, err := ks.VoteProfile(ctx, a1NS, domain.KindMemoryFact, "employer", domain.ProfileVote{AssessmentID: "a4", Single: 1}, 3); err != nil {
		t.Fatal(err)
	}
	p, _ = ks.PredicateProfile(ctx, a1NS, domain.KindMemoryFact, "employer")
	if p.Cardinality != domain.CardinalityMulti || p.Source != domain.ProfileSourceDeclared || p.SingleVotes != 4 {
		t.Fatalf("declared must win but record votes: %+v", p)
	}
	// Removing it from config reverts it to learned, votes kept.
	if ch, _ := ks.SeedDeclaredProfiles(ctx, a1NS, domain.KindMemoryFact, map[string]string{}, 3, "test"); len(ch) != 1 {
		t.Fatalf("un-declare: %v", ch)
	}
	p, _ = ks.PredicateProfile(ctx, a1NS, domain.KindMemoryFact, "employer")
	if p.Source != domain.ProfileSourceLearned || p.Cardinality != domain.CardinalitySingle {
		t.Fatalf("reverted = %+v", p)
	}
	putAll(t, ks, ctx, a1Fact("employer", "Acme", "a1-c1", a1Day("2020-01-01")), a1Fact("employer", "Globex", "a1-c2", a1Day("2021-01-01")),
		a1Fact("employer", "Initech", "a1-c3", a1Day("2022-01-01"), "other"))
	if n, err := ks.RederivePredicate(ctx, a1NS, domain.KindMemoryFact, "employer", 0); err != nil || n != 2 {
		t.Fatalf("RederivePredicate = %d %v, want the 2 keys (two classifications)", n, err)
	}
}

// A1.5: scope IN SQL on the item search and on the chain read. A secret item never comes
// back to a reader whose predicate forbids it, even though it is the nearest.
func TestPg_FactItems_ScopeAppliedInSQL(t *testing.T) {
	ks, r, ctx := newFactSubstrate(t)
	pub := a1Fact("pet", "cat", "a1-c1", a1Day("2023-01-01"), "public")
	sec := a1Fact("pet", "dragon", "a1-c2", a1Day("2023-01-01"), "secret")
	putAll(t, ks, ctx, pub, sec)
	if err := ks.PutItemEmbeddings(ctx, []domain.ItemEmbedding{
		{ItemID: pub.ID, ModelID: "a1-model", Vector: unitVec(2)},
		{ItemID: sec.ID, ModelID: "a1-model", Vector: unitVec(1)},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := r.SearchFacts(ctx, unitVec(1), domain.FactSearchOptions{TopK: 5, Scope: &domain.TagPredicate{ForbiddenTags: []string{"secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Fact.ID != string(pub.ID) {
		t.Fatalf("hits = %+v, want only the public item", hits)
	}
	all, _ := r.SearchFacts(ctx, unitVec(1), domain.FactSearchOptions{TopK: 5, Scope: &domain.TagPredicate{Bypass: true}})
	if len(all) != 2 || all[0].Fact.ID != string(sec.ID) || all[0].Score < 0.99 {
		t.Fatalf("bypass hits = %+v, want both, the secret one nearest", all)
	}
	if req, _ := r.SearchFacts(ctx, unitVec(1), domain.FactSearchOptions{TopK: 5, Scope: &domain.TagPredicate{RequiredTags: []string{"public"}}}); len(req) != 1 {
		t.Fatalf("required tags: %+v", req)
	}
	if c, _ := r.FactChain(ctx, domain.FactFromItem(sec), &domain.TagPredicate{ForbiddenTags: []string{"secret"}}); len(c) != 0 {
		t.Fatalf("a forbidden key's chain must read empty: %+v", c)
	}
	// A wrong-dimension query vector reads nothing rather than erroring on the cast.
	if h, err := r.SearchFacts(ctx, []float32{1, 0}, domain.FactSearchOptions{Scope: &domain.TagPredicate{}}); err != nil || len(h) != 0 {
		t.Fatalf("wrong dimension: %v %v", h, err)
	}
	// Fuzzy candidates: same subject reference, another predicate.
	home := a1Fact("home_city", "Paris", "a1-c3", a1Day("2023-01-01"), "public")
	putAll(t, ks, ctx, home)
	_ = ks.PutItemEmbeddings(ctx, []domain.ItemEmbedding{{ItemID: home.ID, ModelID: "a1-model", Vector: unitVec(3)}})
	sim, err := r.SimilarFacts(ctx, domain.FactFromItem(home), unitVec(2), 5, &domain.TagPredicate{Bypass: true})
	if err != nil || len(sim) != 1 || sim[0].ID != string(pub.ID) {
		t.Fatalf("similar = %+v err %v, want the public pet fact only (the secret one is another classification)", sim, err)
	}
}

// A1-Q4 / ADR-0137 Q5 on the store: erasing the statement that closed a value — here
// across keys, through a contradictory verdict — deletes the assessment citing it and
// leaves a content-free sentinel, so the closed value stays closed.
func TestPg_FactErasure_SentinelKeepsChainClosed(t *testing.T) {
	ks, r, ctx := newFactSubstrate(t)
	paris := a1Fact("lives_in", "Paris", "a1-c1", a1Day("2022-01-01"))
	berlin := a1Fact("moved_to", "Berlin", "a1-c2", a1Day("2024-02-01"))
	putAll(t, ks, ctx, paris, berlin)
	inputs := []domain.KnowledgeItemID{berlin.ID, paris.ID}
	if _, err := ks.PutAssessment(ctx, domain.Assessment{
		ID: domain.AssessmentID("llm_contradiction", "m", "h", inputs), NamespaceID: a1NS, Kind: domain.KindMemoryFact,
		Authority: "llm_contradiction", AuthorityVersion: "m", PromptHash: "h", InputItemIDs: inputs,
		Verdicts: []domain.AssessmentVerdict{{A: berlin.ID, B: paris.ID, Verdict: domain.VerdictContradictory}},
	}); err != nil {
		t.Fatal(err)
	}
	if c := chainByValue(t, r, ctx, paris, &domain.TagPredicate{}); c["Paris"].Current {
		t.Fatalf("precondition: paris closed by berlin: %+v", c)
	}
	// Two keys derived TOGETHER (as a search whose hits span both does) must agree with
	// each derived alone.
	_ = ks.PutItemEmbeddings(ctx, []domain.ItemEmbedding{{ItemID: paris.ID, ModelID: "a1-model", Vector: unitVec(5)}, {ItemID: berlin.ID, ModelID: "a1-model", Vector: unitVec(5)}})
	hits, err := r.SearchFacts(ctx, unitVec(5), domain.FactSearchOptions{TopK: 5, Scope: &domain.TagPredicate{}})
	if err != nil || len(hits) != 2 {
		t.Fatalf("joint search: %+v %v", hits, err)
	}
	for _, h := range hits {
		if h.Fact.Value == "Paris" && (h.Fact.Current || h.SupersededByValue != "Berlin") {
			t.Fatalf("joint derivation re-opened paris: %+v", h)
		}
	}
	n, err := ks.EraseItems(ctx, a1NS, domain.KindMemoryFact, domain.KnowledgeErasure{Entities: []string{berlin.EntityID}})
	if err != nil || n != 1 {
		t.Fatalf("EraseItems = %d %v", n, err)
	}
	var asm int
	_ = ks.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_assessments WHERE namespace_id = $1`, a1NS).Scan(&asm)
	if asm != 0 {
		t.Fatalf("an assessment citing the erased item survived (%d)", asm)
	}
	c := chainByValue(t, r, ctx, paris, &domain.TagPredicate{})
	if p := c["Paris"]; p.Current || p.ValidTo == nil || !p.ValidTo.Equal(*a1Day("2024-02-01")) {
		t.Fatalf("paris re-opened after the erasure: %+v", p)
	}
	item, reason, _, _ := currentRow(t, ks, ctx, paris.EntityID, "alice")
	if item != nil || reason != domain.ReasonFactErased {
		t.Fatalf("current resolution = %v %q, want none/erased", item, reason)
	}
	// The sentinel carries no content: its only values are the marker and the target id.
	var leaked int
	_ = ks.pool.QueryRow(ctx, `SELECT count(*) FROM statement_values sv JOIN knowledge_items ki ON ki.id = sv.item_id
		WHERE ki.namespace_id = $1 AND ki.id LIKE 'ki_fact_erased:%' AND sv.predicate NOT IN ('erased', 'erased_closes')`, a1NS).Scan(&leaked)
	if leaked != 0 {
		t.Fatalf("the sentinel carries %d content values", leaked)
	}
}

// The backfill lister (A1.7): documents with content chunks and no memory_fact marker,
// chunk ids in ingest (-chunk-N) order, the recorded author, marked documents excluded.
func TestPg_FactBackfillLister_UnmarkedInIngestOrder(t *testing.T) {
	ks, _, ctx := newFactSubstrate(t)
	for _, q := range []string{
		`INSERT INTO documents (id, source_type, tags) VALUES ('a1test-doc1', 'a1src', ARRAY['t1']), ('a1test-doc2', 'a1src', ARRAY['t1'])`,
		`INSERT INTO chunks (id, document_id, text, metadata) VALUES
			('a1test-doc1-chunk-10', 'a1test-doc1', 'ten', '{"source_agent_id":"lme-a1-q1"}'),
			('a1test-doc1-chunk-2', 'a1test-doc1', 'two', '{"source_agent_id":"lme-a1-q1"}'),
			('a1test-doc1-chunk-1', 'a1test-doc1', 'one', '{"source_agent_id":"lme-a1-q1"}'),
			('a1test-doc2-chunk-1', 'a1test-doc2', 'x', '{"source_agent_id":"other"}')`,
	} {
		if _, err := ks.pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	cands, err := ks.ListFactBackfillCandidates(ctx, domain.FactBackfillFilter{SourceType: "a1src", Namespace: "lme-a1-"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].DocumentID != "a1test-doc1" || cands[0].Author != "lme-a1-q1" ||
		!reflect.DeepEqual(cands[0].ChunkIDs, []string{"a1test-doc1-chunk-1", "a1test-doc1-chunk-2", "a1test-doc1-chunk-10"}) {
		t.Fatalf("candidates = %+v", cands)
	}
	if c := cands[0]; c.TextChars != len("onetwoten") || c.TimeSource != "" || c.SourceMeta().ReferenceBasis != domain.ReferenceBasisIngested {
		t.Fatalf("source metadata of an undated document = %+v", c)
	}
	if m, err := ks.ExtractionMarker(ctx, "", FactBackfillExtractor, "a1test-doc1", "h"); m != nil || err != nil {
		t.Fatalf("no marker reads as (nil, nil): %+v %v", m, err)
	}
	if err := ks.MarkExtracted(ctx, domain.ExtractionMarker{Extractor: FactBackfillExtractor, DocumentID: "a1test-doc1", ContentHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if m, _ := ks.ExtractionMarker(ctx, "", FactBackfillExtractor, "a1test-doc1", "h"); m == nil {
		t.Fatal("marker not readable")
	}
	if cands, _ := ks.ListFactBackfillCandidates(ctx, domain.FactBackfillFilter{SourceType: "a1src", Namespace: "lme-a1-"}); len(cands) != 0 {
		t.Fatalf("a marked document must be excluded: %+v", cands)
	}
	// A2.4: with the running version, a legacy ('') marker, a marker of another version
	// and a partial marker keep the document a candidate; a complete one excludes it.
	v2 := domain.FactBackfillFilter{SourceType: "a1src", Namespace: "lme-a1-", ExtractorVersion: "fact-extract-v2/m"}
	if cands, _ := ks.ListFactBackfillCandidates(ctx, v2); len(cands) != 1 {
		t.Fatalf("a legacy marker must keep the document pending: %+v", cands)
	}
	for _, c := range []struct {
		m       domain.ExtractionMarker
		pending bool
	}{
		{domain.ExtractionMarker{ExtractorVersion: "fact-extract-v1/m", PassagesCovered: 3, PassagesTotal: 3}, true},
		{domain.ExtractionMarker{ExtractorVersion: "fact-extract-v2/m", PassagesCovered: 1, PassagesTotal: 3, Items: 4}, true},
		{domain.ExtractionMarker{ExtractorVersion: "fact-extract-v2/m", PassagesCovered: 3, PassagesTotal: 3, Items: 5}, false},
	} {
		c.m.Extractor, c.m.DocumentID, c.m.ContentHash = FactBackfillExtractor, "a1test-doc1", "h"
		if err := ks.MarkExtracted(ctx, c.m); err != nil { // an upsert over the same content
			t.Fatal(err)
		}
		got, _ := ks.ExtractionMarker(ctx, "", FactBackfillExtractor, "a1test-doc1", "h")
		if got == nil || got.ExtractorVersion != c.m.ExtractorVersion || got.PassagesCovered != c.m.PassagesCovered ||
			got.PassagesTotal != c.m.PassagesTotal || got.Items != c.m.Items {
			t.Fatalf("upserted marker = %+v, want %+v", got, c.m)
		}
		if cands, _ := ks.ListFactBackfillCandidates(ctx, v2); (len(cands) == 1) != c.pending {
			t.Fatalf("marker %+v: candidates %+v, want pending=%v", c.m, cands, c.pending)
		}
	}
	if cands, _ := ks.ListFactBackfillCandidates(ctx, domain.FactBackfillFilter{SourceType: "a1src", IncludeMarked: true}); len(cands) != 2 {
		t.Fatalf("include-marked: %+v", cands)
	}
}

// a1FactBy is a fact item asserted by actor (its own chunk, so ids never collide).
func a1FactBy(actor, pred, value, chunk string, from *time.Time) domain.KnowledgeItem {
	f := domain.Fact{
		Subject: "user", Attribute: pred, Value: value, SubjectKey: "user",
		AttributeKey: domain.NormalizeFactAttribute(pred), ValueNorm: domain.NormalizeFactText(value),
		ValidFrom: from, AssertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Actor: actor,
		SourceChunkID: chunk, Quote: value,
	}
	return f.ToItem(a1NS)
}

// ADR-0136 A2-Q3: the hard reset deletes, for the selected principals only, the items
// (values and embeddings cascade), the resolutions on their keys, the assessments that
// cite them, their documents' markers and (by default) learned profiles — in one
// transaction; a dry run counts the same and changes nothing; a surviving key cited by
// a deleted assessment is re-derived.
func TestPg_FactReset_SelectedPrincipalsOnly(t *testing.T) {
	ks, _, ctx := newFactSubstrate(t)
	gone1 := a1FactBy("lme-a1-q1", "phone", "iPhone", "a1test-r1", a1Day("2024-01-01"))
	gone2 := a1FactBy("lme-a1-q1", "phone", "Pixel", "a1test-r2", a1Day("2024-03-01"))
	kept := a1FactBy("keeper", "phone", "Nokia", "a1test-r3", a1Day("2024-02-01"))
	putAll(t, ks, ctx, gone1, gone2, kept)
	if err := ks.PutItemEmbeddings(ctx, []domain.ItemEmbedding{{ItemID: gone1.ID, ModelID: "a1-model", Vector: make([]float32, eventTestDim)}}); err != nil {
		t.Fatal(err)
	}
	// One assessment over the selected principal's pair, one that also cites the kept item.
	for _, in := range [][]domain.KnowledgeItemID{{gone1.ID, gone2.ID}, {gone2.ID, kept.ID}} {
		a := domain.Assessment{
			ID: domain.AssessmentID(domain.AssessmentAuthorityLLMContradiction, "m|v", "h", in), NamespaceID: a1NS,
			Kind: domain.KindMemoryFact, Authority: domain.AssessmentAuthorityLLMContradiction, AuthorityVersion: "m|v",
			PromptHash: "h", InputItemIDs: in, RawOutput: "{}",
			Verdicts: []domain.AssessmentVerdict{{A: in[0], B: in[1], Verdict: domain.VerdictContradictory}},
		}
		if _, err := ks.PutAssessment(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ks.VoteProfile(ctx, a1NS, domain.KindMemoryFact, "a1reset_pred", domain.ProfileVote{AssessmentID: "x", Single: 1}, 1); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO documents (id, source_type) VALUES ('a1test-rd1', 'a1src'), ('a1test-rd2', 'a1src')`,
		`INSERT INTO chunks (id, document_id, text, metadata) VALUES
			('a1test-rd1-chunk-1', 'a1test-rd1', 'x', '{"source_agent_id":"lme-a1-q1"}'),
			('a1test-rd2-chunk-1', 'a1test-rd2', 'y', '{"source_agent_id":"keeper"}')`,
	} {
		if _, err := ks.pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for _, d := range []string{"a1test-rd1", "a1test-rd2"} {
		if err := ks.MarkExtracted(ctx, domain.ExtractionMarker{Extractor: FactBackfillExtractor, DocumentID: d, ContentHash: "h", ExtractorVersion: "v"}); err != nil {
			t.Fatal(err)
		}
	}
	counts := func() (items, embs, res, asm, markers, learned int) {
		q := func(sql string, dst *int) {
			if err := ks.pool.QueryRow(ctx, sql).Scan(dst); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		q(`SELECT count(*) FROM knowledge_items WHERE namespace_id = 'a1test'`, &items)
		q(`SELECT count(*) FROM knowledge_item_embeddings e JOIN knowledge_items i ON i.id = e.item_id WHERE i.namespace_id = 'a1test'`, &embs)
		q(`SELECT count(*) FROM resolutions WHERE namespace_id = 'a1test'`, &res)
		q(`SELECT count(*) FROM knowledge_assessments WHERE namespace_id = 'a1test'`, &asm)
		q(`SELECT count(*) FROM knowledge_extractions WHERE document_id LIKE 'a1test-rd%'`, &markers)
		q(`SELECT count(*) FROM predicate_profiles WHERE namespace_id = 'a1test' AND source = 'learned'`, &learned)
		return
	}
	i0, e0, r0, a0, m0, l0 := counts()
	if i0 != 3 || e0 != 1 || a0 != 2 || m0 != 2 || l0 == 0 || r0 == 0 {
		t.Fatalf("fixture: items %d embs %d res %d asm %d markers %d learned %d", i0, e0, r0, a0, m0, l0)
	}

	if _, err := ks.ResetFacts(ctx, domain.FactResetFilter{}); err == nil {
		t.Fatal("a reset with no selector must be refused")
	}
	dry, err := ks.ResetFacts(ctx, domain.FactResetFilter{Namespace: "lme-a1-", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Items != 2 || dry.Assessments != 2 || dry.Markers != 1 || dry.Profiles < 1 || dry.Resolutions == 0 || dry.Rederived < 1 {
		t.Fatalf("dry run = %+v", dry)
	}
	if i, e, r, a, m, l := counts(); i != i0 || e != e0 || r != r0 || a != a0 || m != m0 || l != l0 {
		t.Fatal("a dry run changed the database")
	}

	rep, err := ks.ResetFacts(ctx, domain.FactResetFilter{Namespace: "lme-a1-", KeepLearned: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 2 || rep.Assessments != 2 || rep.Markers != 1 || rep.Profiles != 0 || rep.Rederived != 1 {
		t.Fatalf("reset = %+v", rep)
	}
	i, e, _, a, m, l := counts()
	if i != 1 || e != 0 || a != 0 || m != 1 || l != l0 {
		t.Fatalf("after reset: items %d embs %d asm %d markers %d learned %d", i, e, a, m, l)
	}
	if got, _ := ks.GetItem(ctx, kept.ID); got == nil {
		t.Fatal("another principal's fact was deleted")
	}
	item, _, _, _ := currentRow(t, ks, ctx, kept.EntityID, "keeper")
	if item == nil || *item != string(kept.ID) {
		t.Fatalf("the surviving key must be re-derived and current: %v", item)
	}
	var orphan int
	_ = ks.pool.QueryRow(ctx, `SELECT count(*) FROM resolutions WHERE namespace_id = 'a1test' AND actor = 'lme-a1-q1'`).Scan(&orphan)
	if orphan != 0 {
		t.Fatalf("%d resolution rows of the reset principal survived", orphan)
	}
	// Default: learned profiles go too (owner answer A2-Q2).
	rep, err = ks.ResetFacts(ctx, domain.FactResetFilter{Principal: "nobody"})
	if err != nil || rep.Profiles < 1 || rep.Items != 0 {
		t.Fatalf("reset of learned profiles = %+v %v", rep, err)
	}
	if _, _, _, _, _, l := counts(); l != 0 {
		t.Fatalf("learned profiles left: %d", l)
	}
}
