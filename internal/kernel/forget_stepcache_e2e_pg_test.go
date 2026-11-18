package kernel

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory/erasureworker"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/storage"
)

// TestPg_Forget_StepCacheEvicted (ENG-1967, ADR-0137 D7 / R4 row 28) through the real
// path: an agent's forget commits the erase, which queues the step_cache step once
// (the receipt counts it); the erasure worker runs core's StepCacheErasureStep over
// a real bbolt step cache holding hand-offs that quote the erased text, every entry
// is evicted (no entry records its session or source records), the step is marked
// done and the request completes. A hand-off cached after the erase is served again.
func TestPg_Forget_StepCacheEvicted(t *testing.T) {
	g := newGateWith(t, postgres.WithErasureTailSteps(storage.StepCacheErasureStepName))
	db, err := bbolt.Open(filepath.Join(t.TempDir(), "agents.db"), 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sc, err := storage.NewBBoltStepCache(db)
	if err != nil {
		t.Fatal(err)
	}
	cas, err := storage.NewBBoltContentStore(filepath.Join(t.TempDir(), "content.db"), filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cas.Close() })

	alice := agentGateCtx("eng1967-alice")
	secret := g.tok("sc")
	doc, _ := g.ingest(alice, "eng1967-alice", "stepcache", paragraphs(secret, 2))
	for _, k := range []string{"step-a", "step-b"} {
		if err := sc.Put(g.ctx, k, &domain.Handoff{ID: k, Payload: &domain.Payload{Data: []byte("the answer quotes " + secret)}}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	rec, err := g.svc.Forget(alice, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{doc}}})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if rec.Status != domain.ErasureStatusPendingAsync || rec.PendingSteps < 1 {
		t.Fatalf("receipt = %+v: want pending_async counting the step_cache step", rec)
	}
	if n := g.n(`SELECT count(*) FROM memory_erasure_outbox WHERE request_id = $1 AND step = 'step_cache' AND ref = $1`, rec.RequestID); n != 1 {
		t.Fatalf("the step_cache step was queued %d times, want once", n)
	}

	w := erasureworker.New(g.eraser, cas, erasureworker.Config{
		Poll: time.Hour, TailSteps: []domain.ErasureTailStep{storage.StepCacheErasureStep{Cache: sc}}}, nil)
	if st := w.RunOnce(context.Background()); st.Failed != 0 {
		t.Fatalf("worker pass = %+v", st)
	}
	for _, k := range []string{"step-a", "step-b"} {
		if _, hit, err := sc.Get(g.ctx, k); hit || err != nil {
			t.Fatalf("cached hand-off %s survived the erasure (hit=%v err=%v)", k, hit, err)
		}
	}
	if n := g.n(`SELECT count(*) FROM memory_erasure_outbox WHERE request_id = $1 AND processed_at IS NULL`, rec.RequestID); n != 0 {
		t.Fatalf("%d outbox steps still pending", n)
	}
	if n := g.n(`SELECT count(*) FROM memory_erasures WHERE request_id = $1 AND status = 'complete'`, rec.RequestID); n != 1 {
		t.Fatal("the request did not complete after its steps ran")
	}
	if err := sc.Put(g.ctx, "step-c", &domain.Handoff{ID: "step-c"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := sc.Get(g.ctx, "step-c"); !hit || err != nil {
		t.Fatalf("the step cache does not work after the erasure: hit=%v err=%v", hit, err)
	}
}
