package operator

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory/forget"
)

// rawEraserFake stands in for postgres.PgErasureStore under the REAL enforcing eraser.
type rawEraserFake struct {
	records  []domain.ErasureCandidate
	erased   []domain.ErasureRequest
	receipts map[string]domain.ErasureReceipt
}

func (f *rawEraserFake) Resolve(_ context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	var out []domain.ErasureCandidate
	for _, r := range f.records {
		if slices.Contains(sel.DocumentIDs, r.ID) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *rawEraserFake) Erase(_ context.Context, req domain.ErasureRequest) (domain.ErasureReceipt, error) {
	f.erased = append(f.erased, req)
	rec := domain.ErasureReceipt{RequestID: req.ID(), Principal: req.Principal, PrincipalKind: req.PrincipalKind,
		Reason: req.Reason, SelectorKinds: req.Selector.Kinds(), Status: domain.ErasureStatusComplete,
		Counts: map[string]int{"documents": len(req.Selector.DocumentIDs)}, Targets: len(req.Selector.DocumentIDs)}
	if f.receipts == nil {
		f.receipts = map[string]domain.ErasureReceipt{}
	}
	f.receipts[rec.RequestID] = rec
	return rec, nil
}

func (f *rawEraserFake) Receipt(_ context.Context, id string) (domain.ErasureReceipt, bool, error) {
	r, ok := f.receipts[id]
	return r, ok, nil
}

func (f *rawEraserFake) ListReceipts(context.Context, domain.ErasureReceiptFilter) ([]domain.ErasureReceipt, error) {
	var out []domain.ErasureReceipt
	for _, r := range f.receipts {
		out = append(out, r)
	}
	return out, nil
}

// operatorErasureCtx is what the interceptor leaves on ctx for an operator person.
func operatorErasureCtx(role Role, roles ...string) context.Context {
	ctx := context.WithValue(context.Background(), principalCtxKey{}, principalInfo{principal: "ops", role: role})
	ctx = domain.WithPrincipalRoles(ctx, roles)
	return domain.WithPrincipal(ctx, domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "ops"})
}

func newErasureService(t *testing.T, raw *rawEraserFake) (*Service, *InMemoryAuditStore) {
	t.Helper()
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{}, nil)
	s := NewService(NewSpool(SpoolConfig{}))
	audit := NewInMemoryAuditStore()
	s.SetCommandSources(audit, nil)
	s.SetForgetService(forget.New(e, forget.Config{PolicyFingerprint: "t"}))
	return s, audit
}

// Owner decision 2026-11-16: the operator compliance erase LIFTS the owner fence (a
// record owner-fenced to another person is previewed and erased), the preview carries
// ids only, and the erase is audited and idempotent on command_id.
func TestForgetMemory_OperatorLiftsOwnerFenceIdsOnlyAudited(t *testing.T) {
	raw := &rawEraserFake{records: []domain.ErasureCandidate{{
		Kind: domain.ErasureTargetDocument, ID: "d-private", Via: "d-private", DocumentID: "d-private",
		WrittenBy: "agent-x", Tags: []string{domain.OwnerTag("alice")},
	}}}
	s, audit := newErasureService(t, raw)
	ctx := operatorErasureCtx(RoleOperator, "operator")

	p, err := s.PreviewForgetMemory(ctx, &pb.PreviewForgetMemoryOpRequest{
		Selector: &pb.ErasureSelectorOp{DocumentIds: []string{"d-private"}}})
	if err != nil {
		t.Fatalf("preview of an owner-fenced row by an operator: %v", err)
	}
	if len(p.GetCandidates()) != 1 || p.GetCandidates()[0].GetId() != "d-private" || p.GetCountsByKind()["document"] != 1 {
		t.Fatalf("preview = %+v", p)
	}

	// Without a command_id/reason the command is refused before anything runs.
	if _, err := s.ForgetMemory(ctx, &pb.ForgetMemoryOpRequest{PreviewToken: p.GetPreviewToken()}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing command_id/reason: %v", err)
	}
	resp, err := s.ForgetMemory(ctx, &pb.ForgetMemoryOpRequest{
		CommandId: "cmd-1", Reason: "KVKK request #7", PreviewToken: p.GetPreviewToken()})
	if err != nil {
		t.Fatal(err)
	}
	rec := resp.GetReceipt()
	if rec.GetPrincipalKind() != string(domain.ErasureByOperator) || rec.GetReason() != string(domain.ErasureReasonCompliance) || rec.GetCounts()["documents"] != 1 {
		t.Fatalf("receipt = %+v", rec)
	}
	entries, _ := audit.Query(ctx, AuditFilter{CommandID: "cmd-1"})
	if len(entries) != 1 || entries[0].TargetID != rec.GetRequestId() || entries[0].ActionType != "forget_memory" ||
		strings.Contains(entries[0].After, "d-private") {
		t.Fatalf("audit = %+v (must name the request, never ids or content in After)", entries)
	}
	again, err := s.ForgetMemory(ctx, &pb.ForgetMemoryOpRequest{CommandId: "cmd-1", Reason: "KVKK request #7", PreviewToken: p.GetPreviewToken()})
	if err != nil || !again.GetDeduped() || again.GetReceipt().GetRequestId() != rec.GetRequestId() || len(raw.erased) != 1 {
		t.Fatalf("retry = %+v, %v; erased %d times", again, err, len(raw.erased))
	}

	got, err := s.GetErasure(ctx, &pb.GetErasureOpRequest{RequestId: rec.GetRequestId()})
	if err != nil || got.GetReceipt().GetRequestId() != rec.GetRequestId() {
		t.Fatalf("GetErasure = %+v, %v", got, err)
	}
	if _, err := s.GetErasure(ctx, &pb.GetErasureOpRequest{RequestId: "erase:none"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown receipt: %v", err)
	}
	list, err := s.ListErasures(ctx, &pb.ListErasuresOpRequest{})
	if err != nil || len(list.GetReceipts()) != 1 {
		t.Fatalf("ListErasures = %+v, %v", list, err)
	}
}

// Disabled answers Unimplemented on every door; the role gate keeps every erasure RPC
// from Viewers; a person without the operator role is not an operator at the chokepoint.
func TestForgetMemory_DisabledRoleGateAndNonOperator(t *testing.T) {
	off := NewService(NewSpool(SpoolConfig{}))
	off.SetForgetService(forget.New(nil, forget.Config{}))
	if _, err := off.PreviewForgetMemory(operatorErasureCtx(RoleOperator, "operator"), &pb.PreviewForgetMemoryOpRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("disabled: %v", err)
	}
	for _, m := range []string{"PreviewForgetMemory", "ForgetMemory", "ListErasures", "GetErasure"} {
		if !isOperatorOnly(operatorMethodPrefix+m, nil) {
			t.Errorf("%s must be operator-only", m)
		}
	}

	raw := &rawEraserFake{records: []domain.ErasureCandidate{{Kind: domain.ErasureTargetDocument, ID: "d1", Via: "d1", DocumentID: "d1", WrittenBy: "someone"}}}
	s, _ := newErasureService(t, raw)
	// A role set without operator/admin is an agent-kind caller at the chokepoint:
	// agent erasure is off by default, so the erase is denied.
	_, err := s.ForgetMemory(operatorErasureCtx(RoleOperator, "viewer"), &pb.ForgetMemoryOpRequest{
		CommandId: "c", Reason: "r", Selector: &pb.ErasureSelectorOp{DocumentIds: []string{"d1"}}})
	if status.Code(err) != codes.PermissionDenied || len(raw.erased) != 0 {
		t.Fatalf("non-operator erase: %v (erased %d)", err, len(raw.erased))
	}
	if _, err := s.ForgetMemory(operatorErasureCtx(RoleOperator, "operator"), &pb.ForgetMemoryOpRequest{
		CommandId: "c2", Reason: "r", Selector: &pb.ErasureSelectorOp{DocumentIds: []string{"nope"}}}); status.Code(err) != codes.NotFound {
		t.Fatalf("nothing to erase: %v", err)
	}
	if _, err := s.ForgetMemory(operatorErasureCtx(RoleOperator, "operator"), &pb.ForgetMemoryOpRequest{
		CommandId: "c3", Reason: "r", PreviewToken: "pv:forged"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unknown token: %v", err)
	}
}

// ENG-2018 (contract 0108): the operator receipt names the ADR-0137 D7 async steps
// that could not erase from an external service, as the Go receipt does.
func TestReceiptToOp_CarriesNotErasableExternal(t *testing.T) {
	got := receiptToOp(domain.ErasureReceipt{RequestID: "erase:x", Status: domain.ErasureStatusComplete,
		NotErasableExternal: []string{"langfuse_traces"}}, time.Now())
	if !slices.Equal(got.GetNotErasableExternal(), []string{"langfuse_traces"}) {
		t.Fatalf("not_erasable_external = %v, want [langfuse_traces]", got.GetNotErasableExternal())
	}
	if empty := receiptToOp(domain.ErasureReceipt{RequestID: "erase:y"}, time.Now()); len(empty.GetNotErasableExternal()) != 0 {
		t.Fatalf("a receipt with nothing left behind reports %v", empty.GetNotErasableExternal())
	}
}
