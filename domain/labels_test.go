package domain

import (
	"reflect"
	"strings"
	"testing"
)

// ENG-2170: the one label rule both ingest planes apply.
func TestNormalizeLabels(t *testing.T) {
	got, err := NormalizeLabels([]string{" in-force ", "rev3", "", "rev3", "SOP-001", "  "}, 0, 0)
	if err != nil {
		t.Fatalf("NormalizeLabels: %v", err)
	}
	if want := []string{"in-force", "rev3", "SOP-001"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v (trimmed, blanks and duplicates dropped, case kept)", got, want)
	}
	if got, err := NormalizeLabels(nil, 0, 0); err != nil || got != nil {
		t.Fatalf("no labels = %v, %v; want nil, nil", got, err)
	}
	for name, tc := range map[string]struct {
		in              []string
		maxCount, chars int
	}{
		"control character": {in: []string{"in-force\nrole: admin"}},
		"tab":               {in: []string{"a\tb"}},
		"too long":          {in: []string{strings.Repeat("x", 9)}, chars: 8},
		"too long default":  {in: []string{strings.Repeat("é", DefaultLabelMaxChars+1)}},
		"too many":          {in: []string{"a", "b", "c"}, maxCount: 2},
		"too many default":  {in: manyLabels(DefaultLabelMaxCount + 1)},
	} {
		if _, err := NormalizeLabels(tc.in, tc.maxCount, tc.chars); err == nil {
			t.Errorf("%s: accepted, want refused", name)
		}
	}
	// A label at the limit, counted in characters (runes), is accepted.
	if _, err := NormalizeLabels([]string{strings.Repeat("é", 8)}, 1, 8); err != nil {
		t.Errorf("8-rune label at an 8-character cap refused: %v", err)
	}
	// Duplicates do not count against the cap.
	if _, err := NormalizeLabels([]string{"a", "a", "b"}, 2, 0); err != nil {
		t.Errorf("duplicates counted against the cap: %v", err)
	}
}

func manyLabels(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "l" + strings.Repeat("x", i)
	}
	return out
}

func TestLabelsOf_BothEncodings(t *testing.T) {
	if got := LabelsOf(map[string]any{MetaLabels: []string{"a"}}); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("[]string: %v", got)
	}
	if got := LabelsOf(map[string]any{MetaLabels: []any{"a", 3, "b"}}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("[]any: %v", got)
	}
	if LabelsOf(nil) != nil || LabelsOf(map[string]any{"tags": []string{"x"}}) != nil {
		t.Error("no labels must read as nil, and tags are never labels")
	}
}

// Labels land in the documents-row metadata beside, never inside, the tags.
func TestSourceDocumentMetadata_CarriesLabels(t *testing.T) {
	m := SourceDocumentMetadata(ExternalDocument{Tags: []string{"scope-docs"}, Labels: []string{"rev2", "superseded"}})
	if got := LabelsOf(m); !reflect.DeepEqual(got, []string{"rev2", "superseded"}) {
		t.Fatalf("metadata labels = %v", got)
	}
	if _, ok := m["tags"]; ok {
		t.Fatal("documents.metadata must not carry tags (the tags column is the classification)")
	}
	if SourceDocumentMetadata(ExternalDocument{}) != nil {
		t.Fatal("no descriptive metadata and no labels must stay nil")
	}
}
