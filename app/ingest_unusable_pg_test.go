package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/evidence"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/operator/operatortest"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-2110 end to end (owner decision 2026-11-18: unusable content is not a success).
//
// Real path: a migrated Postgres (PG_TEST_DSN), the memory stack NewMemoryStack builds
// (derivation journal on by default, the stack's triplet batcher wired as boot does),
// ADR-0105 evidence capture over the Postgres evidence store, the operator plane's
// IngestMemory RPC bound through operatorMemoryIngestor (the composition root's
// binding), and the /v1/ingest receiver behind the listener's limitBody cap feeding
// the IngestionManager's batch queue. Only the docling sidecar and the model seats are
// fakes; the sidecar fails or degrades on the readiness block's bad files exactly as
// the live one did.
//
// Every bad input is refused with a reason naming it, and leaves 0 documents, 0
// chunks, 0 evidence rows, 0 outbox rows and 0 derivation-journal rows. A good document
// in the same run, and a NUL-carrying text, still ingest.
func TestPg_IngestRefusesUnusableContent(t *testing.T) {
	dsn, dim := pgscratch.Migrated(t)
	cfg := config.DefaultConfig()
	pgscratch.ApplyToConfig(t, cfg, dsn, dim)
	cfg.Execution.Ingestion.SyncIngestWaitForKG = false
	cfg.Execution.Ingestion.SyncIngestWaitForFacts = false
	if !cfg.Execution.Ingestion.DerivationJournalEnabled {
		t.Fatal("derivation journal must be on by default")
	}
	ctx := context.Background()

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)
	mem := kernel.NewMemoryStack(kernel.NewMemoryBackend(vec), nil, e2eHashEmbedder{dim: cfg.Embedder.Dimensions},
		cfg.Execution, nil, cfg.Chunker, nil, kernel.FactSubstrate{}, kernel.RetrievalDeps{})
	if mem.ChunkTripletsBatcher == nil || mem.Derivations == nil {
		t.Fatal("the composed stack must carry the triplet batcher and the derivation journal")
	}
	mem.ChunkTripletsBatcher.UseExtractor(noTriplets{})
	mem.IngestionManager.SetChunkTripletsBatcher(mem.ChunkTripletsBatcher)
	mem.IngestionManager.SetStructureGraph(readinessSidecar{}, vec.StructureGraphStore())
	blobs, err := storage.NewBBoltContentStore(filepath.Join(t.TempDir(), "content.db"), filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	ev, err := evidence.NewIngestor(blobs, postgres.NewPgEvidenceStore(vec.Pool()))
	if err != nil {
		t.Fatal(err)
	}
	mem.IngestionManager.SetEvidenceCapture(ev)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = mem.Start(runCtx) }()
	t.Cleanup(func() { cancel(); <-done; mem.Shutdown(context.Background()) })

	audit, err := postgres.NewPgAuditStore(ctx, vec.Pool())
	if err != nil {
		t.Fatal(err)
	}
	op := operatortest.New(t, nil, func(d *operator.Deps, _ *operator.Options) {
		d.Audit, d.Ingestor = audit, operatorMemoryIngestor(mem.IngestionManager)
	})
	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	opCtx := domain.WithPrincipal(ctx, domain.UserPrincipal("eng2110-op-"+stamp))

	count := func(table string) int {
		t.Helper()
		var n int
		if err := vec.Pool().QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	tables := []string{"documents", "chunks", "evidence", "evidence_outbox", "derivation_jobs"}
	snapshot := func() map[string]int {
		m := map[string]int{}
		for _, tb := range tables {
			m[tb] = count(tb)
		}
		return m
	}
	assertNothingWritten := func(what string, before map[string]int) {
		t.Helper()
		for _, tb := range tables {
			if got := count(tb); got != before[tb] {
				t.Errorf("%s: %s grew %d -> %d; a refused ingest writes nothing", what, tb, before[tb], got)
			}
		}
	}

	// ---- the operator IngestMemory RPC ----
	pdfLeaves := []byte("%PDF-1.7 readable " + stamp)
	bad := []struct {
		name string
		req  *pb.IngestMemoryOpRequest
		why  string
	}{
		{"random bytes .bin", &pb.IngestMemoryOpRequest{Content: []byte{0x8f, 0x01, 0xfe, 0x00, 0x13}, Filename: "blob-" + stamp + ".bin"}, "document parser failed"},
		{"random bytes named .pdf", &pb.IngestMemoryOpRequest{Content: []byte("\x13\x99random" + stamp), Filename: "fake-" + stamp + ".pdf"}, "degraded parse"},
		{"truncated pdf", &pb.IngestMemoryOpRequest{Content: []byte("%PDF-1.7\n1 0 obj " + stamp), Filename: "cut-" + stamp + ".pdf"}, "degraded parse"},
		{"1x1 png", &pb.IngestMemoryOpRequest{Content: []byte("\x89PNG\r\n\x1a\n" + stamp), Filename: "dot-" + stamp + ".png"}, "found no text"},
		{"extension-less bytes", &pb.IngestMemoryOpRequest{Content: []byte("\x01\x02\x03" + stamp), Filename: "noext" + stamp}, "document parser"},
		{"whitespace-only text", &pb.IngestMemoryOpRequest{Text: "   \n\t  ", Title: "ws-" + stamp}, "whitespace only"},
		{"NUL-only text", &pb.IngestMemoryOpRequest{Text: "\x00 \x00", Title: "nul-" + stamp}, "whitespace only"},
	}
	for i, tc := range bad {
		before := snapshot()
		tc.req.CommandId, tc.req.Reason = fmt.Sprintf("eng2110-%s-%d", stamp, i), "ENG-2110 e2e"
		_, err := op.IngestMemory(opCtx, tc.req)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: want InvalidArgument, got %v", tc.name, err)
			continue
		}
		name := tc.req.GetFilename()
		if name == "" {
			name = tc.req.GetTitle()
		}
		if msg := status.Convert(err).Message(); !strings.Contains(msg, "no usable text could be extracted from "+name+": ") ||
			!strings.Contains(msg, tc.why) {
			t.Errorf("%s: reason %q, want the name %q and %q", tc.name, msg, name, tc.why)
		}
		assertNothingWritten(tc.name, before)
	}

	// A good PDF and a text with a NUL in it, in the same run, ingest.
	resp, err := op.IngestMemory(opCtx, &pb.IngestMemoryOpRequest{CommandId: "eng2110-good-" + stamp, Reason: "ENG-2110 e2e",
		Content: pdfLeaves, Filename: "report-" + stamp + ".pdf"})
	if err != nil {
		t.Fatalf("good pdf: %v", err)
	}
	var docID string
	if err := vec.Pool().QueryRow(ctx, `SELECT document_id FROM chunks WHERE id = $1`, resp.GetDocId()).Scan(&docID); err != nil {
		t.Fatalf("document of the good pdf (%s): %v", resp.GetDocId(), err)
	}
	var chunks, evRows int
	_ = vec.Pool().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE document_id = $1 AND text LIKE '%' || $2 || '%'`, docID, stamp).Scan(&chunks)
	_ = vec.Pool().QueryRow(ctx, `SELECT count(*) FROM evidence WHERE source_key = $1`, docID).Scan(&evRows)
	if chunks == 0 || evRows != 1 {
		t.Errorf("good pdf: %d content chunks, %d evidence rows; want >0 and 1", chunks, evRows)
	}
	nulResp, err := op.IngestMemory(opCtx, &pb.IngestMemoryOpRequest{CommandId: "eng2110-nul-" + stamp, Reason: "ENG-2110 e2e",
		Text: "the budget review\x00 moved to Thursday " + stamp, Title: "nul-text-" + stamp})
	if err != nil {
		t.Fatalf("text with a NUL: want it ingested with the NUL normalised, got %v", err)
	}
	var nulText string
	if err := vec.Pool().QueryRow(ctx, `SELECT d.text FROM documents d JOIN chunks c ON c.document_id = d.id WHERE c.id = $1`,
		nulResp.GetDocId()).Scan(&nulText); err != nil {
		t.Fatalf("document of the NUL text: %v", err)
	}
	if strings.ContainsRune(nulText, 0) || !strings.Contains(nulText, "the budget review  moved") {
		t.Errorf("NUL text stored as %q", nulText)
	}

	// ---- the /v1/ingest HTTP push (batch lane) ----
	push := limitBody(memory.NewWebhookReceiver("tok-"+stamp, mem.IngestionManager.Enqueue), 0)
	post := func(body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/ingest", body)
		req.Header.Set("X-Ingest-Token", "tok-"+stamp)
		rec := httptest.NewRecorder()
		push.ServeHTTP(rec, req)
		return rec
	}
	before := snapshot()
	// The readiness push: a 60 MB body whose only field ExternalDocument does not have.
	big := io.MultiReader(strings.NewReader(`{"payload":"`), io.LimitReader(repeatByte('a'), 60<<20), strings.NewReader(`"}`))
	if rec := post(big); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `unknown field "payload"`) {
		t.Errorf("60 MB unknown-field push: %d %q, want 400 naming the field", rec.Code, rec.Body.String())
	}
	// Over the listener's cap (64 MiB): 413.
	huge := io.MultiReader(strings.NewReader(`{"Body":"`), io.LimitReader(repeatByte('a'), 65<<20), strings.NewReader(`"}`))
	if rec := post(huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("65 MiB push: %d, want 413", rec.Code)
	}
	for _, body := range []string{`{"Title":"empty-` + stamp + `"}`, `{"Body":"  \n\t ","Title":"ws-push-` + stamp + `"}`} {
		if rec := post(strings.NewReader(body)); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no usable text could be extracted") {
			t.Errorf("push %s: %d %q, want 400 with the reason", body, rec.Code, rec.Body.String())
		}
	}
	// A push of unreadable bytes is accepted for the queue (202 "queued") and then
	// dropped by the batch lane with nothing written; a good push beside it lands.
	junk := base64.StdEncoding.EncodeToString([]byte("\x13\x99junk" + stamp))
	if rec := post(strings.NewReader(`{"SourceURI":"https://push/junk-` + stamp + `.pdf","SourceType":"pdf","Data":"` + junk + `"}`)); rec.Code != http.StatusAccepted {
		t.Fatalf("bytes push: %d %q", rec.Code, rec.Body.String())
	}
	if rec := post(strings.NewReader(`{"SourceURI":"https://push/ok-` + stamp + `","SourceType":"web","Title":"ok","Body":"the release shipped ` + stamp + `"}`)); rec.Code != http.StatusAccepted {
		t.Fatalf("good push: %d %q", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		_ = vec.Pool().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE text LIKE '%the release shipped ' || $1 || '%'`, stamp).Scan(&n)
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the good push was never stored")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The batch lane takes documents in order, so the junk push is settled by now:
	// exactly the good push's document was added.
	if got := count("documents") - before["documents"]; got != 1 {
		t.Errorf("push lane added %d documents, want 1 (the good push only)", got)
	}
	var junkChunks int
	_ = vec.Pool().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE metadata::text LIKE '%junk-' || $1 || '%'`, stamp).Scan(&junkChunks)
	if junkChunks != 0 {
		t.Errorf("the unreadable push stored %d chunks", junkChunks)
	}
	if got := count("evidence") - before["evidence"]; got != 0 {
		t.Errorf("push lane added %d evidence rows; the batch lane captures none", got)
	}
}

// readinessSidecar answers like the docling sidecar did in the readiness block: a
// readable PDF yields leaves; random bytes fail; a non-PDF named .pdf and a truncated
// PDF degrade; an image has no text. Text documents take the flat path.
type readinessSidecar struct{}

func (readinessSidecar) Parse(_ context.Context, req memory.StructureParseRequest) (*memory.StructuredDocument, error) {
	if req.DataB64 == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(req.DataB64)
	if err != nil {
		return nil, err
	}
	doc := &memory.StructuredDocument{DocID: req.DocID, Backend: "docling", OK: true}
	switch {
	case bytes.HasPrefix(raw, []byte("%PDF-1.7 readable")):
		doc.Nodes = []memory.StructNode{{ID: "p1", Kind: "paragraph", Text: "Quarterly report. " + string(raw)}}
	case bytes.HasPrefix(raw, []byte("%PDF")):
		doc.OK = false
		doc.Nodes = []memory.StructNode{{ID: "p1", Kind: "paragraph", Text: "  "}}
	case bytes.HasPrefix(raw, []byte("\x89PNG")):
		// an image with no recognised text: an OK parse, no leaves
	case strings.HasSuffix(req.SourceType, "pdf"):
		doc.OK = false
	default:
		return nil, errors.New("docling: unsupported input format")
	}
	return doc, nil
}

type noTriplets struct{}

func (noTriplets) ExtractBatch(_ context.Context, texts, _ []string) ([][]domain.ChunkTriplet, error) {
	return make([][]domain.ChunkTriplet, len(texts)), nil
}

// e2eHashEmbedder is a deterministic bag-of-words embedder.
type e2eHashEmbedder struct{ dim int }

func (h e2eHashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, h.dim)
	for _, tok := range strings.Fields(strings.ToLower(text)) {
		f := fnv.New32a()
		_, _ = f.Write([]byte(tok))
		v[int(f.Sum32()%uint32(h.dim))]++
	}
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		v[0] = 1
		return v, nil
	}
	s := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= s
	}
	return v, nil
}

// repeatByte is an endless reader of one byte, so a large body costs no memory to send.
type repeatByte byte

func (b repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}
