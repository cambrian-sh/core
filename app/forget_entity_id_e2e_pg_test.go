package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/operator/operatortest"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-2245 end to end (owner decision 2026-11-19, readiness finding of ENG-1995):
// the operator forgets a document by the id IngestMemory RETURNED — the source-doc
// entity id "source_doc:<uri>" — through the real operator plane over a migrated
// Postgres: IngestMemory (the composition root's ingestor), PreviewForgetMemory and
// ForgetMemory (the forget service over MemoryStack.Eraser(), the enforcing eraser
// over the raw PgErasureStore, as app/boot.go wires them).
//
//   - two operators ingest the same source: one entity id, two document rows;
//   - PreviewForgetMemory(document_ids=[entity]) names BOTH rows (via = the entity id);
//     before ENG-2245 it answered "nothing to erase";
//   - ForgetMemory with the preview's token erases both rows, their chunks and
//     versions; the receipt counts them;
//   - a second preview of the same entity finds nothing to erase; an entity nobody
//     ingested finds nothing either.
func TestPg_E2E_OperatorForgetsByTheIngestReturnedEntityID(t *testing.T) {
	dsn, dim := pgscratch.Migrated(t)
	cfg := config.DefaultConfig()
	pgscratch.ApplyToConfig(t, cfg, dsn, dim)
	cfg.Execution.Facts.ExtractEnabled = false
	cfg.Execution.Ingestion.SyncIngestWaitForKG = false
	cfg.Execution.Ingestion.SyncIngestWaitForFacts = false
	cfg.Execution.Erasure.Enabled = true
	ctx := context.Background()

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)
	backend := kernel.NewMemoryBackend(vec)
	backend.Eraser = postgres.NewPgErasureStore(postgres.NewPgDocumentLifecycle(postgres.NewPgKnowledgeStore(vec.Pool(), nil)))
	mem := kernel.NewMemoryStack(backend, nil, e2eHashEmbedder{dim: cfg.Embedder.Dimensions},
		cfg.Execution, nil, cfg.Chunker, nil, kernel.FactSubstrate{}, kernel.RetrievalDeps{})
	if mem.Eraser() == nil {
		t.Fatal("erasure enabled but MemoryStack.Eraser() is nil")
	}
	if mem.ChunkTripletsBatcher != nil {
		mem.ChunkTripletsBatcher.UseExtractor(noTriplets{})
		mem.IngestionManager.SetChunkTripletsBatcher(mem.ChunkTripletsBatcher)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = mem.Start(runCtx) }()
	t.Cleanup(func() { cancel(); <-done; mem.Shutdown(context.Background()) })

	audit, err := postgres.NewPgAuditStore(ctx, vec.Pool())
	if err != nil {
		t.Fatal(err)
	}
	ingestor := operatorMemoryIngestor(mem.IngestionManager)
	asWriter := operator.MemoryIngestorFunc(func(ctx context.Context, req operator.IngestRequest) (string, error) {
		req.Author = domain.PrincipalFromContext(ctx).ID
		return ingestor.Ingest(ctx, req)
	})
	svc := forget.New(mem.Eraser(), forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "eng2245-e2e"})
	op := operatortest.New(t, nil, func(d *operator.Deps, o *operator.Options) {
		d.Audit, d.Ingestor = audit, asWriter
		o.Forget = svc
	})

	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	source := "eng2245-memo-" + stamp
	entity := domain.SourceDocEntityPrefix + source
	ops := "eng2245-ops-" + stamp
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = vec.Pool().Exec(bg, `DELETE FROM chunks WHERE id = $1 OR document_id IN (SELECT id FROM documents WHERE id LIKE $2 || '%')`, entity, source)
		_, _ = vec.Pool().Exec(bg, `DELETE FROM document_versions WHERE document_id LIKE $1 || '%'`, source)
		_, _ = vec.Pool().Exec(bg, `DELETE FROM documents WHERE id LIKE $1 || '%'`, source)
		_, _ = vec.Pool().Exec(bg, `DELETE FROM memory_erasures WHERE principal = $1`, ops)
		_, _ = vec.Pool().Exec(bg, `DELETE FROM operator_audit WHERE command_id LIKE $1`, "eng2245-"+stamp+"%")
	})
	ingest := func(writer, text string) *pb.IngestMemoryOpResponse {
		t.Helper()
		resp, err := op.IngestMemory(domain.WithPrincipal(ctx, domain.UserPrincipal(writer)), &pb.IngestMemoryOpRequest{
			CommandId: "eng2245-" + stamp + "-" + writer, Reason: "ENG-2245 e2e", Source: source, Text: text,
		})
		if err != nil {
			t.Fatalf("ingest as %s: %v", writer, err)
		}
		return resp
	}
	first := ingest("eng2245-alice-"+stamp, "The readiness canary codeword for project Larkspur is ZEBRA-"+stamp+".")
	time.Sleep(20 * time.Millisecond)
	second := ingest("eng2245-bob-"+stamp, "Larkspur ships to the Izmir warehouse every Tuesday, codeword ZEBRA-"+stamp+".")
	if first.GetDocId() != entity || second.GetDocId() != entity {
		t.Fatalf("IngestMemory must return the entity id %q: %q, %q", entity, first.GetDocId(), second.GetDocId())
	}
	rowA, rowB := first.GetStatus().GetDocumentId(), second.GetStatus().GetDocumentId()
	if rowA == "" || rowB == "" || rowA == rowB {
		t.Fatalf("two writers must be two document rows: %q, %q", rowA, rowB)
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := vec.Pool().QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM chunks WHERE document_id IN ($1, $2)`, rowA, rowB); n == 0 {
		t.Fatal("the ingests wrote no chunks")
	}

	opCtx := domain.WithPrincipal(domain.WithPrincipalRoles(ctx, []string{"operator"}),
		domain.PrincipalRef{Kind: domain.PrincipalUser, ID: ops})
	sel := &pb.ErasureSelectorOp{DocumentIds: []string{entity}}
	p, err := op.PreviewForgetMemory(opCtx, &pb.PreviewForgetMemoryOpRequest{Selector: sel})
	if err != nil {
		t.Fatalf("PreviewForgetMemory(entity id): %v", err)
	}
	got := map[string]string{}
	for _, c := range p.GetCandidates() {
		got[c.GetDocumentId()] = c.GetVia()
	}
	if len(got) != 2 || got[rowA] != entity || got[rowB] != entity {
		t.Fatalf("preview must name both rows via the entity id: %+v", p.GetCandidates())
	}

	fr, err := op.ForgetMemory(opCtx, &pb.ForgetMemoryOpRequest{CommandId: "eng2245-" + stamp + "-forget",
		Reason: "ENG-2245 e2e", ReasonCode: "user_request", PreviewToken: p.GetPreviewToken()})
	if err != nil {
		t.Fatalf("ForgetMemory: %v", err)
	}
	if rc := fr.GetReceipt(); rc.GetCounts()["documents"] != 2 || rc.GetCounts()["chunks"] == 0 {
		t.Fatalf("receipt: %+v", rc)
	}
	if n := count(`SELECT count(*) FROM documents WHERE id IN ($1, $2)`, rowA, rowB); n != 0 {
		t.Fatalf("%d document rows survived", n)
	}
	if n := count(`SELECT count(*) FROM chunks WHERE document_id IN ($1, $2) OR text LIKE $3`, rowA, rowB, "%ZEBRA-"+stamp+"%"); n != 0 {
		t.Fatalf("%d chunks survived", n)
	}

	var nothing *domain.NothingToEraseError
	for _, id := range []string{entity, entity + "-none"} {
		_, err := op.PreviewForgetMemory(opCtx, &pb.PreviewForgetMemoryOpRequest{Selector: &pb.ErasureSelectorOp{DocumentIds: []string{id}}})
		if err == nil || (!errors.As(err, &nothing) && !strings.Contains(err.Error(), "nothing to erase")) {
			t.Fatalf("preview of %s after the erase: want nothing to erase, got %v", id, err)
		}
	}
}
