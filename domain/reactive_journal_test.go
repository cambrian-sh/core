package domain

import "testing"

// ENG-1966: a dead letter matches an erasure only when its signal payload names an
// erased id exactly, at any depth; text that merely contains the id does not.
func TestReactiveDeadLetter_ReferencesAny(t *testing.T) {
	ids := map[string]bool{"doc-1": true, "cid-9": true}
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{"top-level", map[string]any{"document_id": "doc-1"}, true},
		{"nested map", map[string]any{"delivery": map[string]any{"cid": "cid-9"}}, true},
		{"list", map[string]any{"refs": []any{"x", "doc-1"}}, true},
		{"string list", map[string]any{"refs": []string{"cid-9"}}, true},
		{"substring is not a reference", map[string]any{"text": "see doc-1 please"}, false},
		{"none", map[string]any{"n": 3, "s": "other"}, false},
		{"empty payload", nil, false},
	}
	for _, c := range cases {
		dl := ReactiveDeadLetter{Signal: Signal{Payload: c.payload, RawText: "doc-1"}}
		if got := dl.ReferencesAny(ids); got != c.want {
			t.Errorf("%s: ReferencesAny = %v, want %v", c.name, got, c.want)
		}
	}
	if (ReactiveDeadLetter{Signal: Signal{Payload: map[string]any{"a": "doc-1"}}}).ReferencesAny(nil) {
		t.Error("no ids must match nothing")
	}
}
