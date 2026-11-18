package authz_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// noteEraser resolves memory-note entry ids to candidates the way
// postgres.PgErasureStore does (no writer of record, the document's tags).
type noteEraser struct {
	fakeRawEraser
	notes map[string][]string // entry id → tags
}

func (f *noteEraser) Resolve(_ context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	var out []domain.ErasureCandidate
	for _, id := range sel.MemoryNoteIDs {
		if tags, ok := f.notes[id]; ok {
			out = append(out, domain.ErasureCandidate{Kind: domain.ErasureTargetMemoryNote, ID: id, Via: id, Tags: tags})
		}
	}
	return out, nil
}

// ENG-2106: an agent erases a memory-notes entry only of its RUN PRINCIPAL's personal
// notes (the lease-derived person on ctx); another person's, org notes, and a call with
// no run principal are denied; an operator erases any; hidden stays absent.
func TestEnforcingEraser_MemoryNotesFollowTheRunPrincipal(t *testing.T) {
	alice := domain.MemoryNoteEntryID("personal", "alice", "Locker code: 1")
	bob := domain.MemoryNoteEntryID("personal", "bob", "Locker code: 2")
	org := domain.MemoryNoteEntryID("org", "acme", "Wifi: x")
	raw := &noteEraser{notes: map[string][]string{
		alice: {"memory-notes", "memory-notes:personal", domain.OwnerTag("alice")},
		bob:   {"memory-notes", "memory-notes:personal", domain.OwnerTag("bob")},
		org:   {"memory-notes", "memory-notes:org"},
	}}
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{AgentErase: true}, nil)
	sel := func(id string) domain.ErasureRequest {
		return eraseReq(domain.ErasureSelector{MemoryNoteIDs: []string{id}})
	}
	// The chat agent working alice's conversation: the read predicate holds her owner
	// tag (AllowAllAuthorizer's owner rule) and the run principal is alice.
	asAlice := domain.WithRunPrincipal(agentCtx("chat", &domain.TagPredicate{Bypass: true, Owners: []string{"alice"}}), "alice")

	if _, err := e.Erase(asAlice, sel(alice)); err != nil {
		t.Fatalf("alice's own note via her agent: %v", err)
	}
	if got := raw.erased[0]; !slices.Equal(got.Selector.MemoryNoteIDs, []string{alice}) || got.Principal != "chat" {
		t.Fatalf("erased = %+v", got)
	}
	// Bob's note is owner-fenced away from alice's predicate: absent, not denied.
	if _, err := e.Erase(asAlice, sel(bob)); !errors.Is(err, domain.ErrNothingToErase) {
		t.Fatalf("bob's note: want nothing to erase (hidden), got %v", err)
	}
	// Even a predicate that admits bob's row does not make bob's notes alice's.
	wide := domain.WithRunPrincipal(agentCtx("chat", domain.ScopeMaintenance), "alice")
	if _, err := e.Erase(wide, sel(bob)); !errors.Is(err, authz.ErrEraseDenied) {
		t.Fatalf("another person's notes: want denied, got %v", err)
	}
	if _, err := e.Erase(wide, sel(org)); !errors.Is(err, authz.ErrEraseDenied) {
		t.Fatalf("org notes by an agent: want denied, got %v", err)
	}
	noRun := agentCtx("chat", domain.ScopeMaintenance)
	if _, err := e.Erase(noRun, sel(alice)); !errors.Is(err, authz.ErrEraseDenied) {
		t.Fatalf("no run principal: want denied (fail closed), got %v", err)
	}
	n := len(raw.erased)
	op := domain.WithScope(domain.WithPrincipal(domain.WithPrincipalRoles(context.Background(), []string{"operator"}),
		domain.UserPrincipal("ops")), domain.ScopeMaintenance)
	if _, err := e.Erase(op, eraseReq(domain.ErasureSelector{MemoryNoteIDs: []string{bob, org}})); err != nil {
		t.Fatalf("operator: %v", err)
	}
	if len(raw.erased) != n+1 || raw.erased[n].PrincipalKind != domain.ErasureByOperator {
		t.Fatalf("operator erase = %+v", raw.erased)
	}
}
