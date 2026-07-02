package domain

import "testing"

// The read-gate: ownerless ⇒ open; owner match ⇒ read; owner mismatch (incl. an
// ownerless caller against an owned node) ⇒ denied.
func TestCanReadContentNode(t *testing.T) {
	cases := []struct {
		owner, caller string
		want          bool
	}{
		{"", "anyone", true},  // system/legacy content readable by anyone
		{"", "", true},        // ownerless, no caller session
		{"s1", "s1", true},    // owner reads own node
		{"s1", "s2", false},   // another session denied
		{"s1", "", false},     // a caller with no session cannot read an owned node
	}
	for _, c := range cases {
		if got := CanReadContentNode(c.owner, c.caller); got != c.want {
			t.Errorf("CanReadContentNode(%q, %q) = %v, want %v", c.owner, c.caller, got, c.want)
		}
	}
}
