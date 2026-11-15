package domain

import (
	"errors"
	"strings"
	"testing"
)

// ENG-1046 (owner decision 2026-11-15): an owner-namespace tag in an
// operator-authored descriptor tag list is refused, naming the field and tag.
func TestRefuseOwnerTags(t *testing.T) {
	if err := RefuseOwnerTags("scope_tags", nil); err != nil {
		t.Fatalf("nil list: %v", err)
	}
	if err := RefuseOwnerTags("scope_tags", []string{"ops", "finance", "owners", "x:owner:y"}); err != nil {
		t.Fatalf("non-owner tags refused: %v", err)
	}
	err := RefuseOwnerTags("scope_tags", []string{"ops", "owner:alice", "owner:bob"})
	if !errors.Is(err, ErrOwnerTagInDescriptor) {
		t.Fatalf("want ErrOwnerTagInDescriptor, got %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "scope_tags") || !strings.Contains(msg, `"owner:alice"`) {
		t.Fatalf("message must name the field and the first offending tag: %q", msg)
	}
	// The bare prefix is in the namespace too (IsOwnerTag is a prefix match).
	if err := RefuseOwnerTags("f", []string{OwnerTagPrefix}); err == nil {
		t.Fatal("bare owner: prefix must be refused")
	}
}
