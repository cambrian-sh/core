package kernel

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// ceilingDecisionPoint is a decision point with the premium write rule (ADR-0035
// D8′-2: ceiling ∩ hint, the whole ceiling for an empty hint, provenance stamped).
// That rule is what silently dropped a requested tag before ENG-2115; core cannot
// import premium, so the rule is restated here — the property under test is the
// KERNEL's chokepoints refusing what the decision point would drop.
type ceilingDecisionPoint struct {
	domain.AllowAllAuthorizer
	ceilings map[string][]string
}

func (c ceilingDecisionPoint) ClassifyWrite(_ context.Context, p domain.PrincipalRef, hint []string) ([]string, domain.AccessDecision) {
	if p.Kind == domain.PrincipalSystem {
		return hint, domain.AccessDecision{Allowed: true, Reason: domain.ReasonBypass}
	}
	var req []string
	for _, t := range hint {
		if !strings.HasPrefix(t, "provenance:") && !domain.IsOwnerTag(t) {
			req = append(req, t)
		}
	}
	out := []string{}
	for _, t := range c.ceilings[p.WriterID()] {
		if len(req) == 0 || slices.Contains(req, t) {
			out = append(out, t)
		}
	}
	return domain.StampProvenanceSource(out, p.WriterID()), domain.AccessDecision{Allowed: true, Reason: domain.ReasonAllowed}
}

// ENG-2115 end to end (owner decision 2026-11-18, fail closed): through the real
// enforcing path — a real, auto-migrated Postgres (PG_TEST_DSN), NewMemoryStack with
// the decision point, and IngestionManager.ProcessSync exactly as the IngestMemory
// RPC and the operator upload reach it — a write whose requested classification
// cannot be carried in full is refused with an error naming the tags, and NOTHING
// lands: no document row, no chunk. The honourable cases still land with exactly the
// classification asked for, and an untagged write keeps the default (the ceiling).
func TestPg_E2E_UnhonourableClassificationIsRefusedNothingStored(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Facts.ExtractEnabled = false
	cfg.Execution.Ingestion.SyncIngestWaitForKG = false
	cfg.Execution.Ingestion.SyncIngestWaitForFacts = false
	cfg.Execution.Retrieval.HebbianEnabled = false
	ctx := context.Background()

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)
	dp := ceilingDecisionPoint{ceilings: map[string][]string{
		"eng2115-fin": {"confidential", "finance"},
	}}
	mem := NewMemoryStack(NewMemoryBackend(vec), nil, hashEmbedder{dim: cfg.Embedder.Dimensions}, cfg.Execution, dp,
		cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{})

	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	// Every row this test writes carries a unique token in its text (chunks) and title
	// (documents), so the assertions and the cleanup address exactly these rows.
	token := func(name string) string { return "zqeng2115" + strings.ReplaceAll(name, "-", "") + stamp }
	var tokens []string
	t.Cleanup(func() {
		for _, tok := range tokens {
			_, _ = vec.Pool().Exec(context.Background(), `DELETE FROM chunks WHERE text LIKE '%' || $1 || '%'`, tok)
			_, _ = vec.Pool().Exec(context.Background(), `DELETE FROM document_versions WHERE document_id IN (SELECT id FROM documents WHERE title = $1)`, tok)
			_, _ = vec.Pool().Exec(context.Background(), `DELETE FROM documents WHERE title = $1`, tok)
		}
	})
	ingest := func(writer string, tags []string, name string) (string, error) {
		tok := token(name)
		tokens = append(tokens, tok)
		return mem.IngestionManager.ProcessSync(agentGateCtx(writer), domain.ExternalDocument{
			SourceURI: "ingest_memory://eng2115/" + tok, SourceType: "ingest_memory", Title: tok, Body: paragraphs(tok, 1),
			Author: writer, Tags: tags, Timestamp: time.Now().UTC(),
		})
	}
	rows := func(name string) (docs, chunks int) {
		t.Helper()
		tok := token(name)
		if err := vec.Pool().QueryRow(ctx, `SELECT count(*) FROM documents WHERE title = $1`, tok).Scan(&docs); err != nil {
			t.Fatal(err)
		}
		if err := vec.Pool().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE text LIKE '%' || $1 || '%'`, tok).Scan(&chunks); err != nil {
			t.Fatal(err)
		}
		return docs, chunks
	}
	docTags := func(name string) []string {
		t.Helper()
		var tags []string
		if err := vec.Pool().QueryRow(ctx, `SELECT tags FROM documents WHERE title = $1`, token(name)).Scan(&tags); err != nil {
			t.Fatalf("tags of %s: %v", name, err)
		}
		return tags
	}
	refused := func(err error, name string, want ...string) {
		t.Helper()
		if !errors.Is(err, authz.ErrWriteDenied) {
			t.Fatalf("%s: want a refusal wrapping authz.ErrWriteDenied (InvalidArgument at the RPC), got %v", name, err)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: refusal must name %q, got %q", name, w, err.Error())
			}
		}
		if d, c := rows(name); d != 0 || c != 0 {
			t.Fatalf("%s: a refused write stored %d document(s) and %d chunk(s)", name, d, c)
		}
	}

	// The readiness repro: no write ceiling, "confidential, finance" requested.
	_, err = ingest("eng2115-noceiling", []string{"confidential", "finance"}, "noceiling")
	refused(err, "noceiling", "confidential", "finance")

	// A tag outside the ceiling, beside one inside it: refused as a whole.
	_, err = ingest("eng2115-fin", []string{"finance", "people"}, "outside")
	refused(err, "outside", "people")

	// Narrowing within the ceiling: stored with exactly what was asked.
	if _, err := ingest("eng2115-fin", []string{"finance"}, "narrow"); err != nil {
		t.Fatalf("narrow: %v", err)
	}
	if got := docTags("narrow"); !slices.Contains(got, "finance") || slices.Contains(got, "confidential") {
		t.Fatalf("narrow: document tags %v, want finance without confidential", got)
	}
	var chunkTags []string
	if err := vec.Pool().QueryRow(ctx, `SELECT ARRAY(SELECT jsonb_array_elements_text(metadata->'tags')) FROM chunks WHERE text LIKE '%' || $1 || '%' LIMIT 1`,
		token("narrow")).Scan(&chunkTags); err != nil {
		t.Fatalf("narrow chunk tags: %v", err)
	}
	if !slices.Contains(chunkTags, "finance") {
		t.Fatalf("narrow: chunk tags %v must carry the document's finance", chunkTags)
	}

	// No tags requested: today's default — the writer's ceiling; unclassified with none.
	if _, err := ingest("eng2115-fin", nil, "untagged"); err != nil {
		t.Fatalf("untagged: %v", err)
	}
	if got := docTags("untagged"); !slices.Contains(got, "finance") || !slices.Contains(got, "confidential") {
		t.Fatalf("untagged: document tags %v, want the whole ceiling", got)
	}
	if _, err := ingest("eng2115-noceiling", nil, "untagged-noceiling"); err != nil {
		t.Fatalf("untagged without a ceiling stays allowed: %v", err)
	}
	for _, tg := range docTags("untagged-noceiling") {
		if !strings.HasPrefix(tg, "provenance:") && !domain.IsOwnerTag(tg) {
			t.Fatalf("untagged write with no ceiling must carry no classification, got %q", tg)
		}
	}
}
