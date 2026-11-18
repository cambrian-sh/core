package operator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memorynotes"
)

// ADR-0139 D14 on the operator plane (capability "memory-notes"): the operator half
// of the notes the agent plane reads as a frozen run snapshot. Reads are open to any
// authenticated role and scoped by the caller through the notes service's C8 check
// (an unreadable document is NOT_FOUND, like an unknown one). Mutations are
// Operator-only (the interceptor's naming rule: Set*/Delete* are commands),
// command_id + mandatory reason, audited, idempotent on command_id.
//
// An edit lands for the principal's NEXT run; a run in flight keeps its snapshot.

// MemoryNotesAdmin is the notes service as this plane uses it. *memorynotes.Service
// satisfies it; the port exists so the handlers can be tested without the store.
type MemoryNotesAdmin interface {
	List(ctx context.Context, c memorynotes.Caller, f memorynotes.ListFilter) ([]memorynotes.Summary, error)
	Get(ctx context.Context, c memorynotes.Caller, key memorynotes.Key) (memorynotes.Full, error)
	Set(ctx context.Context, c memorynotes.Caller, key memorynotes.Key, content, ifMatch string, override bool, reason string) (memorynotes.WriteResult, error)
	Delete(ctx context.Context, c memorynotes.Caller, key memorynotes.Key, reason string) (memorynotes.Document, error)
	SetBudget(ctx context.Context, c memorynotes.Caller, scope memorynotes.Scope, owner string, budget int, clear bool) error
}

func (s *Service) notesCaller(ctx context.Context) (memorynotes.Caller, error) {
	if s.notes == nil {
		return memorynotes.Caller{}, status.Error(codes.Unimplemented, "memory notes are not enabled on this kernel")
	}
	actor, _, _ := PrincipalFromContext(ctx)
	return memorynotes.Caller{ID: actor}, nil
}

func notesErr(err error) error {
	switch {
	case errors.Is(err, memorynotes.ErrNotFound):
		return status.Error(codes.NotFound, "memory notes document not found")
	case errors.Is(err, memorynotes.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return status.Errorf(codes.Internal, "memory notes: %v", err)
}

func notesKey(scope, owner string) (memorynotes.Key, error) {
	sc, err := memorynotes.ParseScope(scope)
	if err != nil {
		return memorynotes.Key{}, err
	}
	if strings.TrimSpace(owner) == "" {
		return memorynotes.Key{}, fmt.Errorf("%w: owner_id is required", memorynotes.ErrInvalid)
	}
	return memorynotes.Key{Scope: sc, OwnerID: owner}, nil
}

func notesTime(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// ListMemoryNotes lists the documents the caller may read, without bodies.
func (s *Service) ListMemoryNotes(ctx context.Context, req *pb.ListMemoryNotesOpRequest) (*pb.ListMemoryNotesOpResponse, error) {
	c, err := s.notesCaller(ctx)
	if err != nil {
		return nil, err
	}
	var f memorynotes.ListFilter
	if req.GetScope() != "" {
		sc, err := memorynotes.ParseScope(req.GetScope())
		if err != nil {
			return nil, notesErr(err)
		}
		f.Scope = sc
	}
	f.OwnerID = req.GetOwnerId()
	sums, err := s.notes.List(ctx, c, f)
	if err != nil {
		return nil, notesErr(err)
	}
	page, lo, hi := paginate(len(sums), req.GetPage(), req.GetPageSize())
	resp := &pb.ListMemoryNotesOpResponse{Total: int32(len(sums)), Page: page}
	for _, x := range sums[lo:hi] {
		resp.Notes = append(resp.Notes, &pb.MemoryNotesSummaryOp{
			Scope: string(x.Scope), OwnerId: x.OwnerID, SizeChars: int32(x.Size), BudgetChars: int32(x.Budget),
			Etag: x.ETag, UpdatedAt: notesTime(x.UpdatedAt), UpdatedBy: x.UpdatedBy, OverBudget: x.OverBudget,
		})
	}
	return resp, nil
}

// GetMemoryNotes returns one whole document. Unknown and unreadable: NOT_FOUND.
func (s *Service) GetMemoryNotes(ctx context.Context, req *pb.GetMemoryNotesOpRequest) (*pb.MemoryNotesOp, error) {
	c, err := s.notesCaller(ctx)
	if err != nil {
		return nil, err
	}
	key, err := notesKey(req.GetScope(), req.GetOwnerId())
	if err != nil {
		return nil, notesErr(err)
	}
	full, err := s.notes.Get(ctx, c, key)
	if err != nil {
		return nil, notesErr(err)
	}
	return &pb.MemoryNotesOp{
		Scope: string(full.Scope), OwnerId: full.OwnerID, Content: full.Content,
		SizeChars: int32(full.Size), BudgetChars: int32(full.Budget), Etag: full.ETag,
		UpdatedAt: notesTime(full.UpdatedAt), UpdatedBy: full.UpdatedBy, BudgetOverridden: full.BudgetOverridden,
	}, nil
}

// priorCommand returns the audit entry already recorded for commandID, if any.
//
// The notes commands look the command up BEFORE applying, and record AFTER — the
// reverse of runMutation — because their audit entry must carry the outcome: a
// refused SetMemoryNotes is audited as refused, and a DeleteMemoryNotes entry holds
// the cleared text (Before), which only exists once the delete has run. A retry of
// a recorded command applies nothing and reports deduped. Two concurrent first
// deliveries of one command_id can both apply; both writes are etag-guarded, the
// audit keeps one entry (Record's CommandID dedupe), and a whole-document Set or a
// Delete applied twice converges on the same state.
func (s *Service) priorCommand(ctx context.Context, commandID string) (domain.AuditEntry, bool, error) {
	if s.audit == nil {
		return domain.AuditEntry{}, false, status.Error(codes.Unimplemented, "operator audit store not configured")
	}
	prior, err := s.audit.Query(ctx, AuditFilter{CommandID: commandID, Limit: 1})
	if err != nil {
		return domain.AuditEntry{}, false, status.Errorf(codes.Internal, "audit lookup: %v", err)
	}
	if len(prior) == 0 {
		return domain.AuditEntry{}, false, nil
	}
	return prior[0], true, nil
}

func requireCommand(commandID, reason string) error {
	if commandID == "" || reason == "" {
		return status.Error(codes.InvalidArgument, "command_id and reason are required")
	}
	return nil
}

func (s *Service) recordNotesCommand(ctx context.Context, commandID, reason, action, targetID, before, after, result string) error {
	actor, role, _ := PrincipalFromContext(ctx)
	_, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: commandID, At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: action,
		TargetType: "memory_notes", TargetID: targetID,
		Before: before, After: after, Reason: reason, Result: result,
	})
	return err
}

// SetMemoryNotes replaces a whole document. Same budget and scan as an agent write;
// a refusal is accepted=false with a rejection, never a trim.
func (s *Service) SetMemoryNotes(ctx context.Context, req *pb.SetMemoryNotesOpRequest) (*pb.SetMemoryNotesOpResponse, error) {
	c, err := s.notesCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireCommand(req.GetCommandId(), req.GetReason()); err != nil {
		return nil, err
	}
	key, err := notesKey(req.GetScope(), req.GetOwnerId())
	if err != nil {
		return nil, notesErr(err)
	}
	if prior, ok, err := s.priorCommand(ctx, req.GetCommandId()); err != nil {
		return nil, err
	} else if ok {
		resp := &pb.SetMemoryNotesOpResponse{CommandId: req.GetCommandId(), Deduped: true}
		resp.Accepted = strings.HasPrefix(prior.Result, "accepted")
		if r, found := strings.CutPrefix(prior.Result, "rejected:"); found {
			resp.Rejection = r
		}
		if full, err := s.notes.Get(ctx, c, key); err == nil {
			resp.Etag, resp.SizeChars, resp.BudgetChars = full.ETag, int32(full.Size), int32(full.Budget)
		}
		return resp, nil
	}
	res, err := s.notes.Set(ctx, c, key, req.GetContent(), req.GetIfMatch(), req.GetOverrideInjectionFlag(), req.GetReason())
	if err != nil {
		return nil, notesErr(err)
	}
	result := "accepted"
	after := fmt.Sprintf("etag %s, %d chars", res.ETag, res.Size)
	if res.OverriddenFlag != "" {
		// The override is the audited judgement: name what the scan found.
		result = "accepted:injection_flag_overridden"
		after += "; injection scan flagged (" + res.OverriddenFlag + "), stored by operator override"
	}
	if !res.Accepted {
		result = "rejected:" + string(res.Rejection)
		after = res.Detail
	}
	if err := s.recordNotesCommand(ctx, req.GetCommandId(), req.GetReason(), "set_memory_notes", key.String(),
		"", after, result); err != nil {
		return nil, err
	}
	return &pb.SetMemoryNotesOpResponse{
		CommandId: req.GetCommandId(), Accepted: res.Accepted, Rejection: string(res.Rejection),
		Detail: res.Detail, Etag: res.ETag, SizeChars: int32(res.Size), BudgetChars: int32(res.Budget),
	}, nil
}

// DeleteMemoryNotes clears a document. The cleared text is kept in the audit
// record (Before), so a delete is recoverable by an operator.
func (s *Service) DeleteMemoryNotes(ctx context.Context, req *pb.DeleteMemoryNotesOpRequest) (*pb.CommandAck, error) {
	c, err := s.notesCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireCommand(req.GetCommandId(), req.GetReason()); err != nil {
		return nil, err
	}
	key, err := notesKey(req.GetScope(), req.GetOwnerId())
	if err != nil {
		return nil, notesErr(err)
	}
	if _, ok, err := s.priorCommand(ctx, req.GetCommandId()); err != nil {
		return nil, err
	} else if ok {
		return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: true}, nil
	}
	cleared, err := s.notes.Delete(ctx, c, key, req.GetReason())
	if err != nil {
		return nil, notesErr(err)
	}
	if err := s.recordNotesCommand(ctx, req.GetCommandId(), req.GetReason(), "delete_memory_notes", key.String(),
		cleared.Content, "deleted", "ok"); err != nil {
		return nil, err
	}
	return &pb.CommandAck{CommandId: req.GetCommandId()}, nil
}

// SetMemoryNotesBudget sets a document's budget, or the scope default (owner "").
// Lowering it truncates nothing stored.
func (s *Service) SetMemoryNotesBudget(ctx context.Context, req *pb.SetMemoryNotesBudgetOpRequest) (*pb.CommandAck, error) {
	c, err := s.notesCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireCommand(req.GetCommandId(), req.GetReason()); err != nil {
		return nil, err
	}
	scope, err := memorynotes.ParseScope(req.GetScope())
	if err != nil {
		return nil, notesErr(err)
	}
	if !req.GetClearOverride() && req.GetBudgetChars() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "budget_chars must be > 0")
	}
	if _, ok, err := s.priorCommand(ctx, req.GetCommandId()); err != nil {
		return nil, err
	} else if ok {
		return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: true}, nil
	}
	if err := s.notes.SetBudget(ctx, c, scope, req.GetOwnerId(), int(req.GetBudgetChars()), req.GetClearOverride()); err != nil {
		return nil, notesErr(err)
	}
	target := string(scope) + ":" + req.GetOwnerId()
	after := fmt.Sprintf("budget %d chars", req.GetBudgetChars())
	switch {
	case req.GetClearOverride():
		after = "budget override cleared"
	case req.GetOwnerId() == "":
		target = string(scope) + ":<scope default>"
	}
	if err := s.recordNotesCommand(ctx, req.GetCommandId(), req.GetReason(), "set_memory_notes_budget", target,
		"", after, "ok"); err != nil {
		return nil, err
	}
	return &pb.CommandAck{CommandId: req.GetCommandId()}, nil
}
