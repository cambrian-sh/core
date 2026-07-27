package operator

import (
	"context"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubPolicy is a PolicyAdmin that echoes a scripted decision, so these tests
// exercise the RPC shell and the projection rather than any particular policy.
type stubPolicy struct {
	dec   domain.AccessDecision
	seen  domain.AccessRequest
	vocab []string
}

func (s *stubPolicy) SetAgentScope(context.Context, string, []string, []string, []string) error {
	return nil
}
func (s *stubPolicy) SetAgentWriteTags(context.Context, string, []string) error { return nil }
func (s *stubPolicy) Vocabulary(context.Context) []string                       { return s.vocab }
func (s *stubPolicy) ValidateTag(string) bool                                   { return true }
func (s *stubPolicy) ExplainAccess(_ context.Context, req domain.AccessRequest) domain.AccessDecision {
	s.seen = req
	return s.dec
}

// An OSS build has no policy to explain, and says so rather than answering a
// question it cannot answer.
func TestExplainAccess_UnimplementedWithoutPolicyPlugin(t *testing.T) {
	s := NewService(nil)
	_, err := s.ExplainAccess(context.Background(), &pb.ExplainAccessOpRequest{PrincipalId: "a"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented on an unscoped deployment, got %v", err)
	}
	_, err = s.ListClassificationTags(context.Background(), &pb.ListClassificationTagsOpRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented for the vocabulary listing, got %v", err)
	}
}

func TestExplainAccess_RequiresAPrincipal(t *testing.T) {
	s := NewService(nil)
	s.SetPolicyAdmin(&stubPolicy{})
	if _, err := s.ExplainAccess(context.Background(), &pb.ExplainAccessOpRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a question about nobody is not a question; got %v", err)
	}
}

// An effect class outside the closed set is refused at the boundary rather than
// silently ignored — silently ignoring it would answer a DIFFERENT question than
// the one the administrator asked.
func TestExplainAccess_RejectsUnknownEffectClass(t *testing.T) {
	s := NewService(nil)
	s.SetPolicyAdmin(&stubPolicy{})
	_, err := s.ExplainAccess(context.Background(), &pb.ExplainAccessOpRequest{
		PrincipalId: "a", Effects: []string{"delete"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for an off-vocabulary effect, got %v", err)
	}
}

// The denial names the specific tag AND the policy that contributed it, which is
// the whole difference between an explanation and a shrug (ADR-0085 D8).
func TestExplainAccess_NamesTheTagAndTheContributingPolicy(t *testing.T) {
	stub := &stubPolicy{dec: domain.AccessDecision{
		Allowed:   false,
		Reason:    domain.ReasonForbiddenTag,
		Detail:    "internal_only",
		Resource:  domain.ResourceRef{Kind: domain.KindMemory, ID: "doc-9"},
		Principal: domain.AgentPrincipal("support"),
		Surface:   domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "public"},
		DecidedBy: []domain.PolicyContribution{{
			PolicyID: "p-outsider", PolicyName: "Outsider clamp", LinkedAt: "surface:public",
			Term: "forbidden", Values: []string{"internal_only"}, Enforced: true,
		}},
		PolicyVersion: "v7",
	}}
	s := NewService(nil)
	s.SetPolicyAdmin(stub)

	resp, err := s.ExplainAccess(context.Background(), &pb.ExplainAccessOpRequest{
		PrincipalId: "support", SurfaceKind: "chat", SurfaceId: "public",
		ResourceKind: "memory", ResourceId: "doc-9", Tags: []string{"internal_only"},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := resp.GetDecision()
	if d.GetAllowed() {
		t.Fatalf("expected a denial")
	}
	if d.GetReason() != string(domain.ReasonForbiddenTag) || d.GetDetail() != "internal_only" {
		t.Errorf("reason/detail = %q/%q, want forbidden_tag/internal_only", d.GetReason(), d.GetDetail())
	}
	if d.GetPolicyVersion() != "v7" {
		t.Errorf("the decision must be reproducible against a named policy version, got %q", d.GetPolicyVersion())
	}
	if len(d.GetDecidedBy()) != 1 {
		t.Fatalf("expected one contribution, got %d", len(d.GetDecidedBy()))
	}
	c := d.GetDecidedBy()[0]
	if c.GetPolicyName() != "Outsider clamp" || c.GetLinkedAt() != "surface:public" || !c.GetEnforced() {
		t.Errorf("contribution must name the policy, the link, and the Enforced flag: %+v", c)
	}
	if !strings.Contains(d.GetExplain(), "internal_only") {
		t.Errorf("the rendered sentence must name the responsible tag, got %q", d.GetExplain())
	}

	// The request must reach the decision point intact, including the surface —
	// a question asked about the wrong surface is answered about the wrong surface.
	if stub.seen.Surface.Kind != "chat" || stub.seen.Surface.ID != "public" {
		t.Errorf("surface not threaded to the decision point: %+v", stub.seen.Surface)
	}
	if stub.seen.Principal.Kind != domain.PrincipalAgent {
		t.Errorf("principal kind should default to agent, got %q", stub.seen.Principal.Kind)
	}
}

func TestListClassificationTags_ReturnsTheVocabulary(t *testing.T) {
	s := NewService(nil)
	s.SetPolicyAdmin(&stubPolicy{vocab: []string{"PII", "internal_only", "public"}})
	resp, err := s.ListClassificationTags(context.Background(), &pb.ListClassificationTagsOpRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetTags()) != 3 {
		t.Errorf("expected the full vocabulary so a UI can offer selection, got %v", resp.GetTags())
	}
}

// policyNote must stay quiet when policy played no part — an annotation on every
// response trains operators to ignore the field, which defeats INV-3.
func TestPolicyNote_OnlyWhenPolicyShapedTheOutcome(t *testing.T) {
	cases := []struct {
		name string
		dec  domain.AccessDecision
		want bool
	}{
		{"plain allow", domain.AccessDecision{Allowed: true, Reason: domain.ReasonAllowed}, false},
		{"bypass", domain.AccessDecision{Allowed: true, Reason: domain.ReasonBypass}, false},
		{"denial", domain.AccessDecision{Allowed: false, Reason: domain.ReasonForbiddenTag, Detail: "secrets"}, true},
		{"unsatisfiable", domain.AccessDecision{Allowed: true, Reason: domain.ReasonUnsatisfiablePolicy, Detail: "Required∩Forbidden={x}"}, true},
		{"report-only would-have-denied", domain.AccessDecision{Allowed: true, ReportOnly: true, WouldHaveDenied: true, Reason: domain.ReasonForbiddenTag}, true},
		{"no principal", domain.AccessDecision{Allowed: false, Reason: domain.ReasonNoPrincipal}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := policyNote(tc.dec) != nil
			if got != tc.want {
				t.Errorf("policyNote present = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every DecisionReason must survive the trip to the wire; a reason that projects
// to the empty string is a reason an operator can never act on.
func TestAccessDecisionToOp_CarriesEveryReason(t *testing.T) {
	reasons := []domain.DecisionReason{
		domain.ReasonAllowed, domain.ReasonBypass, domain.ReasonForbiddenTag,
		domain.ReasonMissingRequiredTag, domain.ReasonAnyOfUnsatisfied,
		domain.ReasonEffectNotPermitted, domain.ReasonUnsatisfiablePolicy,
		domain.ReasonNoPrincipal, domain.ReasonSkillGrantClipped, domain.ReasonNotAuthorized,
	}
	for _, r := range reasons {
		op := AccessDecisionToOp(domain.AccessDecision{Reason: r, Detail: "d"})
		if op.GetReason() != string(r) {
			t.Errorf("reason %q did not survive projection, got %q", r, op.GetReason())
		}
		if op.GetExplain() == "" {
			t.Errorf("reason %q produced no readable sentence", r)
		}
	}
}
