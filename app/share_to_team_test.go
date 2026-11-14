package app

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

type fakeRetagger struct{ tags map[string][]string }

func (f *fakeRetagger) DocumentTags(_ context.Context, id string) ([]string, bool, error) {
	t, ok := f.tags[id]
	return t, ok, nil
}
func (f *fakeRetagger) ParentDocumentOf(_ context.Context, id string) (string, bool, error) {
	if id == "chunk-1" {
		return "doc-1", true, nil
	}
	return "", false, nil
}
func (f *fakeRetagger) RetagDocument(_ context.Context, id string, tags []string) error {
	f.tags[id] = tags
	return nil
}

// ADR-0142 D12: only the owner publishes; the owner tag is replaced by the
// target classification; another owner's fence on co-derived material stays.
func TestMemorySharer_OwnerOnlyAndKeepsOtherFences(t *testing.T) {
	r := &fakeRetagger{tags: map[string][]string{
		"doc-1": {"owner:alice", "provenance:source=chat_agent"},
		"doc-2": {"owner:alice", "owner:bob"},
	}}
	s := &memorySharer{docs: r, authz: domain.AllowAllAuthorizer{}}
	ctx := context.Background()
	if _, err := s.ApplyShare(ctx, domain.UserPrincipal("bob"), "doc-1", []string{"team_kb"}); !errors.Is(err, operator.ErrShareNotOwner) {
		t.Fatalf("bob shared alice's document: %v", err)
	}
	if _, err := s.ApplyShare(ctx, domain.UserPrincipal("alice"), "doc-1", []string{"owner:carol"}); !errors.Is(err, operator.ErrShareNotPermitted) {
		t.Fatalf("an owner tag is never a share target: %v", err)
	}
	got, err := s.ApplyShare(ctx, domain.UserPrincipal("alice"), "chunk-1", []string{"team_kb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(domain.OwnersOf(got)) != 0 || !hasString(r.tags["doc-1"], "team_kb") {
		t.Fatalf("shared tags = %v", r.tags["doc-1"])
	}
	got, _ = s.ApplyShare(ctx, domain.UserPrincipal("alice"), "doc-2", []string{"team_kb"})
	if o := domain.OwnersOf(got); len(o) != 1 || o[0] != "bob" {
		t.Fatalf("bob's fence must survive alice's share: %v", got)
	}
}

func hasString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// stampingAuthorizer stamps the private-write owner the way the premium decision
// point does (ADR-0142 D12), so a leaked marker would show up in the result.
type stampingAuthorizer struct{ domain.AllowAllAuthorizer }

func (stampingAuthorizer) ClassifyWrite(ctx context.Context, p domain.PrincipalRef, hint []string) ([]string, domain.AccessDecision) {
	return domain.StampPrivateOwner(ctx, hint), domain.AccessDecision{Allowed: true, Principal: p}
}

// ENG-1268: the share is classified as the PERSON, never under a private-write
// marker the incoming ctx may carry. WithPrivateWrites(ctx, "") left the marker
// in place, so a marked ctx re-fenced the published document to that owner.
func TestMemorySharer_ClearsPrivateWriteMarker(t *testing.T) {
	r := &fakeRetagger{tags: map[string][]string{"doc-1": {"owner:alice"}}}
	s := &memorySharer{docs: r, authz: stampingAuthorizer{}}
	ctx := domain.WithPrivateWrites(context.Background(), "alice")
	got, err := s.ApplyShare(ctx, domain.UserPrincipal("alice"), "doc-1", []string{"team_kb"})
	if err != nil {
		t.Fatal(err)
	}
	if o := domain.OwnersOf(got); len(o) != 0 {
		t.Fatalf("published document still owner-fenced: %v", got)
	}
}
