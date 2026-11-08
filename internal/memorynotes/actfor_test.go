package memorynotes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261107-a-person-s-assistant-cannot-save-that-person-s: in a conversation an
// operator opened ON BEHALF OF a person, the assistant's propose_memory_note lands
// in the OWNER's personal notes, is decided under the owner's principal (never the
// operator's), is owner-scoped so the operator cannot read it through their own
// principal, and the writer of record names both.

// fenceNotes is a decision point that applies the owner fence the way the premium
// Authorizer does (TagPredicate.Check with Owners = the reader) and records every
// request, so the test can see WHO each decision was asked for.
type fenceNotes struct {
	domain.AllowAllAuthorizer
	asked *[]domain.AccessRequest
}

func (f fenceNotes) Authorize(_ context.Context, req domain.AccessRequest) domain.AccessDecision {
	*f.asked = append(*f.asked, req)
	pred := &domain.TagPredicate{}
	if req.Principal.Kind == domain.PrincipalUser {
		pred.Owners = []string{req.Principal.ID}
	}
	d, ok := pred.Check(req.Tags)
	d.Allowed = ok
	return d
}

func TestActFor_NoteLandsOnOwnerAndNamesTheOperator(t *testing.T) {
	var asked []domain.AccessRequest
	st := NewMemStore()
	svc, err := New(st, flagIgnore, fenceNotes{asked: &asked}, Config{PersonalBudgetChars: 500, OrgBudgetChars: 500})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// What notesCaller builds for an act-for chat turn: the run principal is the
	// conversation owner, the lease names the operator who opened the turn.
	run := Principal{ID: "dwhitfield", AgentID: "chat_agent", ActedBy: "operator"}

	res, err := svc.Propose(ctx, run, Proposal{Op: OpAppend, Section: "Preferences", Content: "call me Dan"})
	if err != nil || !res.Accepted {
		t.Fatalf("owner's own note refused: %+v %v", res, err)
	}

	// The subject is the owner: the write landed in personal:dwhitfield, and the
	// decision was asked for user:dwhitfield, never for the operator.
	if d, _ := st.Get(ctx, Key{Scope: ScopePersonal, OwnerID: "dwhitfield"}); !d.Exists() || !strings.Contains(d.Content, "call me Dan") {
		t.Fatalf("note not in the owner's document: %+v", d)
	}
	if d, _ := st.Get(ctx, Key{Scope: ScopePersonal, OwnerID: "operator"}); d.Exists() {
		t.Fatal("the note landed in the operator's document")
	}
	for _, r := range asked {
		if r.Principal != domain.UserPrincipal("dwhitfield") {
			t.Fatalf("a notes decision was asked for %s, not the owner", r.Principal)
		}
	}
	// The personal document is owner-scoped (ADR-0142 D12).
	if last := asked[len(asked)-1]; !containsTag(last.Tags, domain.OwnerTag("dwhitfield")) {
		t.Fatalf("personal notes resource is not owner-scoped: %v", last.Tags)
	}

	// The audit names both: the owner as subject, the operator as the human behind it.
	h := st.History()
	actor := h[len(h)-1].Actor
	if !strings.Contains(actor, "for dwhitfield") || !strings.Contains(actor, "operator:operator") {
		t.Fatalf("writer of record must name owner and operator: %q", actor)
	}

	// The operator, through their OWN principal, cannot read the owner's note.
	if _, err := svc.Get(ctx, Caller{ID: "operator"}, Key{Scope: ScopePersonal, OwnerID: "dwhitfield"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("operator read the owner-only note: %v", err)
	}
	if l, _ := svc.List(ctx, Caller{ID: "operator"}, ListFilter{}); len(l) != 0 {
		t.Fatalf("operator listed the owner-only note: %+v", l)
	}
	// Nor does a run of the operator's own see it.
	snap, _ := svc.Snapshot(ctx, "op-run", Principal{ID: "operator", AgentID: "chat_agent"})
	if len(snap.Documents) != 0 {
		t.Fatalf("operator's run sees the owner's notes: %+v", snap.Documents)
	}
	// The owner reads it back on their next run.
	snap, _ = svc.Snapshot(ctx, "owner-run", Principal{ID: "dwhitfield", AgentID: "chat_agent"})
	if len(snap.Documents) != 1 || !strings.Contains(snap.Documents[0].Content, "call me Dan") {
		t.Fatalf("owner cannot read their note back: %+v", snap.Documents)
	}
}

// Administrative operator mutations stay possible on a person's document (audited
// acts; reading it is what the fence forbids).
func TestActFor_AdminMutationsAreNotOwnerFenced(t *testing.T) {
	var asked []domain.AccessRequest
	st := NewMemStore()
	svc, _ := New(st, flagIgnore, fenceNotes{asked: &asked}, Config{PersonalBudgetChars: 500, OrgBudgetChars: 500})
	ctx := context.Background()
	key := Key{Scope: ScopePersonal, OwnerID: "dwhitfield"}
	if err := svc.SetBudget(ctx, Caller{ID: "operator"}, ScopePersonal, "dwhitfield", 300, false); err != nil {
		t.Fatalf("budget: %v", err)
	}
	if r, err := svc.Set(ctx, Caller{ID: "operator"}, key, "reset", "", false, "moderation"); err != nil || !r.Accepted {
		t.Fatalf("set: %+v %v", r, err)
	}
}

func TestWriterOfRecord(t *testing.T) {
	cases := map[string]Principal{
		"agent:chat_agent for dwhitfield (on behalf, by operator:operator)": {ID: "dwhitfield", AgentID: "chat_agent", ActedBy: "operator"},
		"agent:chat_agent for dwhitfield":                                   {ID: "dwhitfield", AgentID: "chat_agent"},
		// Acting as oneself is not act-for.
		"agent:chat_agent for operator": {ID: "operator", AgentID: "chat_agent", ActedBy: "operator"},
	}
	for want, p := range cases {
		if got := p.writerOfRecord(); got != want {
			t.Errorf("writerOfRecord(%+v) = %q, want %q", p, got, want)
		}
	}
}

func containsTag(tags []string, t string) bool {
	for _, x := range tags {
		if x == t {
			return true
		}
	}
	return false
}
