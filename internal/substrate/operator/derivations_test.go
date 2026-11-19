package operator_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// ENG-2139: the derivation journal on the operator plane.

type fakeDerivations struct {
	backlog  map[domain.DerivationKind]domain.DerivationBacklog
	dead     []domain.DeadDerivation
	requeued []domain.DerivationKey
}

func (f *fakeDerivations) Stats() memory.DerivationJournalStats {
	return memory.DerivationJournalStats{Recorded: 7, Died: 1, Recovered: 2}
}
func (f *fakeDerivations) Backlog(context.Context) (map[domain.DerivationKind]domain.DerivationBacklog, error) {
	return f.backlog, nil
}
func (f *fakeDerivations) DeadJobs(_ context.Context, limit int) ([]domain.DeadDerivation, error) {
	if len(f.dead) > limit {
		return f.dead[:limit], nil
	}
	return f.dead, nil
}
func (f *fakeDerivations) Requeue(_ context.Context, k domain.DerivationKey) (bool, error) {
	for i, d := range f.dead {
		if d.Key == k {
			f.dead = append(f.dead[:i], f.dead[i+1:]...)
			f.requeued = append(f.requeued, k)
			return true, nil
		}
	}
	return false, nil
}

func TestGetDerivationJournal_BacklogCountersAndDeadJobs(t *testing.T) {
	svc, _, _, _ := newCommandService()
	oldest := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	key := domain.DerivationKey{Kind: domain.DerivationFacts, DocumentID: "doc-1", ContentDigest: "v1"}
	svc.SetDerivationJournal(&fakeDerivations{
		backlog: map[domain.DerivationKind]domain.DerivationBacklog{
			domain.DerivationKGTriplets: {Pending: 3, Retrying: 1, Oldest: oldest},
			domain.DerivationFacts:      {Dead: 1},
		},
		dead: []domain.DeadDerivation{{Key: key, Attempts: 5, LastError: "provider 503", ChunkCount: 4, DeadAt: oldest}},
	})
	resp, err := svc.GetDerivationJournal(opCtx(), &pb.GetDerivationJournalOpRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Kinds) != 2 || resp.Kinds[0].Kind != "facts" || resp.Kinds[1].Kind != "kg_triplets" ||
		resp.Kinds[1].Pending != 3 || resp.Kinds[1].Retrying != 1 || resp.Kinds[1].OldestUnixMs != oldest.UnixMilli() ||
		resp.Kinds[0].Dead != 1 || resp.Kinds[0].OldestUnixMs != 0 {
		t.Fatalf("kinds = %+v", resp.Kinds)
	}
	if len(resp.Dead) != 1 || resp.Dead[0].DocumentId != "doc-1" || resp.Dead[0].LastError != "provider 503" ||
		resp.Dead[0].Attempts != 5 || resp.Dead[0].ChunkCount != 4 || resp.Dead[0].ContentDigest != "v1" {
		t.Fatalf("dead = %+v", resp.Dead)
	}
	if resp.Counters.GetRecorded() != 7 || resp.Counters.GetDied() != 1 || resp.Counters.GetRecovered() != 2 {
		t.Fatalf("counters = %+v", resp.Counters)
	}
}

func TestRequeueDerivationJob_AuditedNotFoundAndValidated(t *testing.T) {
	svc, _, audit, _ := newCommandService()
	key := domain.DerivationKey{Kind: domain.DerivationKGTriplets, DocumentID: "doc-2", ContentDigest: "v3"}
	src := &fakeDerivations{dead: []domain.DeadDerivation{{Key: key}}}
	svc.SetDerivationJournal(src)
	req := &pb.RequeueDerivationJobOpRequest{CommandId: "c1", Reason: "provider back", Kind: "kg_triplets", DocumentId: "doc-2", ContentDigest: "v3"}
	if _, err := svc.RequeueDerivationJob(opCtx(), req); err != nil {
		t.Fatal(err)
	}
	if len(src.requeued) != 1 || src.requeued[0] != key {
		t.Fatalf("requeued %v", src.requeued)
	}
	rows, _ := audit.Query(context.Background(), operator.AuditFilter{CommandID: "c1"})
	if len(rows) != 1 || rows[0].ActionType != "requeue_derivation_job" {
		t.Fatalf("audit: %+v", rows)
	}
	req.CommandId = "c2"
	if _, err := svc.RequeueDerivationJob(opCtx(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("second requeue: %v, want NotFound", err)
	}
	if _, err := svc.RequeueDerivationJob(opCtx(), &pb.RequeueDerivationJobOpRequest{CommandId: "c3", Reason: "r", Kind: "nope", DocumentId: "d"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad kind: %v", err)
	}
	if _, err := svc.RequeueDerivationJob(opCtx(), &pb.RequeueDerivationJobOpRequest{CommandId: "c4", Kind: "facts", DocumentId: "d"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing reason: %v", err)
	}
}

func TestDerivationJournalRPCs_UnimplementedWhenOff(t *testing.T) {
	svc, _, _, _ := newCommandService()
	if _, err := svc.GetDerivationJournal(opCtx(), &pb.GetDerivationJournalOpRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("get: %v", err)
	}
	if _, err := svc.RequeueDerivationJob(opCtx(), &pb.RequeueDerivationJobOpRequest{CommandId: "c", Reason: "r", Kind: "facts", DocumentId: "d"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("requeue: %v", err)
	}
}
