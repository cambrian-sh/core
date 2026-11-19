package operator

import (
	"context"
	"sort"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
)

// DerivationJournalSource is the derivation journal the operator plane reads and
// re-drives (ENG-2139; memory.DerivationJournaler over the ENG-2122 journal).
type DerivationJournalSource interface {
	Stats() memory.DerivationJournalStats
	Backlog(ctx context.Context) (map[domain.DerivationKind]domain.DerivationBacklog, error)
	DeadJobs(ctx context.Context, limit int) ([]domain.DeadDerivation, error)
	Requeue(ctx context.Context, k domain.DerivationKey) (bool, error)
}

// derivationDeadDefault bounds GetDerivationJournal's dead list when the request
// names no limit; derivationDeadMax caps it.
const (
	derivationDeadDefault = 100
	derivationDeadMax     = 1000
)

// GetDerivationJournal reports the derivation journal (contract 0113, capability
// "derivation-journal"): the backlog per kind read now, the counters, and the dead
// jobs with their last error. Identifiers and counts only — never document content.
func (s *Service) GetDerivationJournal(ctx context.Context, req *pb.GetDerivationJournalOpRequest) (*pb.GetDerivationJournalOpResponse, error) {
	if s.derivations == nil {
		return nil, status.Error(codes.Unimplemented, "the derivation journal is off on this kernel (execution.ingestion.derivation_journal_enabled)")
	}
	limit := int(req.GetDeadLimit())
	if limit <= 0 {
		limit = derivationDeadDefault
	}
	if limit > derivationDeadMax {
		limit = derivationDeadMax
	}
	backlog, err := s.derivations.Backlog(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "derivation journal backlog: %v", err)
	}
	dead, err := s.derivations.DeadJobs(ctx, limit)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "derivation journal dead jobs: %v", err)
	}
	resp := &pb.GetDerivationJournalOpResponse{}
	kinds := make([]string, 0, len(backlog))
	for k := range backlog {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		b := backlog[domain.DerivationKind(k)]
		op := &pb.DerivationBacklogOp{Kind: k, Pending: int32(b.Pending), Retrying: int32(b.Retrying), Dead: int32(b.Dead)}
		if !b.Oldest.IsZero() {
			op.OldestUnixMs = b.Oldest.UnixMilli()
		}
		resp.Kinds = append(resp.Kinds, op)
	}
	for _, d := range dead {
		op := &pb.DeadDerivationOp{
			Kind: string(d.Key.Kind), DocumentId: d.Key.DocumentID, ContentDigest: d.Key.ContentDigest,
			Attempts: int32(d.Attempts), LastError: d.LastError, ChunkCount: int32(d.ChunkCount),
		}
		if !d.CreatedAt.IsZero() {
			op.CreatedAtUnixMs = d.CreatedAt.UnixMilli()
		}
		if !d.DeadAt.IsZero() {
			op.DeadAtUnixMs = d.DeadAt.UnixMilli()
		}
		resp.Dead = append(resp.Dead, op)
	}
	st := s.derivations.Stats()
	resp.Counters = &pb.DerivationJournalCountersOp{
		Recorded: st.Recorded, RecordFailed: st.RecordFailed, Completed: st.Completed, Failed: st.Failed,
		Deferred: st.Deferred, Died: st.Died, Recovered: st.Recovered,
	}
	return resp, nil
}

// RequeueDerivationJob makes one dead job live again (contract 0113). Mutating:
// command_id + reason, audited. NotFound when no dead job of that version exists.
func (s *Service) RequeueDerivationJob(ctx context.Context, req *pb.RequeueDerivationJobOpRequest) (*pb.CommandAck, error) {
	if s.derivations == nil {
		return nil, status.Error(codes.Unimplemented, "the derivation journal is off on this kernel (execution.ingestion.derivation_journal_enabled)")
	}
	kind := domain.DerivationKind(req.GetKind())
	switch kind {
	case domain.DerivationFacts, domain.DerivationKGTriplets, domain.DerivationEmbed:
	default:
		return nil, status.Errorf(codes.InvalidArgument, "kind must be facts, kg_triplets or embed, got %q", req.GetKind())
	}
	if req.GetDocumentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "document_id is required")
	}
	key := domain.DerivationKey{Kind: kind, DocumentID: req.GetDocumentId(), ContentDigest: req.GetContentDigest()}
	target := string(kind) + ":" + key.DocumentID
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"requeue_derivation_job", "derivation_job", target, "requeued",
		func() error {
			ok, err := s.derivations.Requeue(ctx, key)
			if err != nil {
				return status.Errorf(codes.Unavailable, "requeue: %v", err)
			}
			if !ok {
				return status.Errorf(codes.NotFound, "no dead %s job for document %q at that version", kind, key.DocumentID)
			}
			return nil
		})
}
