package domain

import (
	"slices"
	"testing"
)

// ENG-2106: a memory-note entry id is content-derived (no clock, no position) and
// round-trips to its document; anything else does not parse.
func TestMemoryNoteEntryID_RoundTripAndDeterministic(t *testing.T) {
	id := MemoryNoteEntryID("personal", "emp:EMP-001", "  Locker code: LOCKER-b681ce ")
	if id != MemoryNoteEntryID("personal", "emp:EMP-001", "Locker code: LOCKER-b681ce") {
		t.Fatal("the id must ignore surrounding whitespace and be deterministic")
	}
	scope, owner, digest, ok := ParseMemoryNoteEntryID(id)
	if !ok || scope != "personal" || owner != "emp:EMP-001" || digest != MemoryNoteEntryDigest("Locker code: LOCKER-b681ce")[:16] {
		t.Fatalf("parse(%q) = %q %q %q %v", id, scope, owner, digest, ok)
	}
	if MemoryNoteEntryID("personal", "emp:EMP-001", "other line") == id {
		t.Fatal("different lines must have different ids")
	}
	for _, bad := range []string{"", "chunk-1", "memory-note:personal", "memory-note:personal#abc",
		"memory-note::owner#0123456789abcdef", "memory-note:personal:#0123456789abcdef", "memory-note:personal:o#short"} {
		if _, _, _, ok := ParseMemoryNoteEntryID(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

// The selector carries memory-note ids as their own kind: in the request identity, the
// kinds, the normalized form and the candidates' concrete selector.
func TestErasureSelector_MemoryNoteKind(t *testing.T) {
	a := MemoryNoteEntryID("personal", "p", "a")
	b := MemoryNoteEntryID("personal", "p", "b")
	sel := ErasureSelector{MemoryNoteIDs: []string{b, a, b}}
	if sel.IsEmpty() || sel.Count() != 2 || !slices.Equal(sel.Kinds(), []string{ErasureTargetMemoryNote}) {
		t.Fatalf("selector = %+v kinds %v count %d", sel, sel.Kinds(), sel.Count())
	}
	if want := SortedUniqueIDs([]string{a, b}); !slices.Equal(sel.Normalized().MemoryNoteIDs, want) {
		t.Fatalf("normalized = %v", sel.Normalized().MemoryNoteIDs)
	}
	req := ErasureRequest{Principal: "agent", PrincipalKind: ErasureByAgent, Reason: ErasureReasonUserRequest, Selector: sel}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	other := req
	other.Selector = ErasureSelector{ChunkIDs: []string{a, b}}
	if req.ID() == other.ID() {
		t.Fatal("the kind must be part of the request identity")
	}
	got := SelectorOfCandidates([]ErasureCandidate{{Kind: ErasureTargetMemoryNote, ID: a}})
	if !slices.Equal(got.MemoryNoteIDs, []string{a}) {
		t.Fatalf("SelectorOfCandidates = %+v", got)
	}
}

// ENG-2147: the subject-wide selector kind is its own kind, normalized, part of the
// request identity, and a memory_notes_owner candidate maps back to it.
func TestErasureSelector_MemoryNoteOwners(t *testing.T) {
	sel := ErasureSelector{MemoryNoteOwners: []string{"bob", "alice", "bob"}}.Normalized()
	if !slices.Equal(sel.MemoryNoteOwners, []string{"alice", "bob"}) || sel.Count() != 2 ||
		!slices.Equal(sel.Kinds(), []string{ErasureTargetMemoryNotesOwner}) {
		t.Fatalf("normalized = %+v kinds %v", sel, sel.Kinds())
	}
	a := ErasureRequest{Principal: "ops", PrincipalKind: ErasureByOperator, Selector: ErasureSelector{MemoryNoteOwners: []string{"alice"}}}
	b := ErasureRequest{Principal: "ops", PrincipalKind: ErasureByOperator, Selector: ErasureSelector{MemoryNoteIDs: []string{"alice"}}}
	if a.ID() == b.ID() {
		t.Fatal("a subject-wide erase and a line erase must not share a request id")
	}
	got := SelectorOfCandidates([]ErasureCandidate{{Kind: ErasureTargetMemoryNotesOwner, ID: "alice"}})
	if !slices.Equal(got.MemoryNoteOwners, []string{"alice"}) || len(got.MemoryNoteIDs) != 0 {
		t.Fatalf("SelectorOfCandidates = %+v", got)
	}
}
