package domain

import "testing"

// The kernel's job is to APPLY a predicate, so these tests are all about
// application: precedence, the CNF semantics, the fail-closed nil, and the
// bypass sentinel. Composition (intersection, precedence between policies) is
// tested in the policy plugin, because that is where it lives.

func TestTagPredicate_PrecedenceAndCNF(t *testing.T) {
	// The ADR-0034 D12 worked example, expressed as an already-computed predicate:
	// required{customer_789}, anyOf{published} AND anyOf{support}, forbidden{secrets,internal_only}.
	p := &TagPredicate{
		RequiredTags:  []string{"customer_789"},
		AnyOfClauses:  [][]string{{"published"}, {"support"}},
		ForbiddenTags: []string{"secrets", "internal_only"},
	}

	cases := []struct {
		name       string
		tags       []string
		want       bool
		wantReason DecisionReason
		wantDetail string
	}{
		{"forbidden disqualifies even when everything else matches",
			[]string{"customer_789", "published", "support", "internal_only"}, false, ReasonForbiddenTag, "internal_only"},
		{"missing required tag",
			[]string{"published", "support"}, false, ReasonMissingRequiredTag, "customer_789"},
		{"one clause unsatisfied",
			[]string{"customer_789", "support"}, false, ReasonAnyOfUnsatisfied, "published"},
		{"all clauses satisfied",
			[]string{"customer_789", "published", "support"}, true, ReasonAllowed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, ok := p.Check(tc.tags)
			if ok != tc.want {
				t.Fatalf("Check(%v) = %v, want %v", tc.tags, ok, tc.want)
			}
			if p.Allows(tc.tags) != tc.want {
				t.Fatalf("Allows disagrees with Check for %v", tc.tags)
			}
			if dec.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", dec.Reason, tc.wantReason)
			}
			// INV-3: a denial must name the specific term responsible, or an
			// administrator cannot act on it.
			if tc.wantDetail != "" && dec.Detail != tc.wantDetail {
				t.Errorf("Detail = %q, want %q", dec.Detail, tc.wantDetail)
			}
		})
	}
}

func TestTagPredicate_NilIsFailClosed(t *testing.T) {
	var p *TagPredicate
	if p.Allows(nil) || p.Allows([]string{"anything"}) {
		t.Fatalf("a nil predicate must allow nothing (fail-closed backstop)")
	}
	dec, ok := p.Check([]string{"anything"})
	if ok || dec.Reason != ReasonNoPrincipal {
		t.Fatalf("nil predicate should deny with %q, got %v/%v", ReasonNoPrincipal, ok, dec.Reason)
	}
	// Forbids on a nil receiver is a query about data, not a decision: it forbids
	// nothing, and the chokepoint (not this method) is what fails closed.
	if p.Forbids("secrets") {
		t.Errorf("nil predicate must not claim to forbid anything")
	}
}

func TestScopeSystem_BypassesEverything(t *testing.T) {
	if !ScopeSystem.Bypass {
		t.Fatalf("ScopeSystem.Bypass must be true — it is the greppable kernel-internal sentinel (INV-7)")
	}
	if !ScopeSystem.Allows([]string{"secrets", "internal_only", "PII"}) {
		t.Fatalf("ScopeSystem must admit every row")
	}
	dec, _ := ScopeSystem.Check([]string{"secrets"})
	if dec.Reason != ReasonBypass {
		t.Errorf("a bypass read must be journalled as %q, not silently allowed, got %q", ReasonBypass, dec.Reason)
	}
	if bad, _ := ScopeSystem.Unsatisfiable(); bad {
		t.Errorf("a bypass predicate is never unsatisfiable")
	}
}

func TestTagPredicate_Unsatisfiable(t *testing.T) {
	cases := []struct {
		name string
		p    *TagPredicate
		want bool
	}{
		{"required tag is also forbidden",
			&TagPredicate{RequiredTags: []string{"secrets"}, ForbiddenTags: []string{"secrets"}}, true},
		{"an anyOf clause is fully forbidden",
			&TagPredicate{AnyOfClauses: [][]string{{"a", "b"}}, ForbiddenTags: []string{"a", "b"}}, true},
		{"a partially forbidden clause is still satisfiable",
			&TagPredicate{AnyOfClauses: [][]string{{"a", "b"}}, ForbiddenTags: []string{"a"}}, false},
		{"disjoint sets are satisfiable",
			&TagPredicate{RequiredTags: []string{"order_db"}, ForbiddenTags: []string{"internal_only"}}, false},
		{"empty predicate is satisfiable", &TagPredicate{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad, reason := tc.p.Unsatisfiable()
			if bad != tc.want {
				t.Fatalf("Unsatisfiable() = %v, want %v", bad, tc.want)
			}
			// INV-3: a zero-row result caused by an impossible predicate must be
			// explainable, so the reason string is part of the contract.
			if bad && reason == "" {
				t.Errorf("an unsatisfiable predicate must carry a reason")
			}
		})
	}
}

func TestTagPredicate_IsZero(t *testing.T) {
	var nilP *TagPredicate
	if !nilP.IsZero() {
		t.Errorf("nil predicate is zero")
	}
	if !(&TagPredicate{}).IsZero() {
		t.Errorf("empty predicate is zero")
	}
	// A bypass is NOT zero: it is an explicit, deliberate decision to admit
	// everything, and the distinction is what lets logs tell the two apart.
	if ScopeSystem.IsZero() {
		t.Errorf("the bypass sentinel must not read as an unconstrained-but-absent predicate")
	}
}

func TestAllowAllAuthorizer_FailsOpenAndSaysSo(t *testing.T) {
	// §4.2: in OSS, unrestricted is the correct and only semantics. Getting this
	// backwards makes OSS unusable (fail-closed) or premium insecure.
	a := AllowAllAuthorizer{}
	ctx := t.Context()

	pred, dec := a.ReadFilter(ctx, AgentPrincipal("anyone"), SurfaceRef{Kind: SurfaceAgent})
	if pred == nil || !pred.Bypass {
		t.Fatalf("OSS ReadFilter must hand out a bypass predicate, got %+v", pred)
	}
	if !dec.Allowed || dec.Reason != ReasonAllowed {
		t.Fatalf("OSS ReadFilter decision must be an explained allow, got %+v", dec)
	}
	if dec.Detail == "" {
		t.Errorf("even an allow-all decision explains itself, so an operator can tell OSS from a misconfigured policy")
	}

	// An unresolvable principal is still allowed in OSS — fail-closed is a
	// property of the plugin, never of the kernel default.
	if got := a.Authorize(ctx, AccessRequest{Resource: ResourceRef{Kind: KindMemory, ID: "d1"}}); !got.Allowed {
		t.Errorf("OSS must not deny an anonymous principal; that is the plugin's job")
	}

	kept, rejected := a.Filter(ctx, PrincipalRef{}, SurfaceRef{}, KindSkill, []Taggable{})
	if len(kept) != 0 || len(rejected) != 0 {
		t.Errorf("Filter over an empty candidate set must be empty/empty")
	}

	hint := []string{"public_kb"}
	out, wdec := a.ClassifyWrite(ctx, AgentPrincipal("w"), hint)
	if len(out) != 1 || out[0] != "public_kb" {
		t.Errorf("OSS writes keep their authored tags, got %v", out)
	}
	if !wdec.Allowed {
		t.Errorf("OSS never denies a write")
	}
}

func TestAuthorizerFromContext_DefaultsToAllowAll(t *testing.T) {
	if _, ok := AuthorizerFromContext(t.Context()).(AllowAllAuthorizer); !ok {
		t.Fatalf("an authorizer-free context must yield the allow-all default, not nil")
	}
	custom := AllowAllAuthorizer{}
	ctx := WithAuthorizer(t.Context(), custom)
	if AuthorizerFromContext(ctx) == nil {
		t.Fatalf("WithAuthorizer must round-trip")
	}
}

func TestAccessDecision_ExplainNamesTheResponsibleTerm(t *testing.T) {
	d := AccessDecision{
		Resource:  ResourceRef{Kind: KindMemory, ID: "doc-1"},
		Principal: AgentPrincipal("support_agent"),
		Surface:   SurfaceRef{Kind: SurfaceChat, ID: "public"},
		Reason:    ReasonForbiddenTag,
		Detail:    "internal_only",
		DecidedBy: []PolicyContribution{{
			PolicyID: "p1", PolicyName: "Outsider clamp", LinkedAt: "surface:public",
			Term: "forbidden", Values: []string{"internal_only"},
		}},
	}
	got := d.Explain()
	for _, want := range []string{"denied", "memory/doc-1", "agent:support_agent", "chat:public", "forbidden_tag", "internal_only", "Outsider clamp", "surface:public"} {
		if !contains(got, want) {
			t.Errorf("Explain() must mention %q; got %q", want, got)
		}
	}
}

func TestValidToolEffect_ClosedSet(t *testing.T) {
	for _, e := range AllToolEffects {
		if !ValidToolEffect(e) {
			t.Errorf("%q must be a member of the closed set", e)
		}
	}
	// An open string namespace is exactly what the closed set exists to prevent.
	for _, e := range []ToolEffect{"", "*", "read/*", "delete"} {
		if ValidToolEffect(e) {
			t.Errorf("%q must not be accepted as an effect class", e)
		}
	}
}

func contains(hay, needle string) bool {
	return len(needle) == 0 || (len(hay) >= len(needle) && indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
