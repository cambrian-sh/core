package kernel

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/evidence"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory/erasureworker"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/storage"
)

// journalDeadLetterStep is the ADR-0137 D7 dead-letter step as the premium reactive
// plugin contributes it (cambrian-premium reactive.DeadLetterErasureStep): delete the
// journal's dead letters whose signal names an erased record
// (domain.ReactiveDeadLetter.ReferencesAny). Core cannot import premium; the match is
// the shared domain method, so this is the same rule over the REAL kernel journal.
type journalDeadLetterStep struct{ j *AgentRepoDecorator }

func (s journalDeadLetterStep) Step() string { return "reactive_dead_letters" }
func (s journalDeadLetterStep) RunErasureStep(_ context.Context, e domain.ErasedRecords) (int, error) {
	dls, err := s.j.ListDeadLetters(0)
	if err != nil {
		return 0, err
	}
	ids, n := e.IDs(), 0
	for _, dl := range dls {
		if dl.ReferencesAny(ids) {
			if found, err := s.j.DeleteDeadLetter(dl.ID); err != nil {
				return n, err
			} else if found {
				n++
			}
		}
	}
	return n, nil
}

// TestPg_Forget_DeadLettersOfTheErasedDocumentGone (ENG-1966, ADR-0137 D7 / R4 row 26)
// through the real path: an agent's forget through the enforcing eraser commits the
// erase, which queues the registered tail step once (the receipt counts it); the
// erasure worker then runs it with the request's content-free erased records over the
// real bbolt reactive journal. Dead letters whose signal names the erased document or
// its evidence bytes' content address are deleted; an unrelated dead letter stays; the
// step is marked done and the request completes. A dead letter naming only one of the
// document's CHUNK ids stays too: a document erase tombstones the document and its
// evidence, not each chunk, so chunk ids are not among the erased records (recorded on
// ENG-1966 as a limit of the exact-reference match).
func TestPg_Forget_DeadLettersOfTheErasedDocumentGone(t *testing.T) {
	g := newGateWith(t, postgres.WithErasureTailSteps("reactive_dead_letters"))
	cas, err := storage.NewBBoltContentStore(filepath.Join(t.TempDir(), "content.db"), filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cas.Close() })
	ing, err := evidence.NewIngestor(cas, postgres.NewPgEvidenceStore(g.vec.Pool()))
	if err != nil {
		t.Fatal(err)
	}
	g.mem.IngestionManager.SetEvidenceCapture(ing)
	journal, done := openDeliveryJournal(t, filepath.Join(t.TempDir(), "agents.db"))
	defer done()

	alice := agentGateCtx("eng411d-alice")
	doc, chunks := g.ingest(alice, "eng411d-alice", "deadletter", paragraphs(g.tok("dl"), 2))
	var cid string
	if err := g.vec.Pool().QueryRow(g.ctx, `SELECT content_hash FROM evidence WHERE source_key = $1`, doc).Scan(&cid); err != nil {
		t.Fatalf("positive control: the document's evidence: %v", err)
	}
	now := time.Now().UTC()
	for _, dl := range []domain.ReactiveDeadLetter{
		{ID: "dl-doc", WatchID: "w", Reason: "sink down", FailedAt: now,
			Signal: domain.Signal{StreamID: "s", RawText: "erased words", Payload: map[string]any{"document_id": doc}}},
		{ID: "dl-chunk", WatchID: "w", Reason: "sink down", FailedAt: now,
			Signal: domain.Signal{StreamID: "s", Payload: map[string]any{"memory": map[string]any{"chunk": chunks[0]}}}},
		{ID: "dl-cid", WatchID: "w", Reason: "sink down", FailedAt: now,
			Signal: domain.Signal{StreamID: "ingress.raw.x", Payload: map[string]any{"cid": cid}}},
		{ID: "dl-other", WatchID: "w", Reason: "sink down", FailedAt: now,
			Signal: domain.Signal{StreamID: "s", RawText: "unrelated", Payload: map[string]any{"document_id": "some-other-doc"}}},
	} {
		if err := journal.RecordDeadLetter(dl); err != nil {
			t.Fatal(err)
		}
	}

	rec, err := g.svc.Forget(alice, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{doc}}})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if rec.Status != domain.ErasureStatusPendingAsync || rec.PendingSteps < 2 {
		t.Fatalf("receipt = %+v: want pending_async counting the CAS step(s) and the tail step", rec)
	}
	if n := g.n(`SELECT count(*) FROM memory_erasure_outbox WHERE request_id = $1 AND step = 'reactive_dead_letters' AND ref = $1`, rec.RequestID); n != 1 {
		t.Fatalf("the tail step was queued %d times, want once", n)
	}

	w := erasureworker.New(g.eraser, cas, erasureworker.Config{
		Poll: time.Hour, TailSteps: []domain.ErasureTailStep{journalDeadLetterStep{j: journal}}}, nil)
	st := w.RunOnce(context.Background())
	if st.Failed != 0 {
		t.Fatalf("worker pass = %+v", st)
	}
	left, err := journal.ListDeadLetters(0)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, d := range left {
		kept[d.ID] = true
	}
	if len(left) != 2 || !kept["dl-other"] || !kept["dl-chunk"] {
		ids := []string{}
		for _, d := range left {
			ids = append(ids, d.ID)
		}
		t.Fatalf("dead letters left = %v, want dl-other and dl-chunk", ids)
	}
	if n := g.n(`SELECT count(*) FROM memory_erasure_outbox WHERE request_id = $1 AND processed_at IS NULL`, rec.RequestID); n != 0 {
		t.Fatalf("%d outbox steps still pending", n)
	}
	if n := g.n(`SELECT count(*) FROM memory_erasures WHERE request_id = $1 AND status = 'complete'`, rec.RequestID); n != 1 {
		t.Fatal("the request did not complete after its steps ran")
	}
}
