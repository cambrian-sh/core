package network

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory/forget"
)

type forgetRawFake struct {
	records []domain.ErasureCandidate
	erased  []domain.ErasureRequest
}

func (f *forgetRawFake) Resolve(_ context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	var out []domain.ErasureCandidate
	for _, r := range f.records {
		if slices.Contains(sel.DocumentIDs, r.ID) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *forgetRawFake) Erase(_ context.Context, req domain.ErasureRequest) (domain.ErasureReceipt, error) {
	f.erased = append(f.erased, req)
	return domain.ErasureReceipt{RequestID: req.ID(), Principal: req.Principal, PrincipalKind: req.PrincipalKind,
		Reason: req.Reason, Status: domain.ErasureStatusComplete, Counts: map[string]int{"documents": 1}}, nil
}

func (f *forgetRawFake) Receipt(context.Context, string) (domain.ErasureReceipt, bool, error) {
	return domain.ErasureReceipt{}, false, nil
}

func (f *forgetRawFake) ListReceipts(_ context.Context, flt domain.ErasureReceiptFilter) ([]domain.ErasureReceipt, error) {
	return []domain.ErasureReceipt{{RequestID: "erase:mine", Principal: flt.Principal}}, nil
}

func agentCall(id string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", id))
}

// The agent plane: the caller is the authenticated agent (never a request field), its
// read predicate rides ctx, and the chokepoint keeps the owner fence and the writer-only
// rule — another person's private row is ABSENT (NotFound), a visible row someone else
// wrote is DENIED, its own row is erased after a preview.
func TestForgetRPC_AgentWriterOnlyOwnerFenceKept(t *testing.T) {
	raw := &forgetRawFake{records: []domain.ErasureCandidate{
		{Kind: domain.ErasureTargetDocument, ID: "mine", Via: "mine", DocumentID: "mine", WrittenBy: "alice"},
		{Kind: domain.ErasureTargetDocument, ID: "bobs", Via: "bobs", DocumentID: "bobs", WrittenBy: "bob"},
		{Kind: domain.ErasureTargetDocument, ID: "private", Via: "private", DocumentID: "private", WrittenBy: "alice",
			Tags: []string{domain.OwnerTag("carol")}},
	}}
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{AgentErase: true}, nil)
	s := &Server{Forgetter: forget.New(e, forget.Config{PolicyFingerprint: "t"})}
	ctx := agentCall("alice")

	if _, err := s.PreviewForget(ctx, &pb.PreviewForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"private"}}}); status.Code(err) != codes.NotFound {
		t.Fatalf("owner-fenced row of another person must be absent: %v", err)
	}
	if _, err := s.Forget(ctx, &pb.ForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"bobs"}}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("someone else's row must be denied: %v", err)
	}
	p, err := s.PreviewForget(ctx, &pb.PreviewForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"mine"}}})
	if err != nil || len(p.GetCandidates()) != 1 || p.GetPreviewToken() == "" {
		t.Fatalf("preview = %+v, %v", p, err)
	}
	resp, err := s.Forget(ctx, &pb.ForgetRequest{PreviewToken: p.GetPreviewToken(), ReasonCode: "correction"})
	if err != nil {
		t.Fatal(err)
	}
	if r := resp.GetReceipt(); r.GetPrincipal() != "alice" || r.GetPrincipalKind() != "agent" || r.GetReason() != "correction" {
		t.Fatalf("receipt = %+v", r)
	}
	if len(raw.erased) != 1 || !slices.Equal(raw.erased[0].Selector.DocumentIDs, []string{"mine"}) {
		t.Fatalf("erased = %+v", raw.erased)
	}
	if _, err := s.Forget(ctx, &pb.ForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"mine"}}, ReasonCode: "because"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown reason code: %v", err)
	}
	list, err := s.ListMyErasures(ctx, &pb.ListMyErasuresRequest{})
	if err != nil || len(list.GetReceipts()) != 1 || list.GetReceipts()[0].GetPrincipal() != "alice" {
		t.Fatalf("ListMyErasures = %+v, %v (must be forced to the caller)", list, err)
	}
}

// No agent identity is refused; erasure off answers Unimplemented; agent erasure off
// (the default) is a denial, never a silent success.
func TestForgetRPC_UnknownCallerDisabledAndAgentEraseOff(t *testing.T) {
	raw := &forgetRawFake{records: []domain.ErasureCandidate{{Kind: domain.ErasureTargetDocument, ID: "mine", Via: "mine", DocumentID: "mine", WrittenBy: "alice"}}}
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{}, nil)
	s := &Server{Forgetter: forget.New(e, forget.Config{})}
	if _, err := s.Forget(context.Background(), &pb.ForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"mine"}}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no agent identity: %v", err)
	}
	if _, err := s.Forget(agentCall("alice"), &pb.ForgetRequest{Selector: &pb.ErasureSelector{DocumentIds: []string{"mine"}}}); status.Code(err) != codes.PermissionDenied || len(raw.erased) != 0 {
		t.Fatalf("agent erase off by default must deny: %v", err)
	}
	for _, off := range []*Server{{}, {Forgetter: forget.New(nil, forget.Config{})}} {
		if _, err := off.PreviewForget(agentCall("alice"), &pb.PreviewForgetRequest{}); status.Code(err) != codes.Unimplemented {
			t.Fatalf("disabled: %v", err)
		}
	}
}
