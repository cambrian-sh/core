package app

import (
	"context"
	"strings"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/operator/operatortest"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-2184 end to end (owner decision 2026-11-19, option a): through the operator
// plane's real IngestMemory (bound to the composition root's ingestor), ListDocuments
// (the Postgres adapter, as app/boot.go wires k.Documents) and GetIngestStatus (the
// IngestionManager, as k.Server.IngestStatus), over a migrated Postgres:
//
//   - two operators ingest the same source: one source-doc ENTITY id
//     ("source_doc:<uri>", the doc_id both uploads return), two document ROWS (one per
//     writer, "#w=");
//   - GetIngestStatus with the entity id answers with the most recently written row's
//     status (status.document_id names it), and with each row id as before;
//   - ListDocuments with the entity id as its id lookup returns both rows, newest
//     first; an unknown entity resolves to nothing.
//
// The console holds only the entity id (IngestMemory doc_id, MemoryWrittenOp doc_id);
// the kernel resolves it, the console never re-derives a row id.
func TestPg_E2E_SourceDocEntityIDResolvesToItsRows(t *testing.T) {
	dsn, dim := pgscratch.Migrated(t)
	cfg := config.DefaultConfig()
	pgscratch.ApplyToConfig(t, cfg, dsn, dim)
	cfg.Execution.Facts.ExtractEnabled = false
	cfg.Execution.Ingestion.SyncIngestWaitForKG = false
	cfg.Execution.Ingestion.SyncIngestWaitForFacts = false
	ctx := context.Background()

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)
	mem := kernel.NewMemoryStack(kernel.NewMemoryBackend(vec), nil, e2eHashEmbedder{dim: cfg.Embedder.Dimensions},
		cfg.Execution, nil, cfg.Chunker, nil, kernel.FactSubstrate{}, kernel.RetrievalDeps{})
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
	// The operator auth interceptor makes the signed-in operator the upload's writer of
	// record (IngestRequest.Author); this test calls the handler directly, so the
	// wrapper sets it from the principal on ctx and hands the request to the
	// production ingestor unchanged.
	ingestor := operatorMemoryIngestor(mem.IngestionManager)
	asWriter := operator.MemoryIngestorFunc(func(ctx context.Context, req operator.IngestRequest) (string, error) {
		req.Author = domain.PrincipalFromContext(ctx).ID
		return ingestor.Ingest(ctx, req)
	})
	op := operatortest.New(t, nil, func(d *operator.Deps, o *operator.Options) {
		d.Audit, d.Ingestor = audit, asWriter
		o.Documents = vec
		o.IngestStatus = mem.IngestionManager
	})

	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	source := "eng2184-report-" + stamp
	entity := domain.SourceDocEntityPrefix + source
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = vec.Pool().Exec(bg, `DELETE FROM chunks WHERE id = $1 OR document_id IN (SELECT id FROM documents WHERE id LIKE $2 || '%')`, entity, source)
		_, _ = vec.Pool().Exec(bg, `DELETE FROM document_versions WHERE document_id LIKE $1 || '%'`, source)
		_, _ = vec.Pool().Exec(bg, `DELETE FROM documents WHERE id LIKE $1 || '%'`, source)
	})
	ingest := func(writer, text string) *pb.IngestMemoryOpResponse {
		t.Helper()
		resp, err := op.IngestMemory(domain.WithPrincipal(ctx, domain.UserPrincipal(writer)), &pb.IngestMemoryOpRequest{
			CommandId: "eng2184-" + writer + "-" + stamp, Reason: "ENG-2184 e2e", Source: source, Text: text,
		})
		if err != nil {
			t.Fatalf("ingest as %s: %v", writer, err)
		}
		return resp
	}
	first := ingest("eng2184-alice-"+stamp, "Alice's notes on the quarterly freight review of "+stamp+": costs rose nine percent.")
	time.Sleep(20 * time.Millisecond) // the second writer's row is strictly newer
	second := ingest("eng2184-bob-"+stamp, "Bob's notes on the quarterly freight review of "+stamp+": the carrier contract is up for renewal.")

	if first.GetDocId() != entity || second.GetDocId() != entity {
		t.Fatalf("both uploads must return the one entity id %q: %q, %q", entity, first.GetDocId(), second.GetDocId())
	}
	rowA, rowB := first.GetStatus().GetDocumentId(), second.GetStatus().GetDocumentId()
	if rowA == "" || rowB == "" || rowA == rowB || domain.IsSourceDocEntityID(rowA) {
		t.Fatalf("two writers must be two document rows: %q, %q", rowA, rowB)
	}

	// GetIngestStatus by the entity id: the most recently written row's status.
	got, err := op.GetIngestStatus(ctx, &pb.GetIngestStatusOpRequest{DocumentId: entity})
	if err != nil || !got.GetFound() {
		t.Fatalf("GetIngestStatus(entity) = %+v, %v; want found", got, err)
	}
	if got.GetStatus().GetDocumentId() != rowB || got.GetStatus().GetState() != "searchable" {
		t.Fatalf("GetIngestStatus(entity) = %+v, want the newest row %s, searchable", got.GetStatus(), rowB)
	}
	// Row ids answer as before.
	for _, row := range []string{rowA, rowB} {
		if got, err := op.GetIngestStatus(ctx, &pb.GetIngestStatusOpRequest{DocumentId: row}); err != nil || got.GetStatus().GetDocumentId() != row {
			t.Fatalf("GetIngestStatus(%s) = %+v, %v", row, got, err)
		}
	}

	// ListDocuments' id lookup by the entity id: both rows, newest first.
	list, err := op.ListDocuments(ctx, &pb.ListDocumentsOpRequest{IdPrefix: entity})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(list.GetDocuments()); n != 2 || list.GetDocuments()[0].GetId() != rowB || list.GetDocuments()[1].GetId() != rowA {
		t.Fatalf("ListDocuments(entity) = %+v, want [%s %s]", list.GetDocuments(), rowB, rowA)
	}
	if list.GetDocuments()[0].GetCreatedAtUnixMs() <= 0 {
		t.Fatal("the resolved row must carry its ingest time (the console's document view shows it)")
	}

	// An entity nobody ingested resolves to nothing.
	if got, err := op.GetIngestStatus(ctx, &pb.GetIngestStatusOpRequest{DocumentId: entity + "-none"}); err != nil || got.GetFound() {
		t.Fatalf("unknown entity = %+v, %v; want found=false", got, err)
	}
	if list, err := op.ListDocuments(ctx, &pb.ListDocumentsOpRequest{IdPrefix: entity + "-none"}); err != nil || len(list.GetDocuments()) != 0 {
		t.Fatalf("unknown entity listing = %+v, %v", list, err)
	}
}
