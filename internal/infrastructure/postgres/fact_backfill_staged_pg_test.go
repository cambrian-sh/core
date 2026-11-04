package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory"
)

// The two-phase ordered backfill against the serial ordered run on a REAL, migrated
// Postgres (T-20261103-fact-backfill-processes-each-principal-s): the same corpus and
// deterministic model through memory.FactExtractor over PgKnowledgeStore and
// PgFactItemReader (behind the real authz decorators), once serially and once in two
// phases, must leave identical knowledge_items (+ statement values), knowledge_assessments
// (ids, inputs, verdicts, prompt hashes, raw output), CURRENT resolutions,
// current predicate_profiles and extraction markers. The corpus mirrors the unit test's
// (internal/memory/fact_backfill_staged_test.go): a predicate learned single after three
// votes, one learned multi, a session-time tie, a two-window document, fuzzy candidates
// with ties in distance (identical fact text), and an earlier run's later document.

var pgStagedText = map[string]string{
	"a1test-c1": "I live in Paris and I like jazz.", "a1test-c2": "I live in London now and I like rock.",
	"a1test-c3": "I live in Berlin.", "a1test-c4": "I live in Rome.", "a1test-c5": "I live in Madrid and I like rock.",
	"a1test-c6": "I like pop.", "a1test-c7": "I work at Acme.", "a1test-c8a": "I work at Globex.",
	"a1test-c8b": "My job title is engineer.", "a1test-c9": "I live in Oslo.",
}

var pgStagedFacts = map[string][][3]string{
	"a1test-c1":  {{"home_city", "Paris", "I live in Paris"}, {"likes", "jazz", "I like jazz"}},
	"a1test-c2":  {{"home_city", "London", "I live in London"}, {"likes", "rock", "I like rock"}},
	"a1test-c3":  {{"home_city", "Berlin", "I live in Berlin"}},
	"a1test-c4":  {{"home_city", "Rome", "I live in Rome"}},
	"a1test-c5":  {{"home_city", "Madrid", "I live in Madrid"}, {"likes", "rock", "I like rock"}},
	"a1test-c6":  {{"likes", "pop", "I like pop"}},
	"a1test-c7":  {{"employer", "Acme", "I work at Acme"}},
	"a1test-c8a": {{"employer", "Globex", "I work at Globex"}},
	"a1test-c8b": {{"job_title", "engineer", "My job title is engineer"}},
	"a1test-c9":  {{"home_city", "Oslo", "I live in Oslo"}},
}

var (
	pgNewRe  = regexp.MustCompile(`(?m)^N(\d+): [^|]+\| ([^|]+) \|`)
	pgCandRe = regexp.MustCompile(`(?m)^  C(\d+)\.(\d+): [^|]+\| ([^|]+) \|`)
)

type pgStagedModel struct{}

func (pgStagedModel) Generate(_ context.Context, prompt string) (string, error) {
	if strings.Contains(prompt, "give exactly one verdict") {
		newAttr := map[string]string{}
		for _, m := range pgNewRe.FindAllStringSubmatch(prompt, -1) {
			newAttr[m[1]] = strings.TrimSpace(m[2])
		}
		var vs []string
		for _, m := range pgCandRe.FindAllStringSubmatch(prompt, -1) {
			a, b := newAttr[m[1]], strings.TrimSpace(m[3])
			v := "uncertain"
			switch {
			case a == b && (a == "home_city" || a == "employer"):
				v = "contradictory"
			case a == b && a == "likes", (a == "employer" && b == "job_title") || (a == "job_title" && b == "employer"):
				v = "consistent"
			}
			vs = append(vs, fmt.Sprintf(`{"new":"N%s","candidate":"C%s.%s","verdict":"%s"}`, m[1], m[1], m[2], v))
		}
		return `{"verdicts":[` + strings.Join(vs, ",") + `]}`, nil
	}
	ids := make([]string, 0, len(pgStagedText))
	for id := range pgStagedText {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if strings.Contains(prompt, pgStagedText[id]) {
			var fs []string
			for _, f := range pgStagedFacts[id] {
				fs = append(fs, fmt.Sprintf(`{"subject":"user","attribute":%q,"value":%q,"passage":1,"quote":%q}`, f[0], f[1], f[2]))
			}
			return `{"facts":[` + strings.Join(fs, ",") + `]}`, nil
		}
	}
	return `{"facts":[]}`, nil
}

// pgHashEmbedder is deterministic: the same text always embeds identically (the ties the
// (distance, id) order settles), different texts differently.
type pgHashEmbedder struct{}

func (pgHashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	sum := sha256.Sum256([]byte(text))
	v := make([]float32, eventTestDim)
	for i := range v {
		v[i] = float32(sum[i%len(sum)])/255 + 0.01
	}
	return v, nil
}

// pgStagedChunks is a chunk reader for the backfill (only GetBatch is used).
type pgStagedChunks struct{ domain.VectorStore }

func (pgStagedChunks) GetBatch(_ context.Context, ids []string) ([]domain.Document, error) {
	var out []domain.Document
	for _, id := range ids {
		if t, ok := pgStagedText[id]; ok {
			out = append(out, domain.Document{ID: id, Text: t, DocumentType: domain.DocTypeMnemonicFact,
				Metadata: map[string]any{"tags": []string{"q1"}, "source_agent_id": "lme-q1"}})
		}
	}
	return out, nil
}

type pgStagedLister struct {
	cands []domain.FactBackfillCandidate
}

func (l pgStagedLister) ListFactBackfillCandidates(context.Context, domain.FactBackfillFilter) ([]domain.FactBackfillCandidate, error) {
	return append([]domain.FactBackfillCandidate(nil), l.cands...), nil
}

func pgStagedCand(doc string, day int, chunks ...string) domain.FactBackfillCandidate {
	return domain.FactBackfillCandidate{DocumentID: doc, ChunkIDs: chunks, Tags: []string{"q1"}, Author: "lme-q1",
		SessionTime: time.Date(2023, 1, day, 0, 0, 0, 0, time.UTC), TimeSource: domain.TimeSourceExplicit}
}

func runPgStaged(t *testing.T, ks *PgKnowledgeStore, r *PgFactItemReader, ctx context.Context, gc int) memory.FactBackfillReport {
	t.Helper()
	e := memory.NewFactExtractor(pgStagedModel{}, pgHashEmbedder{}, authz.NewEnforcingKnowledgeWriter(ks, nil, nil),
		authz.NewEnforcingFactItemReader(r, nil), memory.FactExtractorConfig{
			Supersession: true, Assessments: true, FuzzyCandidates: 2, CardinalityMinVotes: 3, MaxPromptChars: 100,
			Namespace: a1NS, EmbedModelID: "a1-model", AssessorModel: "judge", GeneratorModel: "gen",
		})
	pre := memory.NewFactBackfill(pgStagedLister{[]domain.FactBackfillCandidate{pgStagedCand("a1test-d9", 9, "a1test-c9")}}, pgStagedChunks{}, e)
	if rep, err := pre.Run(ctx, memory.FactBackfillOptions{Ordered: true}); err != nil || rep.Extracted != 1 {
		t.Fatalf("pre-run: %+v %v", rep, err)
	}
	main := []domain.FactBackfillCandidate{
		pgStagedCand("a1test-d5", 5, "a1test-c5"), pgStagedCand("a1test-d1", 1, "a1test-c1"), pgStagedCand("a1test-d7", 6, "a1test-c7"),
		pgStagedCand("a1test-d3", 3, "a1test-c3"), pgStagedCand("a1test-d6", 6, "a1test-c6"), pgStagedCand("a1test-d2", 2, "a1test-c2"),
		pgStagedCand("a1test-d8", 7, "a1test-c8a", "a1test-c8b"), pgStagedCand("a1test-d4", 4, "a1test-c4"),
	}
	rep, err := memory.NewFactBackfill(pgStagedLister{main}, pgStagedChunks{}, e).Run(ctx,
		memory.FactBackfillOptions{Ordered: true, Concurrency: 1, GroupConcurrency: gc})
	if err != nil || rep.Extracted != 8 || rep.Failed != 0 || rep.TwoPhase != (gc > 1) {
		t.Fatalf("run (group concurrency %d): %+v %v", gc, rep, err)
	}
	return rep
}

// pgStagedState is the compared state, as sorted text rows.
func pgStagedState(t *testing.T, ks *PgKnowledgeStore, ctx context.Context) map[string][]string {
	t.Helper()
	qs := map[string]string{
		"items": `SELECT ki.id || '|' || ki.entity_id || '|' || ki.asserted_by || '|' || ki.asserted_at::text || '|' ||
			ki.source_ref || '|' || ki.negation::text || '|' || array_to_string(ki.classification, ',') || '|' ||
			coalesce(ki.valid_from::text, '') || '|' || coalesce(ki.valid_to::text, '') || '|' ||
			coalesce((SELECT string_agg(sv.predicate || '=' || coalesce(sv.value_text, '') || coalesce(sv.value_entity_id, '') ||
				coalesce(sv.value_number::text, '') || coalesce(sv.value_date::text, ''), ';' ORDER BY sv.predicate, sv.value_text, sv.value_entity_id)
				FROM statement_values sv WHERE sv.item_id = ki.id), '')
			FROM knowledge_items ki WHERE ki.namespace_id = 'a1test'`,
		"assessments": `SELECT id || '|' || array_to_string(input_item_ids, ',') || '|' || verdicts::text || '|' || prompt_hash || '|' ||
			raw_output || '|' || array_to_string(classification, ',') FROM knowledge_assessments WHERE namespace_id = 'a1test'`,
		"resolutions": `SELECT entity_id || '|' || actor || '|' || coalesce(item_id, '') || '|' || reason_code || '|' ||
			coalesce(valid_from::text, '') || '|' || coalesce(valid_to::text, '') FROM resolutions
			WHERE namespace_id = 'a1test' AND system_to IS NULL`,
		"profiles": `SELECT predicate || '|' || cardinality || '|' || source || '|' || single_votes || '|' || multi_votes || '|' ||
			array_to_string(evidence_assessment_ids, ',') FROM predicate_profiles WHERE namespace_id = 'a1test' AND system_to IS NULL`,
		"markers": `SELECT document_id || '|' || content_hash || '|' || items || '|' || extractor_version || '|' ||
			coalesce(passages_covered, -1) || '|' || coalesce(passages_total, -1) FROM knowledge_extractions WHERE namespace_id = 'a1test'`,
		"journal": `SELECT document_id || '|' || passage_start FROM knowledge_extraction_windows WHERE namespace_id = 'a1test'`,
	}
	out := map[string][]string{}
	for name, q := range qs {
		rows, err := ks.pool.Query(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			out[name] = append(out[name], s)
		}
		rows.Close()
		sort.Strings(out[name])
	}
	return out
}

func pgStagedReset(t *testing.T, ks *PgKnowledgeStore, ctx context.Context) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM resolutions WHERE namespace_id = 'a1test'`,
		`DELETE FROM knowledge_assessments WHERE namespace_id = 'a1test'`,
		`DELETE FROM predicate_profiles WHERE namespace_id = 'a1test'`,
		`DELETE FROM knowledge_items WHERE namespace_id = 'a1test'`,
		`DELETE FROM knowledge_extractions WHERE namespace_id = 'a1test'`,
		`DELETE FROM knowledge_extraction_windows WHERE namespace_id = 'a1test'`,
	} {
		if _, err := ks.pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestPg_FactBackfillTwoPhase_EquivalentToSerialOrdered(t *testing.T) {
	ks, r, ctx := newFactSubstrate(t)
	pgStagedReset(t, ks, ctx)
	t.Cleanup(func() { pgStagedReset(t, ks, context.Background()) })

	runPgStaged(t, ks, r, ctx, 1)
	want := pgStagedState(t, ks, ctx)
	if len(want["assessments"]) < 6 || len(want["resolutions"]) == 0 || len(want["journal"]) != 0 {
		t.Fatalf("fixture: %d assessments, %d resolutions, %d journal rows", len(want["assessments"]), len(want["resolutions"]), len(want["journal"]))
	}
	learned := strings.Join(want["profiles"], "\n")
	if !strings.Contains(learned, "home_city|single|learned") || !strings.Contains(learned, "likes|multi|learned") {
		t.Fatalf("fixture: profiles %v, want home_city learned single and likes learned multi", want["profiles"])
	}

	for _, gc := range []int{3, 8} {
		pgStagedReset(t, ks, ctx)
		rep := runPgStaged(t, ks, r, ctx, gc)
		if rep.SpeculationWaves < 2 {
			t.Fatalf("group concurrency %d: expected a re-speculation after the cardinality flip: %+v", gc, rep)
		}
		got := pgStagedState(t, ks, ctx)
		for _, name := range []string{"items", "assessments", "resolutions", "profiles", "markers", "journal"} {
			if !reflect.DeepEqual(want[name], got[name]) {
				t.Errorf("group concurrency %d: %s differ\nserial:\n  %s\ntwo-phase:\n  %s", gc, name,
					strings.Join(want[name], "\n  "), strings.Join(got[name], "\n  "))
			}
		}
	}
}

// The journal round trip and FactsByID / SimilarFactsHiding on the real store.
func TestPg_ExtractionJournalAndStagedReads(t *testing.T) {
	ks, r, ctx := newFactSubstrate(t)
	pgStagedReset(t, ks, ctx)
	t.Cleanup(func() { pgStagedReset(t, ks, context.Background()) })
	w := domain.ExtractionWindow{NamespaceID: a1NS, Extractor: domain.KindMemoryFact, DocumentID: "a1test-dj", ContentHash: "h",
		ExtractorVersion: "v1", PassageStart: 2, PassageEnd: 5, ItemIDs: []domain.KnowledgeItemID{"b", "a"}, NewItemIDs: []domain.KnowledgeItemID{"a"}}
	w0 := w
	w0.PassageStart, w0.PassageEnd, w0.ItemIDs, w0.NewItemIDs = 0, 2, nil, nil
	for _, row := range []domain.ExtractionWindow{w, w0, w} { // the second w is an idempotent upsert
		if err := ks.PutExtractionWindow(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ks.ExtractionWindows(ctx, a1NS, domain.KindMemoryFact, "a1test-dj", "h", "v1")
	if err != nil || len(got) != 2 || got[0].PassageStart != 0 || !reflect.DeepEqual(got[1].ItemIDs, w.ItemIDs) ||
		!reflect.DeepEqual(got[1].NewItemIDs, w.NewItemIDs) || len(got[0].ItemIDs) != 0 {
		t.Fatalf("journal = %+v %v", got, err)
	}
	if other, _ := ks.ExtractionWindows(ctx, a1NS, domain.KindMemoryFact, "a1test-dj", "h", "v2"); len(other) != 0 {
		t.Fatalf("another version's rows leaked: %+v", other)
	}
	if err := ks.DeleteExtractionWindows(ctx, a1NS, domain.KindMemoryFact, "a1test-dj", "h"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ks.ExtractionWindows(ctx, a1NS, domain.KindMemoryFact, "a1test-dj", "h", "v1"); len(got) != 0 {
		t.Fatalf("rows survived the delete: %+v", got)
	}

	// Staged reads: a subject with three other-predicate facts; hiding one removes it
	// from the ranking and the next one moves up.
	focus := a1Fact("home_city", "Paris", "a1test-s1", a1Day("2023-01-01"))
	others := []domain.KnowledgeItem{
		a1Fact("likes", "jazz", "a1test-s2", a1Day("2023-01-02")),
		a1Fact("employer", "Acme", "a1test-s3", a1Day("2023-01-03")),
		a1Fact("pet", "Luna", "a1test-s4", a1Day("2023-01-04")),
	}
	putAll(t, ks, ctx, append([]domain.KnowledgeItem{focus}, others...)...)
	vec, _ := pgHashEmbedder{}.Embed(ctx, "same")
	var embs []domain.ItemEmbedding
	for _, it := range append([]domain.KnowledgeItem{focus}, others...) {
		embs = append(embs, domain.ItemEmbedding{ItemID: it.ID, ModelID: "a1-model", Vector: vec}) // all ties: ordered by id
	}
	if err := ks.PutItemEmbeddings(ctx, embs); err != nil {
		t.Fatal(err)
	}
	ids := []string{string(others[0].ID), string(others[1].ID), string(others[2].ID)}
	sort.Strings(ids)
	f := domain.FactFromItem(focus)
	all, err := r.SimilarFacts(ctx, f, vec, 2, domain.ScopeSystem)
	if err != nil || len(all) != 2 || all[0].ID != ids[0] || all[1].ID != ids[1] {
		t.Fatalf("SimilarFacts = %v %v, want the two smallest ids %v (ties by id)", factIDs(all), err, ids[:2])
	}
	hid, err := r.SimilarFactsHiding(ctx, f, vec, 2, domain.ScopeSystem, []domain.KnowledgeItemID{domain.KnowledgeItemID(ids[0])})
	if err != nil || len(hid) != 2 || hid[0].ID != ids[1] || hid[1].ID != ids[2] {
		t.Fatalf("SimilarFactsHiding = %v %v, want %v", factIDs(hid), err, ids[1:])
	}
	byID, err := r.FactsByID(ctx, []domain.KnowledgeItemID{focus.ID, "missing"}, domain.ScopeSystem)
	if err != nil || len(byID) != 1 || byID[0].ID != string(focus.ID) || len(byID[0].Embedding) != eventTestDim || byID[0].Value != "Paris" {
		t.Fatalf("FactsByID = %+v %v", byID, err)
	}
}

func factIDs(fs []domain.Fact) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.ID
	}
	return out
}
