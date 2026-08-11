package domain

import (
	"errors"
	"testing"
)

// Row-level entitlement (ADR-0121). These are mostly invariant tests, because
// the invariants are what the design is: a term that can only narrow, that
// survives every projection or refuses, and that is never mistaken for empty.

func partyScoped() *TagPredicate {
	return &TagPredicate{
		RequiredTags:    []string{"orders"},
		PartyScopedTags: []string{"orders"},
		PartyIdentities: []string{"acct:A-1042", "employee:E-42"},
	}
}

// The case the whole ADR exists for: two readers, one record, different answers.
func TestPartyScoping_TheSameRowAnswersDifferentlyPerReader(t *testing.T) {
	row := []string{"orders"}
	mine := []string{"acct:A-1042"}
	theirs := []string{"acct:A-9999"}

	if !partyScoped().AllowsRow(row, mine) {
		t.Error("a reader who is a party must be admitted")
	}
	if partyScoped().AllowsRow(row, theirs) {
		t.Error("a reader who is not a party must be refused")
	}
}

// It can only ever REMOVE rows. Adding the term to a predicate must never admit
// something the tag terms alone rejected — INV-1, and the reason party-scoping
// composes with an intersection algebra at all.
func TestPartyScoping_NeverWidens(t *testing.T) {
	tagsOnly := &TagPredicate{RequiredTags: []string{"orders"}}
	withParty := partyScoped()

	for _, row := range [][]string{
		{}, {"orders"}, {"hr"}, {"orders", "hr"},
	} {
		// Identities generous enough to satisfy any party test.
		if withParty.AllowsRow(row, []string{"acct:A-1042"}) && !tagsOnly.Allows(row) {
			t.Errorf("row %v: party-scoping admitted a row the tag terms refused", row)
		}
	}
}

// A row WITHOUT the party-scoped tag is untouched by the qualifier — the
// implication's left disjunct.
func TestPartyScoping_SaysNothingAboutRowsWithoutTheTag(t *testing.T) {
	p := &TagPredicate{PartyScopedTags: []string{"orders"}}
	if !p.AllowsRow([]string{"hr"}, nil) {
		t.Error("a row that does not carry the party-scoped tag is not restricted by it")
	}
	if p.AllowsRow([]string{"orders"}, nil) {
		t.Error("a row that DOES carry it, with no identities, must be refused")
	}
}

// Fail closed: no identities resolved means no party-scoped row is readable.
// "Party to nothing" and "we could not tell" must agree in the direction of
// access (D6).
func TestPartyScoping_NoIdentitiesReadsNothing(t *testing.T) {
	p := &TagPredicate{PartyScopedTags: []string{"orders"}, PartyIdentities: nil}
	if p.AllowsRow([]string{"orders"}, []string{"acct:A-1042"}) {
		t.Error("a reader with no resolved identities must read no party-scoped row")
	}
}

// A resource with NO parties — a tool, a skill, a document — cannot have the
// reader as one, so a party-scoped tag it carries denies. Fail-closed AND
// correct: if the policy says "only rows you are a party to", a thing nobody can
// be a party to is not one of them.
func TestPartyScoping_APartylessResourceIsRefusedNotIgnored(t *testing.T) {
	p := partyScoped()
	if p.Allows([]string{"orders"}) {
		t.Error("the party-blind test must refuse a party-scoped tag, not ignore it")
	}
	dec, ok := p.Check([]string{"orders"})
	if ok || dec.Reason != ReasonNotAParty {
		t.Errorf("want %s, got %s / allowed=%v", ReasonNotAParty, dec.Reason, ok)
	}
	if dec.Detail != "orders" {
		t.Errorf("the detail should name the responsible tag, got %q", dec.Detail)
	}
}

// The denial must not enumerate other people.
func TestPartyScoping_TheDenialDoesNotLeakIdentities(t *testing.T) {
	dec, _ := partyScoped().CheckRow([]string{"orders"}, []string{"acct:A-9999"})
	for _, secret := range []string{"acct:A-1042", "employee:E-42", "acct:A-9999"} {
		if dec.Detail == secret {
			t.Errorf("the denial detail leaked an identity: %q", dec.Detail)
		}
	}
}

// A tag denial outranks a party denial: the reader should be told the first
// thing that is actually wrong, not the most specific one.
func TestPartyScoping_ATagDenialIsReportedBeforeAPartyDenial(t *testing.T) {
	p := &TagPredicate{
		ForbiddenTags:   []string{"secret"},
		PartyScopedTags: []string{"orders"},
	}
	dec, ok := p.CheckRow([]string{"orders", "secret"}, nil)
	if ok {
		t.Fatal("a forbidden tag denies")
	}
	if dec.Reason != ReasonForbiddenTag {
		t.Errorf("want the forbidden tag named first, got %s", dec.Reason)
	}
}

// Bypass admits everything, party terms included (D5a). A maintenance sweep has
// no identities to be a party to, so the alternative is a GC that reads nothing.
func TestPartyScoping_BypassSkipsPartyTerms(t *testing.T) {
	if !ScopeSystem.AllowsRow([]string{"orders"}, nil) {
		t.Error("bypass must admit a party-scoped row")
	}
}

// IsZero must count the term. A predicate whose only term is party-scoping
// constrains a great deal, and reading it as empty suppresses INV-3's
// policy_note at querymemory.go's `!pred.IsZero()` gate — the silent empty D6
// exists to prevent.
func TestPartyScoping_IsZeroCountsTheTerm(t *testing.T) {
	p := &TagPredicate{PartyScopedTags: []string{"orders"}}
	if p.IsZero() {
		t.Fatal("a party-only predicate is not empty; reading it as empty suppresses the policy note")
	}
	// Identities alone genuinely constrain nothing.
	if !(&TagPredicate{PartyIdentities: []string{"acct:A-1"}}).IsZero() {
		t.Error("identities without a party-scoped tag restrict nothing")
	}
}

// D1b: a projection that cannot carry the term must REFUSE, never truncate.
// Truncating keeps the tag grant and drops the restriction — "restriction lost,
// permission kept", which is a widening.
func TestPartyScoping_ProjectingToATagSetRefusesRatherThanDropping(t *testing.T) {
	_, err := partyScoped().ToTagSet()
	if err == nil {
		t.Fatal("projecting a party-scoped predicate into a TagSet must refuse")
	}
	if !errors.Is(err, ErrPartyTermNotCarryable) {
		t.Errorf("want ErrPartyTermNotCarryable, got %v", err)
	}

	// Without a party term it projects cleanly.
	plain := &TagPredicate{RequiredTags: []string{"orders"}, ForbiddenTags: []string{"secret"}}
	ts, err := plain.ToTagSet()
	if err != nil {
		t.Fatalf("a plain predicate must project: %v", err)
	}
	if len(ts.RequiredTags) != 1 || ts.RequiredTags[0] != "orders" {
		t.Errorf("the tag terms must survive the projection: %+v", ts)
	}

	// And two any-of clauses cannot be flattened into one OR without widening.
	multi := &TagPredicate{AnyOfClauses: [][]string{{"a"}, {"b"}}}
	if _, err := multi.ToTagSet(); err == nil {
		t.Error("two AND-composed clauses cannot become one OR set")
	}
}
