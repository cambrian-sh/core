package operator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

type fakeFacts struct {
	gotFilter domain.FactListFilter
	scopes    []*domain.TagPredicate
	byID      map[string]domain.Fact
	chain     []domain.Fact
	listErr   error
}

func (f *fakeFacts) ListFacts(_ context.Context, fl domain.FactListFilter, s *domain.TagPredicate) ([]domain.Fact, string, error) {
	f.gotFilter, f.scopes = fl, append(f.scopes, s)
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	from := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	return []domain.Fact{{ID: "ki_fact:a", Subject: "user", Attribute: "pet", Value: "cat", ValidFrom: &from,
		Current: true, SupersededBy: "x", Polarity: domain.FactPositive, Tags: []string{"public"}}}, "cursor-2", nil
}
func (f *fakeFacts) FactsByID(_ context.Context, ids []domain.KnowledgeItemID, s *domain.TagPredicate) ([]domain.Fact, error) {
	f.scopes = append(f.scopes, s)
	if fact, ok := f.byID[string(ids[0])]; ok {
		return []domain.Fact{fact}, nil
	}
	return nil, nil
}
func (f *fakeFacts) FactChain(_ context.Context, _ domain.Fact, s *domain.TagPredicate) ([]domain.Fact, error) {
	f.scopes = append(f.scopes, s)
	return f.chain, nil
}
func (f *fakeFacts) FactCoverage(context.Context) (domain.FactCoverage, error) {
	at := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	return domain.FactCoverage{DocumentsWithChunks: 10, DocumentsExtracted: 4, FactItems: 33, LastExtractedAt: at,
		Versions: []domain.FactExtractorVersionCoverage{{Version: "p/v1", DocumentsComplete: 3, DocumentsPartial: 1, LastExtractedAt: at}}}, nil
}

// Contract 0103 (T-20261103-operator-plane-cannot-inspect-facts-no-rpc).
func TestFacts_UnwiredAnswersUnimplemented(t *testing.T) {
	s := &Service{}
	if _, err := s.ListFacts(context.Background(), &pb.ListFactsOpRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("ListFacts unwired: %v", err)
	}
	if _, err := s.GetFactCoverage(context.Background(), &pb.GetFactCoverageOpRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("GetFactCoverage unwired: %v", err)
	}
}

func TestFacts_ListUsesSystemScopeAndRendersStatedBounds(t *testing.T) {
	ff := &fakeFacts{}
	s := &Service{}
	s.SetFactInspector(ff)
	out, err := s.ListFacts(context.Background(), &pb.ListFactsOpRequest{Subject: "User", Attribute: "pet", Actor: "user:a", Limit: 5, PageToken: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if ff.gotFilter != (domain.FactListFilter{Subject: "User", Attribute: "pet", Actor: "user:a", Limit: 5, After: "c1"}) {
		t.Fatalf("filter = %+v", ff.gotFilter)
	}
	if ff.scopes[0] != domain.ScopeSystem {
		t.Fatal("operator fact reads run at domain.ScopeSystem (owner fence kept)")
	}
	if out.GetNextPageToken() != "cursor-2" || len(out.GetFacts()) != 1 {
		t.Fatalf("out = %+v", out)
	}
	f := out.GetFacts()[0]
	if f.GetValidFrom() != "2023-01-01T00:00:00Z" || f.GetValidTo() != "" || f.GetPolarity() != "positive" || f.GetTags()[0] != "public" {
		t.Fatalf("fact = %+v", f)
	}
	if f.GetCurrent() || f.GetSupersededBy() != "" {
		t.Fatal("ListFacts renders stated bounds: current/superseded_by must stay unset")
	}
}

func TestFacts_ListMapsCursorErrorToInvalidArgument(t *testing.T) {
	s := &Service{}
	s.SetFactInspector(&fakeFacts{listErr: fmt.Errorf("wrap: %w", domain.ErrFactCursor)})
	if _, err := s.ListFacts(context.Background(), &pb.ListFactsOpRequest{PageToken: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad cursor: %v", err)
	}
	s.SetFactInspector(&fakeFacts{listErr: fmt.Errorf("db down")})
	if _, err := s.ListFacts(context.Background(), &pb.ListFactsOpRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("store error: %v", err)
	}
}

func TestFacts_ChainIsDerivedAndAbsentFactReadsEmpty(t *testing.T) {
	ff := &fakeFacts{byID: map[string]domain.Fact{"ki_fact:a": {ID: "ki_fact:a"}},
		chain: []domain.Fact{{ID: "ki_fact:a", SupersededBy: "ki_fact:b"}, {ID: "ki_fact:b", Current: true}}}
	s := &Service{}
	s.SetFactInspector(ff)
	if _, err := s.GetFactChain(context.Background(), &pb.GetFactChainOpRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing id: %v", err)
	}
	out, err := s.GetFactChain(context.Background(), &pb.GetFactChainOpRequest{FactId: "ki_fact:a"})
	if err != nil || len(out.GetChain()) != 2 || out.GetChain()[0].GetSupersededBy() != "ki_fact:b" || !out.GetChain()[1].GetCurrent() {
		t.Fatalf("chain = %+v err %v", out, err)
	}
	for _, sc := range ff.scopes {
		if sc != domain.ScopeSystem {
			t.Fatal("every chain read runs at ScopeSystem")
		}
	}
	// A fact the reader may not see answers exactly as an absent one.
	none, err := s.GetFactChain(context.Background(), &pb.GetFactChainOpRequest{FactId: "ki_fact:hidden"})
	if err != nil || len(none.GetChain()) != 0 {
		t.Fatalf("absent fact: %+v %v", none, err)
	}
}

func TestFacts_Coverage(t *testing.T) {
	s := &Service{}
	s.SetFactInspector(&fakeFacts{})
	out, err := s.GetFactCoverage(context.Background(), &pb.GetFactCoverageOpRequest{})
	if err != nil || out.GetDocumentsWithChunks() != 10 || out.GetDocumentsExtracted() != 4 || out.GetFactItems() != 33 ||
		out.GetLastExtractedAt() != "2026-11-14T00:00:00Z" || len(out.GetVersions()) != 1 || out.GetVersions()[0].GetDocumentsPartial() != 1 {
		t.Fatalf("coverage = %+v err %v", out, err)
	}
}

// The three RPCs are reads by name: Operators and Viewers, never Members.
func TestFacts_RoleGate(t *testing.T) {
	for _, m := range []string{"ListFacts", "GetFactChain", "GetFactCoverage"} {
		if isOperatorOnly(operatorMethodPrefix+m, nil) {
			t.Errorf("%s must be a read open to Viewers", m)
		}
		if memberMayCall(operatorMethodPrefix+m, nil) {
			t.Errorf("%s must not be reachable by a Member", m)
		}
	}
}
