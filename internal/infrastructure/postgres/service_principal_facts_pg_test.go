package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory"
)

// ENG-494 end to end on a REAL, migrated Postgres (owner decision 2026-11-15): two
// connectors that ingest the same statement each write facts as their OWN source
// service principal — memory.FactExtractor through the enforcing knowledge writer
// (ClassifyWrite) into PgKnowledgeStore — so each fact's actor is source:<c>:<id>,
// its classification carries provenance:source=source:<c>:<id>, and supersession runs
// per source: telegram's later "London" supersedes telegram's "Paris" and leaves
// slack's "Paris" current. The operator cost guard keeps a third connector out of
// extraction.

var spTexts = map[string]string{
	"a1test-sp-tg1": "I live in Paris.",
	"a1test-sp-sl1": "I live in Paris.",
	"a1test-sp-tg2": "I live in London now.",
	"a1test-sp-mx1": "I live in Rome.",
}

type spModel struct{}

func (spModel) Generate(_ context.Context, prompt string) (string, error) {
	for _, city := range []string{"London", "Paris", "Rome"} {
		if strings.Contains(prompt, "I live in "+city) {
			return fmt.Sprintf(`{"facts":[{"subject":"user","attribute":"home_city","value":%q,"passage":1,"quote":"I live in %s"}]}`, city, city), nil
		}
	}
	return `{"facts":[]}`, nil
}

// spChunk is a stored chunk as the ingest path leaves it under a source principal:
// writer of record = the source, tags = the classification ClassifyWrite stamped.
func spChunk(id string, src domain.PrincipalRef) *domain.Document {
	tags, _ := domain.AllowAllAuthorizer{}.ClassifyWrite(context.Background(), src, []string{"q1"})
	return &domain.Document{ID: id, Text: spTexts[id], DocumentType: domain.DocTypeMnemonicFact,
		Metadata: map[string]any{"tags": tags, "source_agent_id": src.WriterID()}}
}

func TestPg_SourcePrincipalFacts_SupersedePerSource(t *testing.T) {
	ks, r, ctx := newFactSubstrate(t)
	pgStagedReset(t, ks, ctx)
	t.Cleanup(func() { pgStagedReset(t, ks, context.Background()) })

	e := memory.NewFactExtractor(spModel{}, pgHashEmbedder{}, authz.NewEnforcingKnowledgeWriter(ks, domain.AllowAllAuthorizer{}, nil),
		authz.NewEnforcingFactItemReader(r, nil), memory.FactExtractorConfig{
			Supersession: true, SingleAttributes: []string{"home_city"}, Workers: 1,
			Namespace: a1NS, EmbedModelID: "a1-model", AssessorModel: "judge", GeneratorModel: "gen",
			ExtractionSources: []string{"source:telegram", "source:slack"},
		})
	e.Start(ctx)
	t.Cleanup(e.Stop)

	tg := domain.SourcePrincipal("telegram", "bot1")
	sl := domain.SourcePrincipal("slack", "ws")
	mx := domain.SourcePrincipal("mail", "inbox")
	day := func(d int) domain.SourceMeta {
		return domain.SourceMeta{ReferenceTime: time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC), ReferenceBasis: domain.ReferenceBasisEvent}
	}
	ingest := func(src domain.PrincipalRef, doc, chunk string, d int) {
		t.Helper()
		w := e.Enqueue(domain.WithPrincipal(ctx, src), doc, day(d), []*domain.Document{spChunk(chunk, src)}, true)
		wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := w.Wait(wctx); err != nil || w.Failed() {
			t.Fatalf("%s: wait err=%v failed=%v", doc, err, w.Failed())
		}
	}
	ingest(tg, "a1test-sp-dtg1", "a1test-sp-tg1", 1)
	ingest(sl, "a1test-sp-dsl1", "a1test-sp-sl1", 1)
	ingest(tg, "a1test-sp-dtg2", "a1test-sp-tg2", 2)
	ingest(mx, "a1test-sp-dmx1", "a1test-sp-mx1", 3) // filtered by the cost guard

	if st := e.Stats(); st.SourceFiltered != 1 {
		t.Fatalf("cost guard: %+v, want one filtered ingest (mail)", st)
	}

	// Every stored fact: actor = its source, provenance stamp = its source.
	rows, err := ks.pool.Query(ctx, `SELECT asserted_by, classification FROM knowledge_items WHERE namespace_id = $1 AND kind = $2`, a1NS, domain.KindMemoryFact)
	if err != nil {
		t.Fatal(err)
	}
	actors := map[string]int{}
	for rows.Next() {
		var actor string
		var tags []string
		if err := rows.Scan(&actor, &tags); err != nil {
			t.Fatal(err)
		}
		actors[actor]++
		if !strings.HasPrefix(actor, "source:") || !containsTag(tags, domain.ProvenanceSourceTagPrefix+actor) {
			t.Fatalf("fact by %q carries %v: want a source actor and its own provenance stamp", actor, tags)
		}
	}
	rows.Close()
	if actors["source:telegram:bot1"] != 2 || actors["source:slack:ws"] != 1 || actors["source:mail:inbox"] != 0 {
		t.Fatalf("facts per actor %v, want telegram 2, slack 1, mail 0", actors)
	}

	// Supersession per source: each source has its own current home_city.
	current := func(src domain.PrincipalRef) string {
		t.Helper()
		tags, _ := domain.AllowAllAuthorizer{}.ClassifyWrite(ctx, src, []string{"q1"})
		entity := domain.FactEntityID(domain.FactScopeKey(tags), "user", "home_city")
		item, _, _, _ := currentRow(t, ks, ctx, entity, src.WriterID())
		if item == nil {
			t.Fatalf("%v: no current fact", src)
		}
		it, err := ks.GetItem(ctx, domain.KnowledgeItemID(*item))
		if err != nil || it == nil {
			t.Fatalf("get %s: %v", *item, err)
		}
		return domain.FactFromItem(*it).ValueNorm + "|" + valueOf(it)
	}
	if got := current(tg); !strings.Contains(got, "London") {
		t.Fatalf("telegram current = %q, want London (its own later statement supersedes Paris)", got)
	}
	if got := current(sl); !strings.Contains(got, "Paris") {
		t.Fatalf("slack current = %q, want Paris (telegram's statement must not supersede another source)", got)
	}
}

func containsTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func valueOf(it *domain.KnowledgeItem) string {
	for _, v := range it.Values {
		if v.Predicate == domain.FactPredValue {
			return v.Text
		}
	}
	return ""
}
