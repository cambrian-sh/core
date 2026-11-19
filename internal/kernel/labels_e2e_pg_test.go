package kernel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory/forget"
)

// ENG-2170 end to end (owner decision 2026-11-19: a SEPARATE LABEL FIELD), through
// the real enforcing path under a WRITE CEILING: a real, auto-migrated Postgres
// (PG_TEST_DSN), NewMemoryStack with a ceiling decision point (the premium write rule,
// ceilingDecisionPoint) and erasure on, IngestionManager.ProcessSync exactly as both
// ingest RPCs reach it, the operator recall pass with the shipped superseded
// demotion, and the forget service over MemoryStack.Eraser().
//
//   - the defect: a revision whose lifecycle rides in the TAGS is refused under the
//     ceiling (ENG-2115), nothing stored;
//   - the fix: the same revision with its lifecycle in LABELS lands; its tags are the
//     ceiling-narrowed classification alone, and every chunk and the document row
//     carry the labels (metadata "labels");
//   - the ENG-2158 demotion reads the label: the in-force copy of a shared chunk
//     outranks the superseded one;
//   - erasing the superseded revision removes its labels with its rows.
func TestPg_E2E_LabelsCarryTheLifecycleUnderAWriteCeiling(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Facts.ExtractEnabled = false
	cfg.Execution.Ingestion.SyncIngestWaitForKG = false
	cfg.Execution.Ingestion.SyncIngestWaitForFacts = false
	cfg.Execution.Retrieval.HebbianEnabled = false
	cfg.Execution.Erasure.Enabled = true
	cfg.Execution.Erasure.AgentEraseEnabled = true
	ctx := context.Background()

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)
	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	writer := "eng2170-apply-" + stamp
	dp := ceilingDecisionPoint{ceilings: map[string][]string{writer: {"scope-docs", "scope-hr"}}}
	backend := NewMemoryBackend(vec)
	backend.Eraser = postgres.NewPgErasureStore(postgres.NewPgDocumentLifecycle(postgres.NewPgKnowledgeStore(vec.Pool(), nil)))
	mem := NewMemoryStack(backend, nil, hashEmbedder{dim: cfg.Embedder.Dimensions}, cfg.Execution, dp,
		cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{})
	if mem.Eraser() == nil {
		t.Fatal("erasure enabled but MemoryStack.Eraser() is nil")
	}

	token := "zqeng2170" + stamp
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = vec.Pool().Exec(bg, `DELETE FROM chunks WHERE text LIKE '%' || $1 || '%'`, token)
		_, _ = vec.Pool().Exec(bg, `DELETE FROM document_versions WHERE document_id IN (SELECT id FROM documents WHERE title LIKE $1 || '%')`, token)
		_, _ = vec.Pool().Exec(bg, `DELETE FROM documents WHERE title LIKE $1 || '%'`, token)
		_, _ = vec.Pool().Exec(bg, `DELETE FROM memory_erasures WHERE principal = $1`, writer)
	})
	// One shared paragraph (the copy every revision keeps) plus the revision's own.
	shared := "The " + token + " receiving dock is scheduled by the Site Manager every weekday morning."
	body := func(rev int) string {
		return fmt.Sprintf("%s\n\nRevision %d of the %s receiving steps adds check %d.", shared, rev, token, rev)
	}
	ingest := func(rev int, tags, labels []string) (string, error) {
		title := fmt.Sprintf("%s-rev%d", token, rev)
		return mem.IngestionManager.ProcessSync(agentGateCtx(writer), domain.ExternalDocument{
			SourceURI: "operator_ingest://" + title + ".md", SourceType: "operator_ingest", Title: title,
			Body: body(rev), Author: writer, Tags: tags, Labels: labels, Timestamp: time.Now().UTC(),
		})
	}
	lifecycle := func(rev int) []string {
		state := "superseded"
		if rev == 2 {
			state = "in-force"
		}
		return []string{"vantis-medical", "sop-001", fmt.Sprintf("rev%d", rev), state}
	}

	// The defect, reproduced: the importer's identity markers as TAGS under the
	// ceiling are a requested classification the ceiling cannot carry — refused.
	_, err = ingest(9, append([]string{"scope-docs"}, lifecycle(1)...), nil)
	if !errors.Is(err, authz.ErrWriteDenied) || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("lifecycle as tags under a ceiling: err = %v; want the ENG-2115 refusal naming superseded", err)
	}
	var stored int
	if err := vec.Pool().QueryRow(ctx, `SELECT count(*) FROM documents WHERE title = $1`, token+"-rev9").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("a refused write stored %d document(s) (%v)", stored, err)
	}

	// The fix: the lifecycle as LABELS lands, whatever the ceiling.
	for rev := 1; rev <= 2; rev++ {
		if _, err := ingest(rev, []string{"scope-docs"}, lifecycle(rev)); err != nil {
			t.Fatalf("rev%d with labels: %v", rev, err)
		}
	}
	type row struct {
		tags, labels []string
	}
	chunkRows := func(rev int) []row {
		t.Helper()
		rows, err := vec.Pool().Query(ctx, `SELECT
				COALESCE(ARRAY(SELECT jsonb_array_elements_text(metadata->'tags')), '{}'),
				COALESCE(ARRAY(SELECT jsonb_array_elements_text(metadata->'labels')), '{}')
			FROM chunks WHERE metadata->>'document_id' = (SELECT id FROM documents WHERE title = $1)`, fmt.Sprintf("%s-rev%d", token, rev))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.tags, &r.labels); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	for rev := 1; rev <= 2; rev++ {
		rows := chunkRows(rev)
		if len(rows) == 0 {
			t.Fatalf("rev%d: no chunks stored", rev)
		}
		for _, r := range rows {
			if !slices.Equal(r.labels, lifecycle(rev)) {
				t.Errorf("rev%d chunk labels = %v, want %v", rev, r.labels, lifecycle(rev))
			}
			if !slices.Contains(r.tags, "scope-docs") || slices.Contains(r.tags, "scope-hr") {
				t.Errorf("rev%d chunk tags = %v, want the narrowed classification scope-docs", rev, r.tags)
			}
			for _, l := range lifecycle(rev) {
				if slices.Contains(r.tags, l) {
					t.Errorf("rev%d chunk tags %v carry the label %q", rev, r.tags, l)
				}
			}
		}
		var docTags, docLabels []string
		if err := vec.Pool().QueryRow(ctx, `SELECT tags, COALESCE(ARRAY(SELECT jsonb_array_elements_text(metadata->'labels')), '{}')
			FROM documents WHERE title = $1`, fmt.Sprintf("%s-rev%d", token, rev)).Scan(&docTags, &docLabels); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(docLabels, lifecycle(rev)) || slices.Contains(docTags, "superseded") || slices.Contains(docTags, "in-force") {
			t.Errorf("rev%d document row: tags %v labels %v", rev, docTags, docLabels)
		}
	}

	// ENG-2158 reads the label: with the shipped markers the in-force copy of the
	// shared paragraph ranks above the superseded copy.
	mem.QueryService.EnableSupersededDemotion(cfg.Execution.Retrieval.SupersededMarkers, cfg.Execution.Retrieval.SupersededRankFactor)
	res, err := mem.QueryService.SearchSystem(ctx, "Who schedules the "+token+" receiving dock?")
	if err != nil {
		t.Fatal(err)
	}
	rank := map[string]int{}
	for i, r := range res {
		if !strings.Contains(r.Document.Text, token) {
			continue
		}
		for _, l := range domain.LabelsOf(r.Document.Metadata) {
			if (l == "rev1" || l == "rev2") && rank[l] == 0 {
				rank[l] = i + 1
			}
		}
	}
	if rank["rev1"] == 0 || rank["rev2"] == 0 || rank["rev2"] > rank["rev1"] {
		t.Fatalf("ranks rev1=%d rev2=%d (0 = absent): the in-force revision must outrank the superseded one, both present", rank["rev1"], rank["rev2"])
	}

	// Erasure removes the labels with the rows: forget the superseded revision.
	var rev1Doc string
	if err := vec.Pool().QueryRow(ctx, `SELECT id FROM documents WHERE title = $1`, token+"-rev1").Scan(&rev1Doc); err != nil {
		t.Fatal(err)
	}
	svc := forget.New(mem.Eraser(), forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "eng2170"})
	p, err := svc.Preview(agentGateCtx(writer), domain.ErasureSelector{DocumentIDs: []string{rev1Doc}})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if _, err := svc.Forget(agentGateCtx(writer), forget.Request{PreviewToken: p.Token}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	var left int
	if err := vec.Pool().QueryRow(ctx, `SELECT
			(SELECT count(*) FROM chunks WHERE text LIKE '%' || $1 || '%' AND metadata->'labels' ? 'rev1') +
			(SELECT count(*) FROM documents WHERE metadata->'labels' ? 'rev1' AND title LIKE $1 || '%')`, token).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d row(s) still carry the erased revision's labels", left)
	}
	if rows := chunkRows(2); len(rows) == 0 {
		t.Fatal("erasing rev1 took the in-force revision with it")
	}
}
