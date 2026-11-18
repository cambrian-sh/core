package kernel

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/evidence"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/storage"
)

// countingBlobs counts content-store puts, so the gate can assert a refused replay
// wrote no bytes back into the store an erase deleted them from.
type countingBlobs struct {
	domain.ContentStore
	puts atomic.Int64
}

func (c *countingBlobs) Put(ctx context.Context, data []byte, nodeType string, labels []string, snippet string) (domain.CID, error) {
	c.puts.Add(1)
	return c.ContentStore.Put(ctx, data, nodeType, labels, snippet)
}

// TestPg_Forget_NoResurrection_ReactiveReplay is the reactive-replay arm of the
// ADR-0137 no-resurrection gate (ENG-1866). The premium reactive engine re-drives a
// journaled signal with the signal's ORIGINAL time stamped as the ingest origin
// (reactive workItem.origin → domain.WithIngestOrigin; the pipeline scheduler does
// the same with provenance ingested_at). This arm drives the two kernel doors such
// a replay reaches — a memory ingest (IngestionManager.ProcessSync, evidence capture
// ON) and the evidence lane the reactive "ingest" action writes through
// (KernelServices.EvidenceIngest = evidence.Ingestor.Ingest) — with that origin,
// after an erase of the document: nothing reappears in any table, no evidence row
// is re-created and no bytes are re-written to the content store. A delivery
// accepted after the erase is a new act (Q4).
func TestPg_Forget_NoResurrection_ReactiveReplay(t *testing.T) {
	g := newGate(t)
	raw, err := storage.NewBBoltContentStore(filepath.Join(t.TempDir(), "content.db"), filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	blobs := &countingBlobs{ContentStore: raw}
	ing, err := evidence.NewIngestor(blobs, postgres.NewPgEvidenceStore(g.vec.Pool()))
	if err != nil {
		t.Fatal(err)
	}
	g.mem.IngestionManager.SetEvidenceCapture(ing)

	alice := agentGateCtx("eng411d-alice")
	secret, entity := g.tok("replay"), g.tok("replayent")
	body := paragraphs(secret, 2)
	signalAt := time.Now().UTC() // the journaled signal's receive time
	doc, chunks := g.ingest(domain.WithIngestOrigin(alice, signalAt), "eng411d-alice", "replay", body)
	stub := g.stub(doc)
	if n := g.n(`SELECT count(*) FROM evidence WHERE source_key = $1`, doc); n != 1 {
		t.Fatalf("positive control: the ingest captured %d evidence rows for the document, want 1", n)
	}

	if _, err := g.svc.Forget(alice, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{doc}}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	replay := domain.WithIngestOrigin(alice, signalAt)
	putsBefore := blobs.puts.Load()
	// 1. The replayed signal's memory ingest.
	_, err = g.mem.IngestionManager.ProcessSync(replay, domain.ExternalDocument{
		SourceURI: g.uri("replay"), SourceType: "file_drop", Title: "replay", Body: body, Author: "eng411d-alice", Timestamp: signalAt})
	if !errors.Is(err, domain.ErrSourceErased) {
		t.Fatalf("replayed ingest after the erase: want ErrSourceErased, got %v", err)
	}
	// 2. The replayed signal's evidence write (the reactive ingest action's lane).
	_, _, err = ing.Ingest(replay, domain.RawEvidence{SourceID: "watch:eng411d", SourceKey: doc, SourceRevision: "replayed", Bytes: []byte(body)})
	if !errors.Is(err, domain.ErrSourceErased) {
		t.Fatalf("replayed evidence after the erase: want ErrSourceErased, got %v", err)
	}
	if n := blobs.puts.Load() - putsBefore; n != 0 {
		t.Errorf("the refused replays wrote %d blobs back into the content store", n)
	}
	for k, v := range g.derivatives(doc, chunks, secret) {
		if v != 0 {
			t.Errorf("resurrected by the replay: %s = %d", k, v)
		}
	}
	if got := g.leaks(alice, doc, stub, chunks, secret, entity); len(got) != 0 {
		t.Errorf("a lane surfaces replayed content: %v", got)
	}

	// The same delivery accepted after the erase is a new act (ADR-0137 Q4).
	fresh := domain.WithIngestOrigin(alice, time.Now().UTC())
	if _, inserted, err := ing.Ingest(fresh, domain.RawEvidence{SourceID: "watch:eng411d", SourceKey: doc, SourceRevision: "new-act", Bytes: []byte(body)}); err != nil || !inserted {
		t.Fatalf("a new delivery after the erase: inserted=%v err=%v", inserted, err)
	}
}
