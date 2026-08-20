package domain

import (
	"errors"
	"strings"
	"testing"
)

// The identity plane's refusals are the only thing standing between "a heuristic
// proposed this" and "the substrate answers with it". They are pure functions precisely
// so they can be proved here, without a database — a rule that could only be tested
// against Postgres is a rule most test runs would skip.

func TestNewRelationRegistry_SeedsAlwaysPresent(t *testing.T) {
	// The two seed verbs are what the kernel's own lanes emit; a deployment that
	// declares nothing must still carry them (the ResolutionPolicyLatestAssertion
	// precedent).
	for _, reg := range []*RelationRegistry{nil, mustRegistry(t)} {
		same, ok := reg.Spec(RelationSameAs)
		if !ok {
			t.Fatalf("same_as missing from the registry")
		}
		if same.Family != LinkFamilyIdentity || !same.Symmetric || same.Closure != ClosureIdentity {
			t.Fatalf("same_as seeded wrong: %+v", same)
		}
		prec, ok := reg.Spec(RelationPrecededAndSharesEntities)
		if !ok {
			t.Fatalf("preceded_and_shares_entities missing from the registry")
		}
		// Closure MUST stay empty: co-occurrence establishes order and overlap,
		// never identity, and a closure here would let a correlation widen a read.
		if prec.Family != LinkFamilyLineage || prec.Closure != "" {
			t.Fatalf("preceded_and_shares_entities seeded wrong: %+v", prec)
		}
	}
}

func TestNewRelationRegistry_BootRefusals(t *testing.T) {
	cases := []struct {
		name  string
		specs []RelationSpec
		want  string
	}{
		{
			name:  "unnamed verb",
			specs: []RelationSpec{{Family: LinkFamilyRelation}},
			want:  "must name its verb",
		},
		{
			// Two owners for one verb is a fight the boot must referee, not the
			// write path — the KindRegistry rule, applied to verbs.
			name: "duplicate verb",
			specs: []RelationSpec{
				{Name: "subsidiary_of", Family: LinkFamilyRelation},
				{Name: "subsidiary_of", Family: LinkFamilyRelation},
			},
			want: "declared twice",
		},
		{
			name:  "redeclared built-in",
			specs: []RelationSpec{{Name: RelationSameAs, Family: LinkFamilyIdentity}},
			want:  "built in and cannot be redeclared",
		},
		{
			name:  "unknown family",
			specs: []RelationSpec{{Name: "subsidiary_of", Family: "association"}},
			want:  "unknown family",
		},
		{
			name:  "unknown closure",
			specs: []RelationSpec{{Name: "subsidiary_of", Family: LinkFamilyRelation, Closure: "transitive"}},
			want:  "unknown closure",
		},
		{
			name:  "negative cap",
			specs: []RelationSpec{{Name: "subsidiary_of", Family: LinkFamilyRelation, MaxPerEntity: -1}},
			want:  "negative MaxPerEntity",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRelationRegistry(tc.specs)
			if err == nil {
				t.Fatalf("expected a boot refusal, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the violation %q", err, tc.want)
			}
		})
	}
}

func TestRelationRegistry_VerbSetsAreDataNotBranches(t *testing.T) {
	reg, err := NewRelationRegistry([]RelationSpec{
		{Name: "alias_of", Family: LinkFamilyIdentity, Symmetric: true, Closure: ClosureIdentity},
		{Name: "subsidiary_of", Family: LinkFamilyRelation, Closure: ClosureRollup},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	// Read paths ask the registry for a SET and hand it to SQL; they never name a
	// verb. If these accessors stop returning the declared verbs, "both directions
	// for symmetric links" silently becomes "one direction".
	if got := reg.SymmetricVerbs(); !equalStrings(got, []string{"alias_of", RelationSameAs}) {
		t.Fatalf("SymmetricVerbs() = %v", got)
	}
	if got := reg.ClosureVerbs(ClosureIdentity); !equalStrings(got, []string{"alias_of", RelationSameAs}) {
		t.Fatalf("ClosureVerbs(identity) = %v", got)
	}
	if got := reg.ClosureVerbs(ClosureRollup); !equalStrings(got, []string{"subsidiary_of"}) {
		t.Fatalf("ClosureVerbs(rollup) = %v", got)
	}
}

func TestValidateLink_Refusals(t *testing.T) {
	reg := mustRegistry(t)
	base := Link{
		NamespaceID: "default",
		Family:      LinkFamilyIdentity,
		FromRef:     EntityRef("customer/A"),
		ToRef:       EntityRef("customer/B"),
		Relation:    RelationSameAs,
		State:       LinkStateCandidate,
		Mechanism:   LinkMechanismDeclared,
		EvidenceID:  "ev-1",
		AssertedBy:  "mapping/orders",
	}
	if err := reg.ValidateLink(base); err != nil {
		t.Fatalf("the admissible baseline was refused: %v", err)
	}

	cases := []struct {
		name string
		mut  func(l *Link)
		want string
	}{
		{
			// The vocabulary is the deployment's decision; a verb nobody declared
			// must not be admitted at some default.
			name: "unknown verb",
			mut:  func(l *Link) { l.Relation = "probably_the_same_as" },
			want: "is not declared",
		},
		{
			name: "family disagrees with the declaration",
			mut:  func(l *Link) { l.Family = LinkFamilyRelation },
			want: "belongs to family",
		},
		{
			// The trust ceiling: a similarity score does not become true by being
			// confident, so `scored` caps at candidate no matter what it claims.
			name: "trust ceiling — scored may not confirm",
			mut: func(l *Link) {
				l.Mechanism = LinkMechanismScored
				l.State = LinkStateConfirmed
				l.Confidence = 0.99
			},
			want: "may assert at most",
		},
		{
			name: "trust ceiling — derived may not confirm",
			mut: func(l *Link) {
				l.Mechanism = LinkMechanismDerived
				l.State = LinkStateConfirmed
			},
			want: "may assert at most",
		},
		{
			name: "trust ceiling — correlation may not confirm",
			mut: func(l *Link) {
				l.Mechanism = LinkMechanismCorrelation
				l.Relation = RelationPrecededAndSharesEntities
				l.Family = LinkFamilyLineage
				l.State = LinkStateConfirmed
			},
			want: "may assert at most",
		},
		{
			name: "unknown mechanism",
			mut:  func(l *Link) { l.Mechanism = "vibes" },
			want: "not a known mechanism",
		},
		{
			// Admissibility: a machine that cannot say why it believes something
			// has not made an assertion.
			name: "no evidence behind a machine assertion",
			mut:  func(l *Link) { l.EvidenceID = "" },
			want: "asserted with no evidence",
		},
		{
			name: "unknown state",
			mut:  func(l *Link) { l.State = "maybe" },
			want: "is not candidate",
		},
		{
			name: "no asserter",
			mut:  func(l *Link) { l.AssertedBy = "" },
			want: "asserted_by",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := base
			tc.mut(&l)
			err := reg.ValidateLink(l)
			if err == nil {
				t.Fatalf("expected a refusal, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the violation %q", err, tc.want)
			}
			// PERMANENT, and legible as such to the outbox consumers that already
			// learned ErrKindRefused — an at-least-once lane that reads this as
			// transient retries one bad row forever.
			if !errors.Is(err, ErrLinkRefused) {
				t.Fatalf("refusal is not ErrLinkRefused: %v", err)
			}
			if !errors.Is(err, ErrKindRefused) {
				t.Fatalf("refusal is not legible as a permanent kind refusal: %v", err)
			}
		})
	}
}

func TestValidateLink_HumanNeedsNoEvidence(t *testing.T) {
	// The admissibility rule binds MACHINES. A person asserting something is the
	// basis; requiring an evidence row would only produce fake ones.
	reg := mustRegistry(t)
	l := Link{
		Family:     LinkFamilyIdentity,
		FromRef:    EntityRef("customer/A"),
		ToRef:      EntityRef("customer/B"),
		Relation:   RelationSameAs,
		State:      LinkStateConfirmed,
		Mechanism:  LinkMechanismHuman,
		AssertedBy: "operator:ada",
	}
	if err := reg.ValidateLink(l); err != nil {
		t.Fatalf("a human confirmation with no evidence was refused: %v", err)
	}
}

func TestCanonicalizeLink(t *testing.T) {
	// Without canonical ordering, "A same_as B" and "B same_as A" are two rows no
	// dedup key can reconcile — the same equivalence counted twice and reviewed
	// twice.
	swapped := CanonicalizeLink(Link{
		Family:   LinkFamilyIdentity,
		FromRef:  EntityRef("customer/Z"),
		ToRef:    EntityRef("customer/A"),
		Relation: RelationSameAs,
	})
	if swapped.FromRef != EntityRef("customer/A") || swapped.ToRef != EntityRef("customer/Z") {
		t.Fatalf("identity endpoints not canonically ordered: %s → %s", swapped.FromRef, swapped.ToRef)
	}
	if already := CanonicalizeLink(swapped); already != swapped {
		t.Fatalf("canonicalisation is not idempotent")
	}
	// Direction is the MEANING in the other families; swapping "A subsidiary_of B"
	// would invert the claim.
	rel := Link{
		Family:   LinkFamilyRelation,
		FromRef:  EntityRef("company/Z"),
		ToRef:    EntityRef("company/A"),
		Relation: "subsidiary_of",
	}
	if got := CanonicalizeLink(rel); got != rel {
		t.Fatalf("a relation link was reordered: %+v", got)
	}
}

func TestEntityKindFromID(t *testing.T) {
	cases := []struct {
		id   string
		want string
		ok   bool
	}{
		{"customer/C-1042", "customer", true},
		{"purchase_order/PO-7", "purchase_order", true},
		// An unscoped id is the collision the prefix exists to prevent, so it is a
		// refusal rather than a default kind.
		{"C-1042", "", false},
		{"/C-1042", "", false},
		{"customer/", "", false},
	}
	for _, tc := range cases {
		got, ok := EntityKindFromID(tc.id)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("EntityKindFromID(%q) = %q,%v want %q,%v", tc.id, got, ok, tc.want, tc.ok)
		}
	}
}

func TestValidateEntityKind_WellFormednessOnly(t *testing.T) {
	// Well-formedness ONLY (amendment S3): a deployment cannot enumerate every kind
	// its sources will ever carry, so an undeclared-but-well-formed kind passes here
	// and is checked where a human is present.
	for _, ok := range []string{"customer", "purchase_order", "gate3"} {
		if err := ValidateEntityKind(ok); err != nil {
			t.Fatalf("ValidateEntityKind(%q) refused a well-formed kind: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Customer", "purchase-order", "customer/sub", "1customer", "_x"} {
		err := ValidateEntityKind(bad)
		if err == nil {
			t.Fatalf("ValidateEntityKind(%q) admitted a malformed kind", bad)
		}
		if !errors.Is(err, ErrLinkRefused) {
			t.Fatalf("ValidateEntityKind(%q) refusal is not permanent: %v", bad, err)
		}
	}
}

func mustRegistry(t *testing.T) *RelationRegistry {
	t.Helper()
	reg, err := NewRelationRegistry(nil)
	if err != nil {
		t.Fatalf("relation registry: %v", err)
	}
	return reg
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
