package operator

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// perPrincipalPolicy answers by principal id, so a test can assert that answers
// land on the RIGHT CELLS rather than merely that the right number came back.
// Distinct from stubPolicy in access_policy_test.go, which echoes one scripted
// decision and cannot tell cells apart.
type perPrincipalPolicy struct {
	allow map[string]bool
	calls int
}

func (s *perPrincipalPolicy) ExplainAccess(_ context.Context, req domain.AccessRequest) domain.AccessDecision {
	s.calls++
	return domain.AccessDecision{
		Allowed: s.allow[req.Principal.ID],
		Reason:  domain.ReasonAllowed,
	}
}
func (s *perPrincipalPolicy) Vocabulary(context.Context) []string { return nil }
func (s *perPrincipalPolicy) SetAgentScope(context.Context, string, []string, []string, []string) error {
	return nil
}
func (s *perPrincipalPolicy) SetAgentWriteTags(context.Context, string, []string) error { return nil }
func (s *perPrincipalPolicy) ValidateTag(string) bool                                   { return true }

func batchService(p domain.PolicyAdmin) *Service {
	s := &Service{}
	s.SetPolicyAdmin(p)
	return s
}

// Answers are POSITIONAL. A dropped or reordered cell puts the wrong answer under
// the wrong row, and a matrix that is silently off by one is worse than one that
// refuses to render.
func TestExplainAccessBatch_AnswersArePositional(t *testing.T) {
	p := &perPrincipalPolicy{allow: map[string]bool{"alice": true, "bob": false, "carol": true}}
	s := batchService(p)

	resp, err := s.ExplainAccessBatch(context.Background(), &pb.ExplainAccessBatchOpRequest{
		Queries: []*pb.ExplainAccessOpRequest{
			{PrincipalId: "alice"},
			{PrincipalId: "bob"},
			{PrincipalId: "carol"},
		},
	})
	if err != nil {
		t.Fatalf("ExplainAccessBatch: %v", err)
	}
	if len(resp.GetDecisions()) != 3 {
		t.Fatalf("got %d decisions, want 3", len(resp.GetDecisions()))
	}
	want := []bool{true, false, true}
	for i, w := range want {
		if resp.GetDecisions()[i].GetAllowed() != w {
			t.Fatalf("decision[%d].allowed = %v, want %v — answers are off their cells",
				i, resp.GetDecisions()[i].GetAllowed(), w)
		}
	}
}

// A malformed cell must occupy its slot with a DENIAL, not be skipped and not
// fail the batch. Skipping shifts every later answer; failing lets one bad cell
// blank an otherwise correct grid.
func TestExplainAccessBatch_MalformedCellDeniesInPlace(t *testing.T) {
	p := &perPrincipalPolicy{allow: map[string]bool{"alice": true, "carol": true}}
	s := batchService(p)

	resp, err := s.ExplainAccessBatch(context.Background(), &pb.ExplainAccessBatchOpRequest{
		Queries: []*pb.ExplainAccessOpRequest{
			{PrincipalId: "alice"},
			{PrincipalId: ""}, // malformed
			{PrincipalId: "carol"},
		},
	})
	if err != nil {
		t.Fatalf("one malformed cell failed the whole batch: %v", err)
	}
	if len(resp.GetDecisions()) != 3 {
		t.Fatalf("got %d decisions, want 3 — the malformed cell must hold its slot", len(resp.GetDecisions()))
	}
	bad := resp.GetDecisions()[1]
	if bad.GetAllowed() {
		t.Fatal("a malformed cell reported ALLOWED — denial is the only safe direction here")
	}
	if bad.GetExplain() == "" {
		t.Fatal("a denial with no explanation is not actionable")
	}
	// The cells around it are still correct and still in place.
	if !resp.GetDecisions()[0].GetAllowed() || !resp.GetDecisions()[2].GetAllowed() {
		t.Fatal("neighbouring cells were disturbed by the malformed one")
	}
}

// An unknown effect class is a query error, not a policy answer — and it must
// deny rather than fall through to an evaluation that ignores it.
func TestExplainAccessBatch_UnknownEffectDenies(t *testing.T) {
	p := &perPrincipalPolicy{allow: map[string]bool{"alice": true}}
	s := batchService(p)

	resp, _ := s.ExplainAccessBatch(context.Background(), &pb.ExplainAccessBatchOpRequest{
		Queries: []*pb.ExplainAccessOpRequest{{PrincipalId: "alice", Effects: []string{"teleport"}}},
	})
	d := resp.GetDecisions()[0]
	if d.GetAllowed() {
		t.Fatal("an unknown effect class was evaluated as if it were absent")
	}
	if p.calls != 0 {
		t.Fatal("the decision point was consulted with an invalid effect")
	}
}

// Over-large batches are truncated and SAY SO, so a console cannot draw a partial
// grid as though it were complete.
func TestExplainAccessBatch_TruncatesAndReportsIt(t *testing.T) {
	p := &perPrincipalPolicy{allow: map[string]bool{}}
	s := batchService(p)

	queries := make([]*pb.ExplainAccessOpRequest, MaxExplainBatch+10)
	for i := range queries {
		queries[i] = &pb.ExplainAccessOpRequest{PrincipalId: "a"}
	}
	resp, err := s.ExplainAccessBatch(context.Background(), &pb.ExplainAccessBatchOpRequest{Queries: queries})
	if err != nil {
		t.Fatalf("ExplainAccessBatch: %v", err)
	}
	if len(resp.GetDecisions()) != MaxExplainBatch {
		t.Fatalf("got %d decisions, want %d", len(resp.GetDecisions()), MaxExplainBatch)
	}
	if !resp.GetTruncated() {
		t.Fatal("truncated = false on a truncated batch — the console would draw a partial grid as complete")
	}
	if resp.GetLimit() != MaxExplainBatch {
		t.Fatalf("limit = %d, want %d so a client can page deliberately", resp.GetLimit(), MaxExplainBatch)
	}
}

func TestExplainAccessBatch_UnconfiguredAndEmpty(t *testing.T) {
	if _, err := (&Service{}).ExplainAccessBatch(context.Background(), &pb.ExplainAccessBatchOpRequest{
		Queries: []*pb.ExplainAccessOpRequest{{PrincipalId: "a"}},
	}); codeOf(err) != codes.Unimplemented {
		t.Fatalf("unconfigured: code = %v, want Unimplemented", codeOf(err))
	}

	s := batchService(&perPrincipalPolicy{})
	if _, err := s.ExplainAccessBatch(context.Background(), &pb.ExplainAccessBatchOpRequest{}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("empty: code = %v, want InvalidArgument", codeOf(err))
	}
}
