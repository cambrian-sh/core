package network

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory/forget"
)

// Agent-plane memory erasure (ADR-0137 D2/D9, ENG-411 stage C; contract 0105; the
// query form of PreviewForget, contract 0106, ENG-1948).
//
// Every handler seeds the CALLER's principal and read predicate on ctx (AGENTS.md
// §2.4) before it reaches the forget service, which runs the one erasure chokepoint
// (MemoryStack.Eraser(), authz.EnforcingEraser): the principal is the authenticated
// x-agent-id, never a request field; the predicate is the decision point's ReadFilter
// for that agent on its surface, narrowed by the conversation owner its lease carries —
// exactly what QueryMemory reads under. The chokepoint then keeps the owner fence and
// admits only records the agent wrote (writer-only within read scope, ADR-0137 Q3).

// forgetCaller resolves who is asking and returns ctx carrying principal + predicate.
func (s *Server) forgetCaller(ctx context.Context) (context.Context, error) {
	if s.Forgetter == nil || !s.Forgetter.Enabled() {
		return nil, status.Error(codes.Unimplemented, domain.ErrErasureDisabled.Error())
	}
	agentID := callerAgentID(ctx)
	if agentID == "" {
		return nil, status.Error(codes.PermissionDenied, "unknown principal: no agent identity on the call")
	}
	if sessionID := s.resolveCallerSession(ctx); sessionID != "" {
		ctx = domain.WithSessionID(ctx, sessionID)
		ctx = s.withTurnSurface(ctx, sessionID)
	} else {
		ctx = s.withTurnSurface(ctx, "")
	}
	ctx = s.withConversationOwner(ctx)
	// ENG-2106: the person the agent acts for, from the presented lease (as on every
	// tool call): whose personal memory notes the query form searches and the
	// chokepoint lets this agent erase lines of. No lease → none, and no notes.
	ctx = s.withRunPrincipal(ctx, callerLeaseID(ctx, nil))
	p := domain.AgentPrincipal(agentID)
	pred, dec := s.authorizer().ReadFilter(ctx, p, domain.SurfaceFromContextOr(ctx, domain.SurfaceRef{Kind: domain.SurfaceAgent}))
	if pred == nil {
		return nil, status.Error(codes.PermissionDenied, "unknown principal: "+dec.Explain())
	}
	return domain.WithScope(domain.WithPrincipal(ctx, p), pred), nil
}

// PreviewForget resolves a selector — or, since contract 0106 (ENG-1948), a free-text
// query — to the records the calling agent can see. The query form runs the agent's own
// recall (forget.Service.PreviewQuery over the QueryService QueryMemory uses) under the
// principal and predicate forgetCaller seeded, lists only whole documents (or
// document-less chunks) the agent wrote, and attaches a clipped snippet of the agent's
// own text to each candidate. Exactly one of selector or query.
func (s *Server) PreviewForget(ctx context.Context, req *pb.PreviewForgetRequest) (*pb.PreviewForgetResponse, error) {
	ctx, err := s.forgetCaller(ctx)
	if err != nil {
		return nil, err
	}
	sel := selectorFromPB(req.GetSelector())
	query := strings.TrimSpace(req.GetQuery())
	var p forget.Preview
	switch {
	case query != "" && !sel.Normalized().IsEmpty():
		return nil, status.Error(codes.InvalidArgument, "preview forget: name a selector or a query, not both")
	case query != "":
		p, err = s.Forgetter.PreviewQuery(ctx, query, callerAgentID(ctx))
	default:
		p, err = s.Forgetter.Preview(ctx, sel)
	}
	if err != nil {
		return nil, forgetStatus("preview forget", err)
	}
	out := &pb.PreviewForgetResponse{PreviewToken: p.Token, ExpiresAt: timestamppb.New(p.ExpiresAt), Truncated: p.Truncated}
	for _, c := range p.Candidates {
		out.Candidates = append(out.Candidates, &pb.ErasureCandidate{
			Kind: c.Kind, Id: c.ID, Via: c.Via, DocumentId: c.DocumentID, Snippet: p.Snippets[forget.SnippetKey(c)],
		})
	}
	return out, nil
}

// Forget executes a previewed (or explicitly named) erasure.
func (s *Server) Forget(ctx context.Context, req *pb.ForgetRequest) (*pb.ForgetResponse, error) {
	ctx, err := s.forgetCaller(ctx)
	if err != nil {
		return nil, err
	}
	reason, err := domain.ParseErasureReason(req.GetReasonCode())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	rec, err := s.Forgetter.Forget(ctx, forget.Request{
		PreviewToken: req.GetPreviewToken(), Selector: selectorFromPB(req.GetSelector()), Reason: reason,
	})
	if err != nil {
		return nil, forgetStatus("forget", err)
	}
	return &pb.ForgetResponse{Receipt: receiptToPB(rec)}, nil
}

// ListMyErasures lists the calling agent's own receipts (the chokepoint forces it).
func (s *Server) ListMyErasures(ctx context.Context, req *pb.ListMyErasuresRequest) (*pb.ListMyErasuresResponse, error) {
	ctx, err := s.forgetCaller(ctx)
	if err != nil {
		return nil, err
	}
	recs, err := s.Forgetter.List(ctx, domain.ErasureReceiptFilter{Limit: int(req.GetLimit()), Before: req.GetBefore()})
	if err != nil {
		return nil, forgetStatus("list erasures", err)
	}
	out := &pb.ListMyErasuresResponse{}
	for _, r := range recs {
		out.Receipts = append(out.Receipts, receiptToPB(r))
	}
	return out, nil
}

func selectorFromPB(sel *pb.ErasureSelector) domain.ErasureSelector {
	return domain.ErasureSelector{
		DocumentIDs: sel.GetDocumentIds(), ThreadIDs: sel.GetThreadIds(), ChunkIDs: sel.GetChunkIds(),
		ExperienceIDs: sel.GetExperienceIds(), EntityRefs: sel.GetEntityRefs(),
		MemoryNoteIDs: sel.GetMemoryNoteIds(),
	}
}

func receiptToPB(r domain.ErasureReceipt) *pb.ErasureReceipt {
	out := &pb.ErasureReceipt{
		RequestId: r.RequestID, Principal: r.Principal, PrincipalKind: string(r.PrincipalKind),
		Reason: string(r.Reason), SelectorKinds: r.SelectorKinds, Status: r.Status,
		Targets: int32(r.Targets), PendingSteps: int32(r.PendingSteps), Replayed: r.Replayed,
		Counts: map[string]int32{}, NotErasableExternal: r.NotErasableExternal,
		RedactedMessages: r.RedactedMessages,
	}
	if !r.ErasedAt.IsZero() {
		out.ErasedAt = timestamppb.New(r.ErasedAt)
	}
	if !r.AsyncDeadline.IsZero() {
		out.AsyncDeadline = timestamppb.New(r.AsyncDeadline)
	}
	for k, v := range r.Counts {
		out.Counts[k] = int32(v)
	}
	return out
}

// forgetStatus maps the erasure sentinels to distinct gRPC codes (ADR-0137 D3,
// AGENTS.md §3.2): denied ≠ nothing-matched ≠ the caller's mistake ≠ disabled.
func forgetStatus(op string, err error) error {
	var code codes.Code
	switch {
	case errors.Is(err, domain.ErrErasureDisabled), errors.Is(err, forget.ErrQueryUnavailable):
		code = codes.Unimplemented
	case errors.Is(err, forget.ErrNoPrincipal):
		code = codes.PermissionDenied
	case errors.Is(err, authz.ErrScopeMissing), errors.Is(err, domain.ErrEraseDenied):
		code = codes.PermissionDenied
	case errors.Is(err, domain.ErrNothingToErase):
		code = codes.NotFound
	case errors.Is(err, domain.ErrPreviewUnknown):
		code = codes.FailedPrecondition
	case forget.IsCallerError(err):
		code = codes.InvalidArgument
	default:
		code = codes.Internal
	}
	return status.Errorf(code, "%s: %v", op, err)
}
