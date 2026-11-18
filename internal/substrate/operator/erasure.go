package operator

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/internal/memorynotes"
)

// Operator-plane memory erasure: the namespace-wide compliance erase (ADR-0137 D3/D9,
// ENG-411 stage C; contract 0105, capability "memory-erasure").
//
// Owner decision 2026-11-16 (ENG-411): an operator compliance erase LIFTS the owner
// fence — a right-to-erasure request must reach all of the subject's data, including
// rows owner-fenced to another person's private scope. So every handler here reads at
// domain.ScopeMaintenance, the one greppable predicate that admits every owner's rows.
// It is handed to this path only because of the safeguards that decision names, all
// enforced here:
//   - the operator never sees content: previews and receipts carry ids, kinds and
//     counts (ErasureCandidateOp has no title, snippet or tag; ErasureReceiptOp no text).
//     One exception, owner decision 2026-11-19 (ENG-2147): a memory-notes candidate
//     carries its line (note_text) so the operator can check it names the right one —
//     read through the notes service's own operator read (GetMemoryNotes), so it shows
//     exactly what that operator can already read there and nothing else;
//   - the act is journalled (operator audit + the chokepoint's authz_erase log line) and
//     receipted (memory_erasures, and the premium signed receipt via ErasureObserver);
//   - only the operator plane reaches it, behind operator-role auth: PreviewForgetMemory
//     and ForgetMemory are commands by name and ListErasures/GetErasure are
//     operatorOnlyReads (authz.go), so a Viewer or Member is refused at the interceptor;
//     the chokepoint independently treats only a person with the operator/admin role as
//     an operator (anyone else is an agent-kind caller: writer-only).
//
// Agent and principal erases (network/forget_rpc.go) keep the owner fence.

// erasureCaller returns ctx carrying the operator's maintenance predicate (see above).
// The principal and roles were stamped by the operator interceptor.
func (s *Service) erasureCaller(ctx context.Context) (context.Context, error) {
	if s.forget == nil || !s.forget.Enabled() {
		return nil, status.Error(codes.Unimplemented, domain.ErrErasureDisabled.Error())
	}
	if domain.PrincipalFromContext(ctx).IsZero() {
		return nil, status.Error(codes.Unauthenticated, "no operator principal")
	}
	// ADR-0137 + owner decision 2026-11-16: the compliance erase lifts the owner fence.
	return domain.WithScope(ctx, domain.ScopeMaintenance), nil
}

// PreviewForgetMemory resolves a selector to the records it would erase: ids only.
func (s *Service) PreviewForgetMemory(ctx context.Context, req *pb.PreviewForgetMemoryOpRequest) (*pb.PreviewForgetMemoryOpResponse, error) {
	ctx, err := s.erasureCaller(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.forget.Preview(ctx, selectorFromOp(req.GetSelector()))
	if err != nil {
		return nil, erasureStatus("preview forget", err)
	}
	out := &pb.PreviewForgetMemoryOpResponse{
		PreviewToken: p.Token, ExpiresAt: timestamppb.New(p.ExpiresAt), CountsByKind: map[string]int32{},
	}
	texts := s.noteTexts(ctx, p.Candidates)
	for _, c := range p.Candidates {
		out.Candidates = append(out.Candidates, &pb.ErasureCandidateOp{Kind: c.Kind, Id: c.ID, Via: c.Via, DocumentId: c.DocumentID,
			NoteText: texts[c.Kind+":"+c.ID]})
		out.CountsByKind[c.Kind]++
	}
	return out, nil
}

// noteTexts returns, per memory-notes candidate (key kind:id), the text the preview
// shows (ENG-2147): a memory_note entry's line, a memory_notes_owner document's live
// text. Each document is read once, through the notes service's operator read with
// this operator as the caller — its own access check decides, and a document it does
// not let this operator read shows no text (the candidate still lists). Without the
// notes surface nothing is shown.
func (s *Service) noteTexts(ctx context.Context, cands []domain.ErasureCandidate) map[string]string {
	out := map[string]string{}
	if s.notes == nil {
		return out
	}
	actor, _, _ := PrincipalFromContext(ctx)
	caller := memorynotes.Caller{ID: actor}
	docs := map[memorynotes.Key]*string{}
	content := func(key memorynotes.Key) (string, bool) {
		if c, ok := docs[key]; ok {
			return derefOr(c), c != nil
		}
		full, err := s.notes.Get(ctx, caller, key)
		if err != nil {
			docs[key] = nil // unreadable or gone: no text, never an error for a preview
			return "", false
		}
		docs[key] = &full.Content
		return full.Content, true
	}
	for _, c := range cands {
		switch c.Kind {
		case domain.ErasureTargetMemoryNote:
			scope, owner, _, ok := domain.ParseMemoryNoteEntryID(c.ID)
			if !ok {
				continue
			}
			sc, err := memorynotes.ParseScope(scope)
			if err != nil {
				continue
			}
			key := memorynotes.Key{Scope: sc, OwnerID: owner}
			text, ok := content(key)
			if !ok {
				continue
			}
			for _, e := range memorynotes.Entries(text) {
				if domain.MemoryNoteEntryID(string(key.Scope), key.OwnerID, e.Text) == c.ID {
					out[c.Kind+":"+c.ID] = e.Text
					break
				}
			}
		case domain.ErasureTargetMemoryNotesOwner:
			if text, ok := content(memorynotes.Key{Scope: memorynotes.ScopePersonal, OwnerID: c.ID}); ok {
				out[c.Kind+":"+c.ID] = text
			}
		}
	}
	return out
}

func derefOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ForgetMemory executes a compliance erase. command_id + reason, audited; a retried
// command_id answers deduped with the receipt it produced.
func (s *Service) ForgetMemory(ctx context.Context, req *pb.ForgetMemoryOpRequest) (*pb.ForgetMemoryOpResponse, error) {
	ctx, err := s.erasureCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireCommand(req.GetCommandId(), req.GetReason()); err != nil {
		return nil, err
	}
	code := req.GetReasonCode()
	if code == "" {
		code = string(domain.ErasureReasonCompliance)
	}
	reason, err := domain.ParseErasureReason(code)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if prior, ok, err := s.priorCommand(ctx, req.GetCommandId()); err != nil {
		return nil, err
	} else if ok {
		resp := &pb.ForgetMemoryOpResponse{CommandId: req.GetCommandId(), Deduped: true}
		if rec, found, err := s.forget.Receipt(ctx, prior.TargetID); err == nil && found {
			resp.Receipt = receiptToOp(rec, time.Now())
		}
		return resp, nil
	}
	rec, err := s.forget.Forget(ctx, forget.Request{
		PreviewToken: req.GetPreviewToken(), Selector: selectorFromOp(req.GetSelector()), Reason: reason,
	})
	if err != nil {
		return nil, erasureStatus("forget", err)
	}
	actor, role, _ := PrincipalFromContext(ctx)
	if _, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "forget_memory",
		TargetType: "memory_erasure", TargetID: rec.RequestID,
		After: erasureAuditSummary(rec), Reason: req.GetReason(), Result: "ok",
	}); err != nil {
		// The erase committed; only its audit row failed. Say so, naming the request,
		// rather than report a bare error the caller would retry into a replay.
		return nil, status.Errorf(codes.Internal, "erasure %s committed but its audit entry failed: %v", rec.RequestID, err)
	}
	return &pb.ForgetMemoryOpResponse{CommandId: req.GetCommandId(), Receipt: receiptToOp(rec, time.Now())}, nil
}

// ListErasures lists receipts, newest first (every principal, or the one named).
func (s *Service) ListErasures(ctx context.Context, req *pb.ListErasuresOpRequest) (*pb.ListErasuresOpResponse, error) {
	ctx, err := s.erasureCaller(ctx)
	if err != nil {
		return nil, err
	}
	recs, err := s.forget.List(ctx, domain.ErasureReceiptFilter{
		Principal: req.GetPrincipal(), Limit: int(req.GetLimit()), Before: req.GetBefore(),
	})
	if err != nil {
		return nil, erasureStatus("list erasures", err)
	}
	now := time.Now()
	out := &pb.ListErasuresOpResponse{}
	for _, r := range recs {
		out.Receipts = append(out.Receipts, receiptToOp(r, now))
	}
	return out, nil
}

// GetErasure returns one receipt; an unknown request id is NotFound.
func (s *Service) GetErasure(ctx context.Context, req *pb.GetErasureOpRequest) (*pb.GetErasureOpResponse, error) {
	ctx, err := s.erasureCaller(ctx)
	if err != nil {
		return nil, err
	}
	rec, found, err := s.forget.Receipt(ctx, req.GetRequestId())
	if err != nil {
		return nil, erasureStatus("get erasure", err)
	}
	if !found {
		return nil, status.Errorf(codes.NotFound, "no erasure %q", req.GetRequestId())
	}
	return &pb.GetErasureOpResponse{Receipt: receiptToOp(rec, time.Now())}, nil
}

func selectorFromOp(sel *pb.ErasureSelectorOp) domain.ErasureSelector {
	return domain.ErasureSelector{
		DocumentIDs: sel.GetDocumentIds(), ThreadIDs: sel.GetThreadIds(), ChunkIDs: sel.GetChunkIds(),
		ExperienceIDs: sel.GetExperienceIds(), EntityRefs: sel.GetEntityRefs(),
		MemoryNoteIDs: sel.GetMemoryNoteIds(), MemoryNoteOwners: sel.GetMemoryNoteOwners(),
	}
}

func receiptToOp(r domain.ErasureReceipt, now time.Time) *pb.ErasureReceiptOp {
	out := &pb.ErasureReceiptOp{
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
		out.Overdue = r.Status == domain.ErasureStatusPendingAsync && now.After(r.AsyncDeadline)
	}
	for k, v := range r.Counts {
		out.Counts[k] = int32(v)
	}
	return out
}

// erasureAuditSummary is the audit entry's After value: content-free (status, selector
// kinds, targets and per-store counts).
func erasureAuditSummary(r domain.ErasureReceipt) string {
	kinds := append([]string(nil), r.SelectorKinds...)
	sort.Strings(kinds)
	// Strings, ints and a map[string]int: Marshal cannot fail.
	b, _ := json.Marshal(struct {
		Status        string         `json:"status"`
		SelectorKinds []string       `json:"selector_kinds"`
		Targets       int            `json:"targets"`
		Counts        map[string]int `json:"counts"`
	}{r.Status, kinds, r.Targets, r.Counts})
	return string(b)
}

// erasureStatus maps the erasure sentinels to distinct codes (ADR-0137 D3).
func erasureStatus(op string, err error) error {
	var code codes.Code
	switch {
	case errors.Is(err, domain.ErrErasureDisabled):
		code = codes.Unimplemented
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
