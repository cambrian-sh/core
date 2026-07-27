package network

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// notePolicy scripts a ReadFilter answer so the annotation logic can be exercised
// across the shapes a real decision point produces.
type notePolicy struct {
	domain.AllowAllAuthorizer
	pred *domain.TagPredicate
	dec  domain.AccessDecision
}

func (n notePolicy) ReadFilter(context.Context, domain.PrincipalRef, domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	return n.pred, n.dec
}

// §1.4 is the failure this exists to prevent: an unrecognised principal gets zero
// results and no error, ingest succeeds, queries return nothing, and nothing
// anywhere reports a problem. The note is what breaks that silence.
func TestPolicyNote_UnresolvedPrincipalIsStatedOutLoud(t *testing.T) {
	s := &Server{Authz: notePolicy{
		pred: nil,
		dec: domain.AccessDecision{
			Principal: domain.AgentPrincipal("ghost"),
			Reason:    domain.ReasonNoPrincipal,
			Detail:    "unknown principal agent:ghost",
		},
	}}
	note := s.policyNote(context.Background(), "ghost", 0)
	if note == "" {
		t.Fatalf("an empty result caused by an unresolved principal must carry a reason (INV-3)")
	}
	if !strings.Contains(note, string(domain.ReasonNoPrincipal)) {
		t.Errorf("the note must name the reason, got %q", note)
	}
}

// The zombie boundary: a predicate that can never match anything is a SAFE state
// and therefore the easiest one to mistake for "there is no data".
func TestPolicyNote_UnsatisfiablePredicateIsStated(t *testing.T) {
	s := &Server{Authz: notePolicy{
		pred: &domain.TagPredicate{RequiredTags: []string{"secrets"}, ForbiddenTags: []string{"secrets"}},
		dec: domain.AccessDecision{
			Allowed: true, Reason: domain.ReasonUnsatisfiablePolicy, Detail: "Required∩Forbidden={secrets}",
		},
	}}
	note := s.policyNote(context.Background(), "a", 0)
	if !strings.Contains(note, "secrets") {
		t.Errorf("the note must name the contradiction, got %q", note)
	}
}

// A real boundary that simply matched nothing still says a boundary was in play —
// "no results here" and "no results anywhere" are different answers.
func TestPolicyNote_RestrictedButEmptyMentionsTheBoundary(t *testing.T) {
	s := &Server{Authz: notePolicy{
		pred: &domain.TagPredicate{ForbiddenTags: []string{"secrets"}},
		dec:  domain.AccessDecision{Allowed: true, Reason: domain.ReasonAllowed},
	}}
	if note := s.policyNote(context.Background(), "a", 0); note == "" {
		t.Errorf("a restricted read that returned nothing must say a boundary applied")
	}
}

// Silence in the two cases where policy played no part: otherwise callers learn
// to ignore the field, and it stops being an alarm.
func TestPolicyNote_QuietWhenPolicyPlayedNoPart(t *testing.T) {
	unrestricted := &Server{Authz: notePolicy{
		pred: &domain.TagPredicate{},
		dec:  domain.AccessDecision{Allowed: true, Reason: domain.ReasonAllowed},
	}}
	if note := unrestricted.policyNote(context.Background(), "a", 0); note != "" {
		t.Errorf("an unrestricted read over an empty corpus needs no note, got %q", note)
	}
	bypass := &Server{Authz: notePolicy{
		pred: domain.ScopeSystem,
		dec:  domain.AccessDecision{Allowed: true, Reason: domain.ReasonBypass},
	}}
	if note := bypass.policyNote(context.Background(), "a", 0); note != "" {
		t.Errorf("a bypass read needs no note, got %q", note)
	}
	// Results came back: nothing to explain.
	restricted := &Server{Authz: notePolicy{
		pred: &domain.TagPredicate{ForbiddenTags: []string{"secrets"}},
		dec:  domain.AccessDecision{Allowed: true, Reason: domain.ReasonAllowed},
	}}
	if note := restricted.policyNote(context.Background(), "a", 3); note != "" {
		t.Errorf("a non-empty result needs no note, got %q", note)
	}
}

// With no decision point installed there is no policy to blame, so the OSS build
// never annotates — an empty corpus is just an empty corpus.
func TestPolicyNote_SilentInOSS(t *testing.T) {
	s := &Server{}
	if note := s.policyNote(context.Background(), "a", 0); note != "" {
		t.Errorf("an unscoped deployment must not invent a policy explanation, got %q", note)
	}
}
