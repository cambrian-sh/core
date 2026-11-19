package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memorynotes"
)

// ADR-0139 D14 on the agent plane: Orchestrator/GetMemoryNotes and
// Orchestrator/ProposeMemoryNote. Both resolve the run and its principal from the
// presented lease — there is no principal field, and nothing in the request can
// name whose notes are read or written (INV-5). A missing, foreign or expired lease
// is PERMISSION_DENIED, the same answer ExecuteTool gives.

// MemoryNotesPrincipalFunc resolves a lease binding to the run principal (the
// "sub"): whose personal notes the run sees. Bound at the composition root to the
// conversation owner / task beneficiary / configured default; "" = none.
type MemoryNotesPrincipalFunc func(ctx context.Context, b domain.LeaseBinding) string

// notesLease picks the lease from the request field, falling back to x-lease-id.
func notesLease(ctx context.Context, field string) domain.LeaseID {
	if field != "" {
		return domain.LeaseID(field)
	}
	return domain.LeaseID(mdValue(ctx, leaseHeader))
}

// notesCaller resolves the presented lease to the notes principal and the run key
// the frozen snapshot is held under, and returns ctx carrying the turn's surface
// (ADR-0085 D7, ENG-1615): the decision point sees a chat:telegram conversation's
// note reads and writes on that surface, not as a plain agent call.
func (s *Server) notesCaller(ctx context.Context, leaseField string) (context.Context, memorynotes.Principal, string, error) {
	if s.MemoryNotes == nil {
		return ctx, memorynotes.Principal{}, "", status.Error(codes.Unimplemented, "memory notes are not enabled on this kernel")
	}
	lease := notesLease(ctx, leaseField)
	if lease == "" {
		return ctx, memorynotes.Principal{}, "", status.Error(codes.PermissionDenied, "a lease is required")
	}
	b, ok := s.resolveLeaseFor(ctx, lease)
	if !ok {
		return ctx, memorynotes.Principal{}, "", status.Error(codes.PermissionDenied, "lease is missing, foreign or expired")
	}
	ctx = s.withBindingSurface(ctx, b)
	p := memorynotes.Principal{
		AgentID: b.AgentID,
		Session: b.SessionID,
		ActedBy: b.ActedBy,
	}
	if p.AgentID == "" {
		p.AgentID, _ = callerAgentIdentity(ctx)
	}
	if s.MemoryNotesPrincipal != nil {
		p.ID = s.MemoryNotesPrincipal(ctx, b)
	}
	// The snapshot is frozen per RUN: every step lease of one plan run shares its
	// RunID, so they all see the run-start notes. A lease minted outside a run (a
	// chat turn) is its own run.
	runKey := "lease:" + string(lease)
	if b.RunID != "" {
		runKey = "run:" + string(b.RunID)
	}
	return ctx, p, runKey, nil
}

// GetMemoryNotes returns the run's FROZEN run-start snapshot (ADR-0139 D14 / D3).
func (s *Server) GetMemoryNotes(ctx context.Context, req *pb.GetMemoryNotesRequest) (*pb.GetMemoryNotesResponse, error) {
	ctx, p, runKey, err := s.notesCaller(ctx, req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	snap, err := s.MemoryNotes.Snapshot(ctx, runKey, p)
	if err != nil {
		slog.Warn("ADR-0139 D14: memory notes snapshot failed", "err", err)
		return nil, status.Error(codes.Unavailable, "memory notes are unavailable")
	}
	resp := &pb.GetMemoryNotesResponse{Version: snap.Version, SnapshotAt: timestamppb.New(snap.At)}
	for _, d := range snap.Documents {
		resp.Documents = append(resp.Documents, &pb.MemoryNotesDocument{
			Scope:       scopeToPB(d.Scope),
			OwnerId:     d.OwnerID,
			Content:     d.Content,
			SizeChars:   int32(d.Size),
			BudgetChars: int32(d.Budget),
			Etag:        d.ETag,
			Truncated:   d.Truncated,
			UpdatedAt:   timestamppb.New(d.UpdatedAt),
		})
	}
	return resp, nil
}

// ProposeMemoryNote writes to the run principal's notes. A refusal is an OK
// response with accepted=false and a rejection — the model sees it as a tool
// result — never a silent trim or eviction.
func (s *Server) ProposeMemoryNote(ctx context.Context, req *pb.ProposeMemoryNoteRequest) (*pb.ProposeMemoryNoteResponse, error) {
	ctx, p, _, err := s.notesCaller(ctx, req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	scope := memorynotes.ScopePersonal
	switch req.GetScope() {
	case pb.MemoryNotesScope_MEMORY_NOTES_SCOPE_UNSPECIFIED, pb.MemoryNotesScope_MEMORY_NOTES_SCOPE_PERSONAL:
	case pb.MemoryNotesScope_MEMORY_NOTES_SCOPE_ORG:
		scope = memorynotes.ScopeOrg
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown scope %v", req.GetScope())
	}
	var op memorynotes.Op
	switch req.GetOp() {
	case pb.MemoryNoteOp_MEMORY_NOTE_OP_APPEND:
		op = memorynotes.OpAppend
	case pb.MemoryNoteOp_MEMORY_NOTE_OP_REPLACE_SECTION:
		op = memorynotes.OpReplaceSection
	case pb.MemoryNoteOp_MEMORY_NOTE_OP_REMOVE_SECTION:
		op = memorynotes.OpRemoveSection
	default:
		return nil, status.Error(codes.InvalidArgument, "op is required")
	}
	// ENG-2247: a note write from a session that read untrusted content asks the
	// person first (the Rule of Two); declined, nothing is written.
	if why, ok := s.gateKernelWrite(ctx, req.GetLeaseId(), "propose_memory_note",
		fmt.Sprintf("%s %s notes [%s]: %s", req.GetOp().String(), scope, req.GetSection(), previewText(req.GetContent(), 160)),
		map[string]any{"scope": string(scope), "op": req.GetOp().String(), "section": req.GetSection(), "content": req.GetContent()}); !ok {
		return &pb.ProposeMemoryNoteResponse{Accepted: false, Detail: why}, nil
	}
	res, err := s.MemoryNotes.Propose(ctx, p, memorynotes.Proposal{
		Scope: scope, Op: op, Section: req.GetSection(), Content: req.GetContent(),
		IfMatch: req.GetIfMatch(), Reason: req.GetReason(),
	})
	if errors.Is(err, memorynotes.ErrInvalid) {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err != nil {
		slog.Warn("ADR-0139 D14: memory note write failed", "err", err)
		return nil, status.Error(codes.Unavailable, "memory notes are unavailable")
	}
	return &pb.ProposeMemoryNoteResponse{
		Accepted:    res.Accepted,
		Rejection:   rejectionToPB(res.Rejection),
		Detail:      res.Detail,
		Etag:        res.ETag,
		SizeChars:   int32(res.Size),
		BudgetChars: int32(res.Budget),
	}, nil
}

func scopeToPB(s memorynotes.Scope) pb.MemoryNotesScope {
	if s == memorynotes.ScopeOrg {
		return pb.MemoryNotesScope_MEMORY_NOTES_SCOPE_ORG
	}
	return pb.MemoryNotesScope_MEMORY_NOTES_SCOPE_PERSONAL
}

func rejectionToPB(r memorynotes.Rejection) pb.MemoryNoteRejection {
	switch r {
	case memorynotes.RejectOverBudget:
		return pb.MemoryNoteRejection_MEMORY_NOTE_REJECTION_OVER_BUDGET
	case memorynotes.RejectInjection:
		return pb.MemoryNoteRejection_MEMORY_NOTE_REJECTION_INJECTION_FLAGGED
	case memorynotes.RejectScopeDenied:
		return pb.MemoryNoteRejection_MEMORY_NOTE_REJECTION_SCOPE_DENIED
	case memorynotes.RejectConflict:
		return pb.MemoryNoteRejection_MEMORY_NOTE_REJECTION_CONFLICT
	case memorynotes.RejectSectionNotFound:
		return pb.MemoryNoteRejection_MEMORY_NOTE_REJECTION_SECTION_NOT_FOUND
	}
	return pb.MemoryNoteRejection_MEMORY_NOTE_REJECTION_UNSPECIFIED
}
