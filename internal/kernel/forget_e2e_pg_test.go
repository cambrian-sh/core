package kernel

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory/forget"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

type recordingErasureObserver struct {
	mu   sync.Mutex
	seen []domain.ErasureReceipt
}

func (o *recordingErasureObserver) ObserveErasure(_ context.Context, r domain.ErasureReceipt) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, r)
}

// ADR-0137 S6 end to end (ENG-411 stage C): the production composition — a real
// Postgres adapter, the raw PgErasureStore as MemoryBackend.Eraser, NewMemoryStack with
// execution.erasure on, the forget service over MemoryStack.Eraser() — driven through
// the real agent-plane and operator-plane RPC handlers, with the effect checked in
// Postgres:
//   - an agent cannot erase another agent's memory (PermissionDenied, row kept);
//   - an agent cannot even see a row owner-fenced to another person (NotFound);
//   - an agent previews its own memory and erases it with the token; the row is gone,
//     a tombstone and a receipt remain, the observer saw it once, a retry replays;
//   - an operator's compliance erase lifts the owner fence and removes the fenced row,
//     audited, idempotent on command_id.
//
// Requires PG_TEST_DSN pointing at a scratch database (the adapter auto-migrates it).
func TestPg_Forget_EndToEndThroughBothPlanes(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Erasure.Enabled = true
	cfg.Execution.Erasure.AgentEraseEnabled = true
	ctx := context.Background()
	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)

	obs := &recordingErasureObserver{}
	backend := NewMemoryBackend(vec)
	backend.Eraser = postgres.NewPgErasureStore(postgres.NewPgDocumentLifecycle(postgres.NewPgKnowledgeStore(vec.Pool(), nil)),
		postgres.WithErasureObserver(obs))
	mem := NewMemoryStack(backend, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{})
	if mem.Eraser() == nil {
		t.Fatal("erasure enabled but MemoryStack.Eraser() is nil")
	}
	svc := forget.New(mem.Eraser(), forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "e2e"})

	stamp := time.Now().UTC().Format("150405.000000")
	mine, bobs, fenced := "eng411c-mine-"+stamp, "eng411c-bobs-"+stamp, "eng411c-fenced-"+stamp
	maint := domain.WithScope(ctx, domain.ScopeMaintenance)
	t.Cleanup(func() {
		for _, id := range []string{mine, bobs, fenced} {
			_ = vec.Delete(maint, id)
		}
		_, _ = vec.Pool().Exec(context.Background(),
			`DELETE FROM memory_erasures WHERE principal IN ('eng411c-alice', 'eng411c-ops')`)
	})
	vector := make([]float32, cfg.Embedder.Dimensions)
	vector[0] = 1
	write := func(agent, id string) {
		t.Helper()
		d := &domain.Document{ID: id, DocumentType: domain.DocTypeMnemonicFact, Text: "eng411c secret " + id,
			Embedding: domain.Embedding{Vector: vector}, ActivationStrength: 0.5}
		if err := mem.Writer().Save(domain.WithPrincipal(ctx, domain.AgentPrincipal(agent)), d); err != nil {
			t.Fatalf("Writer().Save %s: %v", id, err)
		}
	}
	write("eng411c-alice", mine)
	write("eng411c-bob", bobs)
	write("eng411c-alice", fenced)
	// Owner-fence the third row to another person (as a personal ingress would).
	if _, err := vec.Pool().Exec(ctx, `UPDATE chunks SET metadata = jsonb_set(metadata, '{tags}', '["owner:eng411c-carol"]'::jsonb) WHERE id = $1`, fenced); err != nil {
		t.Fatalf("fence: %v", err)
	}
	rows := func(id string) int {
		t.Helper()
		var n int
		if err := vec.Pool().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	tombstones := func(id string) int {
		t.Helper()
		var n int
		if err := vec.Pool().QueryRow(ctx, `SELECT count(*) FROM memory_erasure_targets WHERE target_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// ── Agent plane ──
	agentSrv := &subnetwork.Server{Forgetter: svc}
	alice := metadata.NewIncomingContext(ctx, metadata.Pairs("x-agent-id", "eng411c-alice"))
	sel := func(id string) *pb.ErasureSelector { return &pb.ErasureSelector{ChunkIds: []string{id}} }

	if _, err := agentSrv.Forget(alice, &pb.ForgetRequest{Selector: sel(bobs)}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("alice erasing bob's memory: %v, want PermissionDenied", err)
	}
	if rows(bobs) != 1 {
		t.Fatal("a denied erase must leave bob's row")
	}
	if _, err := agentSrv.PreviewForget(alice, &pb.PreviewForgetRequest{Selector: sel(fenced)}); status.Code(err) != codes.NotFound {
		t.Fatalf("alice previewing a row fenced to carol: %v, want NotFound (absent)", err)
	}
	p, err := agentSrv.PreviewForget(alice, &pb.PreviewForgetRequest{Selector: sel(mine)})
	if err != nil || len(p.GetCandidates()) != 1 || p.GetCandidates()[0].GetId() != mine {
		t.Fatalf("preview of alice's own row = %+v, %v", p, err)
	}
	if rows(mine) != 1 {
		t.Fatal("a preview must erase nothing")
	}
	resp, err := agentSrv.Forget(alice, &pb.ForgetRequest{PreviewToken: p.GetPreviewToken()})
	if err != nil {
		t.Fatalf("Forget(token): %v", err)
	}
	if rows(mine) != 0 || tombstones(mine) != 1 {
		t.Fatalf("after Forget: chunk rows %d (want 0), tombstones %d (want 1)", rows(mine), tombstones(mine))
	}
	if r := resp.GetReceipt(); r.GetPrincipal() != "eng411c-alice" || r.GetPrincipalKind() != "agent" || r.GetReplayed() {
		t.Fatalf("receipt = %+v", r)
	}
	again, err := agentSrv.Forget(alice, &pb.ForgetRequest{PreviewToken: p.GetPreviewToken()})
	if err != nil || !again.GetReceipt().GetReplayed() || again.GetReceipt().GetRequestId() != resp.GetReceipt().GetRequestId() {
		t.Fatalf("retry = %+v, %v; want the same request replayed", again, err)
	}
	list, err := agentSrv.ListMyErasures(alice, &pb.ListMyErasuresRequest{})
	if err != nil || len(list.GetReceipts()) == 0 || list.GetReceipts()[0].GetPrincipal() != "eng411c-alice" {
		t.Fatalf("ListMyErasures = %+v, %v", list, err)
	}

	// ── Operator plane: the compliance erase lifts the owner fence ──
	opSvc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	audit := operator.NewInMemoryAuditStore()
	opSvc.SetCommandSources(audit, nil)
	opSvc.SetForgetService(svc)
	opCtx := domain.WithPrincipal(domain.WithPrincipalRoles(ctx, []string{"operator"}),
		domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "eng411c-ops"})
	op, err := opSvc.PreviewForgetMemory(opCtx, &pb.PreviewForgetMemoryOpRequest{Selector: &pb.ErasureSelectorOp{ChunkIds: []string{fenced}}})
	if err != nil || len(op.GetCandidates()) != 1 {
		t.Fatalf("operator preview of the fenced row = %+v, %v", op, err)
	}
	fr, err := opSvc.ForgetMemory(opCtx, &pb.ForgetMemoryOpRequest{CommandId: "eng411c-" + stamp, Reason: "KVKK erasure request", PreviewToken: op.GetPreviewToken()})
	if err != nil {
		t.Fatalf("ForgetMemory: %v", err)
	}
	if rows(fenced) != 0 || tombstones(fenced) != 1 || fr.GetReceipt().GetPrincipalKind() != "operator" || fr.GetReceipt().GetReason() != "compliance" {
		t.Fatalf("operator erase: rows %d tombstones %d receipt %+v", rows(fenced), tombstones(fenced), fr.GetReceipt())
	}
	dup, err := opSvc.ForgetMemory(opCtx, &pb.ForgetMemoryOpRequest{CommandId: "eng411c-" + stamp, Reason: "KVKK erasure request", PreviewToken: op.GetPreviewToken()})
	if err != nil || !dup.GetDeduped() || dup.GetReceipt().GetRequestId() != fr.GetReceipt().GetRequestId() {
		t.Fatalf("retried command = %+v, %v", dup, err)
	}
	all, err := opSvc.ListErasures(opCtx, &pb.ListErasuresOpRequest{})
	if err != nil || len(all.GetReceipts()) < 2 {
		t.Fatalf("operator ListErasures = %d receipts, %v; want both erasures", len(all.GetReceipts()), err)
	}
	if rows(bobs) != 1 {
		t.Fatal("bob's row must survive everything above")
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.seen) != 2 {
		t.Fatalf("observer saw %d receipts, want 2 (the replay is not observed)", len(obs.seen))
	}
}
