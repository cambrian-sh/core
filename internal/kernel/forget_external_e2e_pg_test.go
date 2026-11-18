package kernel

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory/erasureworker"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/storage"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/operator/operatortest"
)

// externalTracesStep stands in for a plugin tail step over an external service the
// kernel cannot delete from (the premium langfuse_traces step with no delete API
// configured): it reports ErrErasureNotErasableExternal, wrapped, and counts its runs.
type externalTracesStep struct{ runs int }

func (s *externalTracesStep) Step() string { return "langfuse_traces" }
func (s *externalTracesStep) RunErasureStep(_ context.Context, e domain.ErasedRecords) (int, error) {
	s.runs++
	if len(e.DocumentIDs) == 0 {
		return 0, fmt.Errorf("handed no erased documents")
	}
	return 0, fmt.Errorf("langfuse: deletion not configured: %w", domain.ErrErasureNotErasableExternal)
}

// TestPg_Forget_ExternalStepRecordedNotErasable (ENG-1968, ADR-0137 D7 / R4 row 31)
// through the real path: an agent's forget commits the erase, which queues the
// registered external step once (the receipt counts it as pending); the erasure worker
// runs it, it answers not_erasable_external, and the worker marks it processed with
// that outcome instead of retrying it. The request completes, nothing stays pending,
// and the stored receipt names the step in NotErasableExternal.
func TestPg_Forget_ExternalStepRecordedNotErasable(t *testing.T) {
	g := newGateWith(t, postgres.WithErasureTailSteps("langfuse_traces"))
	alice := agentGateCtx("eng1968-alice")
	doc, _ := g.ingest(alice, "eng1968-alice", "external", paragraphs(g.tok("ext"), 2))

	rec, err := g.svc.Forget(alice, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{doc}}})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if rec.Status != domain.ErasureStatusPendingAsync || rec.PendingSteps < 1 || len(rec.NotErasableExternal) != 0 {
		t.Fatalf("receipt = %+v: want pending_async counting the external step, nothing reported yet", rec)
	}
	if n := g.n(`SELECT count(*) FROM memory_erasure_outbox WHERE request_id = $1 AND step = 'langfuse_traces' AND ref = $1`, rec.RequestID); n != 1 {
		t.Fatalf("the external step was queued %d times, want once", n)
	}

	step := &externalTracesStep{}
	cas, err := storage.NewBBoltContentStore(filepath.Join(t.TempDir(), "content.db"), filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cas.Close() })
	w := erasureworker.New(g.eraser, cas, erasureworker.Config{
		Poll: time.Hour, MaxAttempts: 3, TailSteps: []domain.ErasureTailStep{step}}, nil)
	st := w.RunOnce(context.Background())
	w.RunOnce(context.Background())
	if st.NotErasable != 1 || st.Failed != 0 || step.runs != 1 {
		t.Fatalf("worker pass = %+v, step runs = %d: want one terminal not-erasable outcome, run once", st, step.runs)
	}
	if n := g.n(`SELECT count(*) FROM memory_erasure_outbox WHERE request_id = $1 AND processed_at IS NULL`, rec.RequestID); n != 0 {
		t.Fatalf("%d outbox steps still pending", n)
	}
	if n := g.n(`SELECT count(*) FROM memory_erasures WHERE request_id = $1 AND status = 'complete'`, rec.RequestID); n != 1 {
		t.Fatal("the request did not complete after its steps ran")
	}
	stored, found, err := g.eraser.Receipt(g.ctx, rec.RequestID)
	if err != nil || !found {
		t.Fatalf("Receipt: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(stored.NotErasableExternal, []string{"langfuse_traces"}) || stored.PendingSteps != 0 {
		t.Fatalf("stored receipt: not_erasable_external=%v pending=%d, want [langfuse_traces] and 0",
			stored.NotErasableExternal, stored.PendingSteps)
	}
	list, err := g.eraser.ListReceipts(g.ctx, domain.ErasureReceiptFilter{Principal: stored.Principal})
	if err != nil || len(list) == 0 || !reflect.DeepEqual(list[0].NotErasableExternal, []string{"langfuse_traces"}) {
		t.Fatalf("ListReceipts: %v %+v", err, list)
	}

	// ENG-2018 (contract 0108): both gRPC planes carry what the erase left behind.
	agentSrv := &subnetwork.Server{Forgetter: g.svc}
	aliceRPC := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", "eng1968-alice"))
	mine, err := agentSrv.ListMyErasures(aliceRPC, &pb.ListMyErasuresRequest{})
	if err != nil || len(mine.GetReceipts()) == 0 {
		t.Fatalf("ListMyErasures = %+v, %v", mine, err)
	}
	if got := mine.GetReceipts()[0]; got.GetRequestId() != rec.RequestID ||
		!reflect.DeepEqual(got.GetNotErasableExternal(), []string{"langfuse_traces"}) {
		t.Fatalf("agent-plane receipt = %+v, want not_erasable_external [langfuse_traces]", got)
	}
	opSvc := operatortest.New(t, nil, func(_ *operator.Deps, o *operator.Options) { o.Forget = g.svc })
	opCtx := domain.WithPrincipal(domain.WithPrincipalRoles(context.Background(), []string{"operator"}),
		domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "eng2018-ops"})
	got, err := opSvc.GetErasure(opCtx, &pb.GetErasureOpRequest{RequestId: rec.RequestID})
	if err != nil || !reflect.DeepEqual(got.GetReceipt().GetNotErasableExternal(), []string{"langfuse_traces"}) {
		t.Fatalf("operator GetErasure = %+v, %v; want not_erasable_external [langfuse_traces]", got, err)
	}
}
