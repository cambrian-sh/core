package domain

import (
	"context"
	"testing"
)

// ADR-0146, the own-rows rule, on the AUTHORITATIVE Go form (Check / Allows /
// CheckRow / LedgerEntryReadable). The predicates below have the shape premium's
// resolveParts builds: an agent (floor any_of [provenance:source=composed_worker,
// ops]) reading for owner A, whose floor is any_of [provenance:source=A, shared:x,
// engineering] and who has a closed tag "hr" not granted (→ forbidden).

const ownA = "vantis-ops-lkovacs"

func ownRowsAgentForA() *TagPredicate {
	agentAnyOf := []string{"provenance:source=composed_worker", "ops"}
	ownerAnyOf := []string{"provenance:source=" + ownA, "shared:x", "engineering"}
	return &TagPredicate{
		// main = agent ∩ owner (both any_of clauses ANDed), the owner's closure.
		AnyOfClauses:  [][]string{agentAnyOf, ownerAnyOf},
		ForbiddenTags: []string{"hr"},
		Owners:        []string{ownA},
		OwnRows: &TagPredicate{
			// OwnRows = agent ∩ restrictive(owner).
			AnyOfClauses:  [][]string{agentAnyOf},
			ForbiddenTags: []string{"hr"},
			Owners:        []string{ownA},
		},
	}
}

// Test 1: the regression — the worker's own entry is readable.
func TestOwnRows_AgentReadsWhatItWroteForItsOwner(t *testing.T) {
	p := ownRowsAgentForA()
	row := []string{"provenance:source=composed_worker", OwnerTag(ownA)}
	dec, ok := p.Check(row)
	if !ok {
		t.Fatalf("own row denied: %+v", dec)
	}
	if dec.Detail != ownRowDetail {
		t.Errorf("an own-row allow should say so, got %+v", dec)
	}
	// Without the term (today's behaviour) the same row is denied by the OWNER's
	// any_of — that is the defect.
	main := *p
	main.OwnRows = nil
	if d, ok := main.Check(row); ok || d.Reason != ReasonAnyOfUnsatisfied {
		t.Fatalf("main predicate alone: %+v %v", d, ok)
	}
	// A ledger entry of that shape (all 13 entries of the evidence run).
	e := LedgerEntry{Tags: row}
	if !LedgerEntryReadable(p, e) {
		t.Fatal("LedgerEntryReadable refused the worker's own entry")
	}
}

// Test 2: no conversation owner on ctx → the decision point builds no Owners and
// no OwnRows; the fence then hides every owner-stamped row.
func TestOwnRows_NoOwnerNoExemption(t *testing.T) {
	p := &TagPredicate{AnyOfClauses: [][]string{{"provenance:source=composed_worker", "ops"}}}
	if p.Allows([]string{"provenance:source=composed_worker", OwnerTag(ownA)}) {
		t.Fatal("an agent acting for nobody read an owner-stamped row")
	}
	// Even a (malformed) OwnRows with no outer owner is ignored.
	p.OwnRows = &TagPredicate{Owners: []string{ownA}}
	if p.Allows([]string{"provenance:source=composed_worker", OwnerTag(ownA)}) {
		t.Fatal("an OwnRows term without an outer owner was applied")
	}
}

// Test 3: mixed owners and a foreign owner are never own rows.
func TestOwnRows_MixedOrForeignOwnersDenied(t *testing.T) {
	p := ownRowsAgentForA()
	for name, row := range map[string][]string{
		"A and B":      {"provenance:source=composed_worker", OwnerTag(ownA), OwnerTag("bob")},
		"B only":       {"provenance:source=composed_worker", OwnerTag("bob")},
		"empty owner":  {"provenance:source=composed_worker", OwnerTag(ownA), OwnerTagPrefix},
		"no owner tag": {"provenance:source=composed_worker"},
	} {
		if p.Allows(row) {
			t.Errorf("%s: admitted %v", name, row)
		}
		if p.IsOwnRow(row) {
			t.Errorf("%s: classified as an own row", name)
		}
	}
	// A duplicated own tag is still exactly {owner:A}.
	if !p.IsOwnRow([]string{OwnerTag(ownA), OwnerTag(ownA)}) {
		t.Error("a duplicated own tag is still an own row")
	}
}

// Test 4: a tag closed for A (and not granted) — enforced as A's forbidden tag —
// still denies an own row.
func TestOwnRows_OwnersForbiddenAndClosedTagsStillDeny(t *testing.T) {
	p := ownRowsAgentForA()
	if d, ok := p.Check([]string{"provenance:source=composed_worker", OwnerTag(ownA), "hr"}); ok || d.Reason != ReasonForbiddenTag {
		t.Fatalf("closed tag on an own row: %+v %v", d, ok)
	}
}

// Test 5: the AGENT's positive terms still apply to its owner's rows.
func TestOwnRows_AgentFloorStillApplies(t *testing.T) {
	p := ownRowsAgentForA()
	// The row satisfies the OWNER's any_of but not the agent's.
	if p.Allows([]string{"engineering", OwnerTag(ownA)}) {
		t.Fatal("an own row outside the agent's floor was admitted")
	}
	// The agent's required tag, when it has one.
	p.RequiredTags = []string{"tenant:t1"}
	p.OwnRows.RequiredTags = []string{"tenant:t1"}
	if p.Allows([]string{"provenance:source=composed_worker", OwnerTag(ownA)}) {
		t.Fatal("the agent's required tag was waived")
	}
}

// Test 6: a derived entry whose sources A's full predicate denies stays denied,
// each SourceTags set classified on its own.
func TestOwnRows_DerivedEntryWithDeniedSourcesDenied(t *testing.T) {
	p := ownRowsAgentForA()
	own := []string{"provenance:source=composed_worker", OwnerTag(ownA)}
	for name, src := range map[string][]string{
		"forbidden tag":                       {"provenance:source=composed_worker", OwnerTag(ownA), "hr"},
		"foreign owner":                       {"provenance:source=composed_worker", OwnerTag("bob")},
		"classified outside A's any_of, no A": {"provenance:source=composed_worker", "finance"},
	} {
		e := LedgerEntry{Tags: own, SourceTags: [][]string{own, src}}
		if LedgerEntryReadable(p, e) {
			t.Errorf("%s: derived entry readable through a denied source %v", name, src)
		}
	}
	// Sources that are A's own rows keep the derived entry readable.
	e := LedgerEntry{Tags: own, SourceTags: [][]string{own}}
	if !LedgerEntryReadable(p, e) {
		t.Fatal("a derived entry of the owner's own sources was refused")
	}
}

// D4 / D2: a malformed OwnRows is never applied (OwnRowTerm is the one gate), and
// a nil predicate admits nothing whatever it would have carried.
func TestOwnRows_MalformedTermIgnored(t *testing.T) {
	row := []string{"provenance:source=composed_worker", OwnerTag(ownA)}
	for name, mutate := range map[string]func(p *TagPredicate){
		"inner bypass":        func(p *TagPredicate) { p.OwnRows.Bypass = true },
		"inner all owners":    func(p *TagPredicate) { p.OwnRows.AllOwners = true },
		"inner ledger grant":  func(p *TagPredicate) { p.OwnRows.LedgerRuns = &LedgerRunGrant{Roots: []string{"r"}} },
		"nested own rows":     func(p *TagPredicate) { p.OwnRows.OwnRows = &TagPredicate{Owners: []string{ownA}} },
		"inner owner differs": func(p *TagPredicate) { p.OwnRows.Owners = []string{"bob"} },
		"inner two owners":    func(p *TagPredicate) { p.OwnRows.Owners = []string{ownA, "bob"} },
		"outer two owners":    func(p *TagPredicate) { p.Owners = []string{ownA, "bob"}; p.OwnRows.Owners = []string{ownA} },
		"outer no owner":      func(p *TagPredicate) { p.Owners = nil },
	} {
		p := ownRowsAgentForA()
		mutate(p)
		if p.OwnRowTerm() != nil {
			t.Errorf("%s: OwnRowTerm accepted a malformed term", name)
		}
		if p.Allows(row) {
			t.Errorf("%s: a malformed OwnRows admitted the row", name)
		}
	}
	var nilPred *TagPredicate
	if nilPred.Allows(row) || nilPred.IsOwnRow(row) || nilPred.OwnRowTerm() != nil {
		t.Fatal("a nil predicate admitted something")
	}
}

// D1 for a PERSON reading directly (owner decision 2026-11-12): OwnRows is their
// predicate without its positive terms; their own rows are readable, shared
// material still bounded by their floor, a forbidden tag still denies.
func TestOwnRows_PersonReadsOwnRows(t *testing.T) {
	p := &TagPredicate{
		RequiredTags:  []string{"tenant:t1"},
		AnyOfClauses:  [][]string{{"provenance:source=" + ownA, "shared:x", "engineering"}},
		ForbiddenTags: []string{"hr"},
		Owners:        []string{ownA},
		OwnRows:       &TagPredicate{ForbiddenTags: []string{"hr"}, Owners: []string{ownA}},
	}
	if !p.Allows([]string{"provenance:source=composed_worker", OwnerTag(ownA)}) {
		t.Fatal("the person cannot read a row their assistant wrote for them")
	}
	if !p.Allows([]string{"mail", OwnerTag(ownA)}) {
		t.Fatal("the person cannot read their own personal ingest")
	}
	if p.Allows([]string{"finance"}) {
		t.Fatal("the floor no longer bounds shared material")
	}
	if p.Allows([]string{"mail", OwnerTag(ownA), "hr"}) {
		t.Fatal("a forbidden tag on the person's own row was waived")
	}
}

// Party scoping (ADR-0121) still applies to an own row on the row-aware form.
func TestOwnRows_PartyScopingStillApplies(t *testing.T) {
	p := ownRowsAgentForA()
	p.PartyScopedTags = []string{"case"}
	p.PartyIdentities = []string{"ent:a"}
	p.OwnRows.PartyScopedTags = []string{"case"}
	p.OwnRows.PartyIdentities = []string{"ent:a"}
	row := []string{"provenance:source=composed_worker", OwnerTag(ownA), "case"}
	if _, ok := p.CheckRow(row, []string{"ent:b"}); ok {
		t.Fatal("an own row the reader is not a party to was admitted")
	}
	if _, ok := p.CheckRow(row, []string{"ent:a"}); !ok {
		t.Fatal("an own row the reader is a party to was refused")
	}
	if _, ok := p.CheckRow([]string{"provenance:source=composed_worker", OwnerTag(ownA)}, nil); !ok {
		t.Fatal("an own row with no party-scoped tag was refused on the row-aware form")
	}
}

// Test 11, structurally: an enforcement point unaware of OwnRows applies the main
// predicate, which never admits more than main ∪ own-rows does — over a matrix,
// main ⊆ Check(p).
func TestOwnRows_MainNeverWiderThanCheck(t *testing.T) {
	p := ownRowsAgentForA()
	main := *p
	main.OwnRows = nil
	rows := [][]string{
		{"provenance:source=composed_worker", OwnerTag(ownA)},
		{"provenance:source=composed_worker", "engineering", OwnerTag(ownA)},
		{"ops", "shared:x"},
		{"ops", "hr", OwnerTag(ownA)},
		{"engineering"},
		{OwnerTag(ownA)},
		{"ops", OwnerTag("bob")},
		{},
	}
	for _, r := range rows {
		if main.Allows(r) && !p.Allows(r) {
			t.Errorf("main admits %v but the full predicate refuses it", r)
		}
		if main.Allows(r) && !p.OwnRows.Allows(r) && p.IsOwnRow(r) {
			t.Errorf("main ⊄ OwnRows on own row %v", r)
		}
	}
}

func TestDropCallerOwnerTags(t *testing.T) {
	in := []string{"sales", OwnerTag("bob"), "provenance:source=x", OwnerTagPrefix}
	got := DropCallerOwnerTags(context.Background(), "test", in)
	if len(got) != 2 || got[0] != "sales" || got[1] != "provenance:source=x" {
		t.Fatalf("got %v", got)
	}
	if len(in) != 4 || in[1] != OwnerTag("bob") {
		t.Fatal("the input was mutated")
	}
	clean := []string{"sales"}
	if out := DropCallerOwnerTags(context.Background(), "test", clean); &out[0] != &clean[0] {
		t.Fatal("a list with no owner tag should be returned as is")
	}
	if DropCallerOwnerTags(context.Background(), "test", nil) != nil {
		t.Fatal("nil in, nil out")
	}
}
