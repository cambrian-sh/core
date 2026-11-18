package operator

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"

	pb "github.com/cambrian-sh/core/api/proto"
)

// stubLinkWriter records the pair-level calls; the row-level methods are not
// exercised here.
type stubLinkWriter struct {
	LinkWriter
	pairs     []LinkPairRecord
	confirmed []LinkPairKey
	rejected  []LinkPairKey
}

func (s *stubLinkWriter) ListLinkPairs(context.Context, string, int) ([]LinkPairRecord, error) {
	return s.pairs, nil
}

func (s *stubLinkWriter) ConfirmLinkPair(_ context.Context, k LinkPairKey, actor string) ([]LinkRecord, error) {
	s.confirmed = append(s.confirmed, k)
	return []LinkRecord{{ID: "c1", SourceRef: "confirm:a", AssertedBy: actor}, {ID: "c2", SourceRef: "confirm:b"}}, nil
}

func (s *stubLinkWriter) RejectLinkPair(_ context.Context, k LinkPairKey, _, _ string) (int, error) {
	s.rejected = append(s.rejected, k)
	return 2, nil
}

func pairReq() *pb.LinkPairKeyOp {
	return &pb.LinkPairKeyOp{Family: "identity", FromRef: "entity:a/1", ToRef: "entity:b/2", Relation: "same_as"}
}

// Contract 0109: the pair RPCs answer Unimplemented without a review lane, like
// the row-level ones.
func TestLinkPairs_UnwiredReturnsUnimplemented(t *testing.T) {
	t.Parallel()
	s := &Service{}
	ctx := context.Background()
	if _, err := s.ListLinkPairs(ctx, &pb.ListLinkPairsOpRequest{}); codeOf(err) != codes.Unimplemented {
		t.Errorf("ListLinkPairs: %v", codeOf(err))
	}
	if _, err := s.ConfirmLinkPair(ctx, &pb.ConfirmLinkPairOpRequest{CommandId: "c", Reason: "r", Pair: pairReq()}); codeOf(err) != codes.Unimplemented {
		t.Errorf("ConfirmLinkPair: %v", codeOf(err))
	}
	if _, err := s.RejectLinkPair(ctx, &pb.RejectLinkPairOpRequest{CommandId: "c", Reason: "r", Pair: pairReq()}); codeOf(err) != codes.Unimplemented {
		t.Errorf("RejectLinkPair: %v", codeOf(err))
	}
}

// A pair that names no claim is refused before anything reaches the writer.
func TestLinkPairs_IncompletePairIsInvalid(t *testing.T) {
	t.Parallel()
	w := &stubLinkWriter{}
	s := newWriteService(nil, nil)
	s.SetLinkWriter(w)
	ctx := context.Background()
	bad := &pb.LinkPairKeyOp{Family: "identity", FromRef: "entity:a/1"}
	if _, err := s.ConfirmLinkPair(ctx, &pb.ConfirmLinkPairOpRequest{CommandId: "c", Reason: "r", Pair: bad}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("ConfirmLinkPair: %v, want InvalidArgument", codeOf(err))
	}
	if _, err := s.RejectLinkPair(ctx, &pb.RejectLinkPairOpRequest{CommandId: "c", Reason: "r"}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("RejectLinkPair: %v, want InvalidArgument", codeOf(err))
	}
	if len(w.confirmed)+len(w.rejected) != 0 {
		t.Fatal("an incomplete pair reached the writer")
	}
}

// The pair RPCs map the writer's answer onto the wire: every pair once with its
// rows and flag, every confirmation of a pair confirm, the retracted count.
func TestLinkPairs_MapsTheWriter(t *testing.T) {
	t.Parallel()
	w := &stubLinkWriter{pairs: []LinkPairRecord{{
		Key:        LinkPairKey{NamespaceID: "default", Family: "identity", FromRef: "entity:a/1", ToRef: "entity:b/2", Relation: "same_as"},
		Confidence: 0.9,
		Candidates: []LinkRecord{
			{ID: "l1", SupportingEvidenceIDs: []string{"ev_2"}},
			{ID: "l2"},
		},
		Confirmations:    []LinkRecord{{ID: "k1"}},
		AlreadyConfirmed: true,
	}}}
	s := newWriteService(nil, nil)
	s.SetLinkWriter(w)
	ctx := context.Background()

	list, err := s.ListLinkPairs(ctx, &pb.ListLinkPairsOpRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetPairs()) != 1 {
		t.Fatalf("pairs = %d", len(list.GetPairs()))
	}
	p := list.GetPairs()[0]
	if p.GetPair().GetFromRef() != "entity:a/1" || len(p.GetCandidates()) != 2 || len(p.GetConfirmations()) != 1 ||
		!p.GetAlreadyConfirmed() || p.GetConfidence() != 0.9 {
		t.Fatalf("pair = %v", p)
	}
	if got := p.GetCandidates()[0].GetSupportingEvidenceIds(); len(got) != 1 || got[0] != "ev_2" {
		t.Fatalf("supporting_evidence_ids = %v", got)
	}

	conf, err := s.ConfirmLinkPair(ctx, &pb.ConfirmLinkPairOpRequest{CommandId: "cmd-1", Reason: "same person", Pair: pairReq()})
	if err != nil {
		t.Fatal(err)
	}
	if len(conf.GetConfirmations()) != 2 || len(w.confirmed) != 1 || w.confirmed[0].Relation != "same_as" {
		t.Fatalf("confirm = %v, writer saw %v", conf, w.confirmed)
	}
	// Idempotent on command_id: the replay applies nothing and returns no rows.
	again, err := s.ConfirmLinkPair(ctx, &pb.ConfirmLinkPairOpRequest{CommandId: "cmd-1", Reason: "same person", Pair: pairReq()})
	if err != nil || !again.GetDeduped() || len(w.confirmed) != 1 {
		t.Fatalf("replay: %v %v, writer calls %d", again, err, len(w.confirmed))
	}

	rej, err := s.RejectLinkPair(ctx, &pb.RejectLinkPairOpRequest{CommandId: "cmd-2", Reason: "different people", Pair: pairReq()})
	if err != nil || rej.GetRetracted() != 2 || len(w.rejected) != 1 {
		t.Fatalf("reject = %v %v", rej, err)
	}
}
