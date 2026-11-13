package domain

import (
	"strings"
	"testing"
)

func TestOntologyDraftID_DeterministicOverContentNamespaceProposerAndRef(t *testing.T) {
	t.Parallel()
	a := OntologyDoc{
		Kinds:     []KindSpec{{Kind: "invoice", Predicates: map[string]ValueSpec{"amount": {Type: "number"}}}, {Kind: "case"}},
		Relations: []RelationSpec{{Name: "billed_to", Family: LinkFamilyRelation}},
	}
	// The same declarations in another spec order are the same change.
	b := OntologyDoc{Kinds: []KindSpec{a.Kinds[1], a.Kinds[0]}, Relations: a.Relations}
	id := func(ns string, d OntologyDoc, by, ref string) string {
		t.Helper()
		got, err := OntologyDraftID(ns, d, by, ref)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := id("default", a, "agent:planner", "")
	if !strings.HasPrefix(base, "od_") || len(base) != 3+32 {
		t.Fatalf("id shape: %q", base)
	}
	if again := id("default", a, "agent:planner", ""); again != base {
		t.Fatalf("a retried draft must be the same id: %s vs %s", again, base)
	}
	if reordered := id("default", b, "agent:planner", ""); reordered != base {
		t.Fatalf("spec order must not change the id: %s vs %s", reordered, base)
	}
	other := OntologyDoc{Kinds: []KindSpec{{Kind: "invoice", Predicates: map[string]ValueSpec{"amount": {Type: "text"}}}, {Kind: "case"}}}
	for name, got := range map[string]string{
		"namespace":  id("tenant-b", a, "agent:planner", ""),
		"proposer":   id("default", a, "agent:other", ""),
		"client ref": id("default", a, "agent:planner", "r2"),
		"content":    id("default", other, "agent:planner", ""),
	} {
		if got == base {
			t.Errorf("a different %s must be a different draft", name)
		}
	}
}

func TestParseOntologyChange_IsStrict(t *testing.T) {
	t.Parallel()
	if _, err := ParseOntologyChange(`{"kinds":[{"kind":"invoice","predicates":{"n":{"type":"number","requried":true}}}]}`); err == nil ||
		!strings.Contains(err.Error(), "requried") {
		t.Fatalf("a misspelt field must be refused, naming it: %v", err)
	}
	if _, err := ParseOntologyChange(`{}`); err == nil || !strings.Contains(err.Error(), "declares no kinds") {
		t.Fatalf("an empty change must be refused: %v", err)
	}
	doc, err := ParseOntologyChange(`{"relations":[{"name":"billed_to","family":"relation"}]}`)
	if err != nil || len(doc.Relations) != 1 || doc.Relations[0].Name != "billed_to" {
		t.Fatalf("a valid change: %+v %v", doc, err)
	}
}
