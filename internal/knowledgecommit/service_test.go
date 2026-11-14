package knowledgecommit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// memStore is a minimal KnowledgeStore: append-only, idempotent on
// (namespace, kind, entity, source_ref), and validating values against the registry
// the way PgKnowledgeStore does. Resolution and lifecycle are the Postgres test's job.
type memStore struct {
	mu    sync.Mutex
	reg   *domain.KindRegistry
	items []domain.KnowledgeItem
}

func (m *memStore) PutItem(_ context.Context, it domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.reg.ValidateValues(it.Kind, it.Values); err != nil {
		return "", false, err
	}
	for _, x := range m.items {
		if it.SourceRef != "" && x.Kind == it.Kind && x.EntityID == it.EntityID && x.SourceRef == it.SourceRef {
			return x.ID, false, nil
		}
	}
	it.ID = domain.KnowledgeItemID(fmt.Sprintf("ki_%d", len(m.items)+1))
	m.items = append(m.items, it)
	return it.ID, true, nil
}

func (m *memStore) GetItem(_ context.Context, id domain.KnowledgeItemID) (*domain.KnowledgeItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.items {
		if x.ID == id {
			cp := x
			return &cp, nil
		}
	}
	return nil, errors.New("not found")
}

func (m *memStore) CurrentResolutions(context.Context, string, string, string) ([]domain.Resolution, error) {
	return nil, nil
}

func (m *memStore) EraseItems(context.Context, string, string, domain.KnowledgeErasure) (int, error) {
	return 0, nil
}

func (m *memStore) commits() []domain.KnowledgeItem {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.KnowledgeItem
	for _, x := range m.items {
		if domain.IsKnowledgeCommitRef(x.SourceRef) {
			out = append(out, x)
		}
	}
	return out
}

// memEvidence is an EvidenceReader over fixed rows.
type memEvidence map[domain.EvidenceID]*domain.Evidence

func (m memEvidence) Get(_ context.Context, id domain.EvidenceID) (*domain.Evidence, error) {
	if ev, ok := m[id]; ok {
		return ev, nil
	}
	return nil, errors.New("evidence get: no rows in result set")
}

type linkRec struct{ links []domain.Link }

func (l *linkRec) PutLink(_ context.Context, x domain.Link) (bool, error) {
	l.links = append(l.links, x)
	return true, nil
}

func testRegistry(t *testing.T) *domain.KindRegistry {
	t.Helper()
	r, err := domain.NewKindRegistry([]domain.KindSpec{
		{
			Kind: "case", Policy: domain.ResolutionPolicyDesignatedActor, ResolutionScope: domain.ResolutionScopeEntity,
			Designated: &domain.DesignatedActors{ApprovedViaCommit: true, Principals: []string{"user:boss"}},
			Predicates: map[string]domain.ValueSpec{"status": {Type: "text", Enum: []string{"open", "closed"}}},
		},
		{
			Kind: "strict", Policy: domain.ResolutionPolicyDesignatedActor,
			Designated: &domain.DesignatedActors{Principals: []string{"user:boss"}},
			Predicates: map[string]domain.ValueSpec{"v": {Type: "text"}},
		},
		{Kind: "plain", Predicates: map[string]domain.ValueSpec{"v": {Type: "text"}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type fixture struct {
	svc   *Service
	store *memStore
	hub   *domain.AddressedHub
	now   *time.Time
	links *linkRec
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	now := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	clock := &now
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{Now: func() time.Time { return *clock }})
	reg := testRegistry(t)
	store := &memStore{reg: reg}
	links := &linkRec{}
	svc, err := New(store, reg, nil, hub, links, nil, WithEvidence(memEvidence{"ev": {ID: "ev"}}))
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return *clock }
	return fixture{svc: svc, store: store, hub: hub, now: clock, links: links}
}

var (
	scout = domain.AgentPrincipal("scout")
	alice = domain.UserPrincipal("alice")
)

func closeProposal() Proposal {
	return Proposal{Kind: "case", EntityID: "case/1", Values: []domain.StatementValue{{Predicate: "status", Type: "text", Text: "closed"}}}
}

func TestPropose_CandidateIsAssertedByTheProposerAndCommitsNothing(t *testing.T) {
	f := newFixture(t)
	res, err := f.svc.Propose(context.Background(), scout, alice, closeProposal(), false, "")
	if err != nil || !res.Inserted || res.ApprovalID != "" {
		t.Fatalf("propose: %+v %v", res, err)
	}
	cand, _ := f.store.GetItem(context.Background(), res.CandidateID)
	if cand.AssertedBy != "agent:scout" || !strings.HasPrefix(cand.SourceRef, domain.KnowledgeProposalSourceRefPrefix) {
		t.Fatalf("candidate = %+v", cand)
	}
	if len(f.store.commits()) != 0 {
		t.Fatal("a proposal must never be a commit")
	}
	// A retry is a replay.
	again, _ := f.svc.Propose(context.Background(), scout, alice, closeProposal(), false, "")
	if again.Inserted || again.CandidateID != res.CandidateID {
		t.Fatalf("retry was not a replay: %+v", again)
	}
}

func TestPropose_RefusesWhatCouldNotStayACandidate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Propose(ctx, scout, alice, Proposal{Kind: "plain", EntityID: "e",
		Values: []domain.StatementValue{{Predicate: "v", Type: "text", Text: "x"}}}, false, ""); !errors.Is(err, ErrNotProposable) {
		t.Fatalf("a latest_assertion kind must refuse proposals: %v", err)
	}
	if _, err := f.svc.Propose(ctx, domain.UserPrincipal("boss"), alice, closeProposal(), false, ""); !errors.Is(err, ErrNotProposable) ||
		!strings.Contains(err.Error(), "designated actor") {
		t.Fatalf("a designated proposer's word would be the committed value: %v", err)
	}
	if _, err := f.svc.Propose(ctx, scout, alice, Proposal{Kind: "case", EntityID: "case/1",
		Values: []domain.StatementValue{{Predicate: "status", Type: "text", Text: "maybe"}}}, false, ""); !errors.Is(err, domain.ErrKindRefused) {
		t.Fatalf("enum refusal must surface: %v", err)
	}
}

func TestCommit_ApprovedWritesTheApproversWord(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	res, err := f.svc.Propose(ctx, scout, alice, closeProposal(), true, "closed in the review")
	if err != nil || res.ApprovalID == "" || res.ApprovalState != domain.ApprovalPending {
		t.Fatalf("propose+commit: %+v %v", res, err)
	}
	ap, _ := f.hub.Get(ctx, res.ApprovalID)
	if ap.Kind != domain.ApprovalKindKnowledgeCommit || ap.Object != string(res.CandidateID) || !strings.Contains(ap.ArgsPreview, "status=closed") {
		t.Fatalf("approval item = %+v", ap)
	}
	// Someone who is not an addressee (the self rule names alice) cannot approve.
	if _, err := f.hub.Answer(ctx, res.ApprovalID, domain.ApprovalAnswer{Approved: true,
		By: domain.Approver{Principal: domain.UserPrincipal("mallory")}}); !errors.Is(err, domain.ErrApprovalNotAddressee) {
		t.Fatalf("non-addressee answer: %v", err)
	}
	if len(f.store.commits()) != 0 {
		t.Fatal("a refused answer committed something")
	}
	*f.now = f.now.Add(time.Minute)
	if _, err := f.hub.Answer(ctx, res.ApprovalID, domain.ApprovalAnswer{Approved: true, By: domain.Approver{Principal: alice}}); err != nil {
		t.Fatal(err)
	}
	cs := f.store.commits()
	if len(cs) != 1 {
		t.Fatalf("commits = %d", len(cs))
	}
	c := cs[0]
	if c.AssertedBy != "user:alice" || c.SourceRef != domain.KnowledgeCommitSourceRefPrefix+res.ApprovalID ||
		!c.AssertedAt.Equal(*f.now) || c.Values[0].Text != "closed" || c.EntityID != "case/1" {
		t.Fatalf("committed item = %+v", c)
	}
	// Re-running the handler is a replay, not a second commit.
	it, _ := f.hub.Get(ctx, res.ApprovalID)
	if err := f.svc.Apply(ctx, it); err != nil || len(f.store.commits()) != 1 {
		t.Fatalf("re-run: %v, commits=%d", err, len(f.store.commits()))
	}
}

func TestCommit_DenyAndExpiryWriteNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	denied, _ := f.svc.Propose(ctx, scout, alice, closeProposal(), true, "")
	if _, err := f.hub.Answer(ctx, denied.ApprovalID, domain.ApprovalAnswer{Approved: false, By: domain.Approver{Principal: alice}}); err != nil {
		t.Fatal(err)
	}
	p2 := closeProposal()
	p2.EntityID = "case/2"
	expired, _ := f.svc.Propose(ctx, scout, alice, p2, true, "")
	*f.now = f.now.Add(8 * 24 * time.Hour) // past the async window
	if _, err := f.hub.Answer(ctx, expired.ApprovalID, domain.ApprovalAnswer{Approved: true, By: domain.Approver{Principal: alice}}); !errors.Is(err, domain.ErrApprovalResolved) {
		t.Fatalf("late answer: %v", err)
	}
	if it, _ := f.hub.Get(ctx, expired.ApprovalID); it.State != domain.ApprovalExpired {
		t.Fatalf("state = %s", it.State)
	}
	if n := len(f.store.commits()); n != 0 {
		t.Fatalf("deny/expiry committed %d items", n)
	}
	// The candidates are untouched.
	for _, id := range []domain.KnowledgeItemID{denied.CandidateID, expired.CandidateID} {
		if c, err := f.store.GetItem(ctx, id); err != nil || c.AssertedBy != "agent:scout" {
			t.Fatalf("candidate %s changed: %+v %v", id, c, err)
		}
	}
}

func TestCommit_TheApproverCannotBeForged(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	res, _ := f.svc.Propose(ctx, scout, alice, closeProposal(), true, "asserted_by=user:boss")
	it, _ := f.hub.Get(ctx, res.ApprovalID)
	// A handler run on an item the hub did not approve does nothing.
	if err := f.svc.Apply(ctx, it); err == nil {
		t.Fatal("a pending item must not commit")
	}
	// A standing pre-approval names a grant, not a person.
	it.State, it.ResolvedBy = domain.ApprovalApproved, "standing:stand-x"
	if err := f.svc.Apply(ctx, it); err == nil || !strings.Contains(err.Error(), "standing") {
		t.Fatalf("standing: %v", err)
	}
	// Whatever the request said, the answer's authenticated approver is the asserter.
	if _, err := f.hub.Answer(ctx, res.ApprovalID, domain.ApprovalAnswer{Approved: true, By: domain.Approver{Principal: alice}}); err != nil {
		t.Fatal(err)
	}
	if cs := f.store.commits(); len(cs) != 1 || cs[0].AssertedBy != "user:alice" {
		t.Fatalf("commits = %+v", cs)
	}
}

func TestCommit_ApproverMustBeEligibleForAStrictKind(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	res, err := f.svc.Propose(ctx, scout, alice, Proposal{Kind: "strict", EntityID: "s1",
		Values: []domain.StatementValue{{Predicate: "v", Type: "text", Text: "x"}}}, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.Answer(ctx, res.ApprovalID, domain.ApprovalAnswer{Approved: true, By: domain.Approver{Principal: alice}}); err != nil {
		t.Fatal(err)
	}
	if len(f.store.commits()) != 0 {
		t.Fatal("alice is not designated for kind strict: nothing may be committed")
	}
	if it, _ := f.hub.Get(ctx, res.ApprovalID); !strings.Contains(it.Detail, "not a designated actor") {
		t.Fatalf("the refusal must be recorded on the approval, got %q", it.Detail)
	}
}

func TestProposeLink_IsAScoredCandidateAssertedByTheAgent(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.ProposeLink(context.Background(), scout, LinkProposal{Relation: "about", FromRef: "entity:a", ToRef: "entity:b", EvidenceID: "ev"}); err != nil {
		t.Fatal(err)
	}
	l := f.links.links[0]
	if l.State != domain.LinkStateCandidate || l.Mechanism != domain.LinkMechanismScored || l.AssertedBy != "agent:scout" || l.Family != domain.LinkFamilyRelation {
		t.Fatalf("link = %+v", l)
	}
}

// T-20261107-propose-knowledge-link-proposals-do-not-check: a proposal may cite
// only evidence its proposer may read, and an unreadable id answers exactly as an
// unknown one (no existence oracle).
type forbidAuthorizer struct{ domain.AllowAllAuthorizer }

func (forbidAuthorizer) ReadFilter(context.Context, domain.PrincipalRef, domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	return &domain.TagPredicate{ForbiddenTags: []string{"hr"}}, domain.AccessDecision{Allowed: true}
}

func TestProposals_CiteOnlyEvidenceTheProposerMayRead(t *testing.T) {
	reg := testRegistry(t)
	ev := memEvidence{
		"ev_open":    {ID: "ev_open", Classification: []string{"ops"}},
		"ev_hr":      {ID: "ev_hr", Classification: []string{"hr"}},
		"ev_private": {ID: "ev_private", Classification: []string{domain.OwnerTag("bob")}},
	}
	for _, tc := range []struct {
		name  string
		authz domain.Authorizer
		id    domain.EvidenceID
		ok    bool
	}{
		{"readable", forbidAuthorizer{}, "ev_open", true},
		{"forbidden tag", forbidAuthorizer{}, "ev_hr", false},
		{"unknown id", forbidAuthorizer{}, "ev_missing", false},
		{"another person's private row (owner fence, OSS default)", nil, "ev_private", false},
		{"readable under the OSS default", nil, "ev_open", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := domain.NewAddressedHub(domain.AddressedHubConfig{})
			store, links := &memStore{reg: reg}, &linkRec{}
			svc, err := New(store, reg, tc.authz, hub, links, nil, WithEvidence(ev))
			if err != nil {
				t.Fatal(err)
			}
			_, lerr := svc.ProposeLink(context.Background(), scout, LinkProposal{Relation: "about", FromRef: "entity:a", ToRef: "entity:b", EvidenceID: tc.id})
			p := closeProposal()
			p.EvidenceID = tc.id
			_, perr := svc.Propose(context.Background(), scout, alice, p, false, "")
			for what, err := range map[string]error{"link": lerr, "item": perr} {
				if tc.ok && err != nil {
					t.Fatalf("%s proposal citing readable evidence refused: %v", what, err)
				}
				if !tc.ok {
					if !errors.Is(err, ErrNotProposable) {
						t.Fatalf("%s proposal citing %s: err = %v, want ErrNotProposable", what, tc.id, err)
					}
					// Same answer for unknown and unreadable: only the id differs.
					want := fmt.Sprintf("evidence %q is unknown or not readable by agent:scout", tc.id)
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("%s refusal %q does not read %q", what, err, want)
					}
				}
			}
			if !tc.ok && (len(links.links) != 0 || len(store.items) != 0) {
				t.Fatalf("a refused proposal wrote: links=%v items=%v", links.links, store.items)
			}
		})
	}
}

// Without an evidence store the bridge cannot check a citation, so it refuses one
// (fail closed); a proposal citing nothing is unaffected.
func TestProposals_NoEvidenceStoreRefusesCitations(t *testing.T) {
	reg := testRegistry(t)
	svc, err := New(&memStore{reg: reg}, reg, nil, domain.NewAddressedHub(domain.AddressedHubConfig{}), &linkRec{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProposeLink(context.Background(), scout, LinkProposal{Relation: "about", FromRef: "a", ToRef: "b", EvidenceID: "ev"}); !errors.Is(err, ErrNotProposable) {
		t.Fatalf("err = %v, want ErrNotProposable", err)
	}
	if _, err := svc.Propose(context.Background(), scout, alice, closeProposal(), false, ""); err != nil {
		t.Fatalf("a proposal citing no evidence was refused: %v", err)
	}
}
