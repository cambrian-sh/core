package ontology

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

// Unit tests for the draft → approval → apply bridge, with no database: an
// in-memory draft store holding the same append-only rules as migration 0041,
// a fake hub, and an applier that classifies with the real classifier.

type memDraftStore struct {
	mu     sync.Mutex
	drafts map[string]domain.OntologyDraft
	events map[string][]draftEvent
}

func newMemDraftStore() *memDraftStore {
	return &memDraftStore{drafts: map[string]domain.OntologyDraft{}, events: map[string][]draftEvent{}}
}

func (s *memDraftStore) insertDraft(_ context.Context, d domain.OntologyDraft) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.drafts[d.ID]; ok {
		return false, nil
	}
	s.drafts[d.ID] = d
	return true, nil
}

func (s *memDraftStore) fold(id string) (domain.OntologyDraft, bool) {
	d, ok := s.drafts[id]
	if !ok {
		return d, false
	}
	d.State, d.ApprovalID = domain.OntologyDraftPending, ""
	for _, e := range s.events[id] {
		if e.State == domain.OntologyDraftPending {
			d.ApprovalID, d.StateBy, d.StateAt = e.ApprovalID, e.Actor, e.At
		}
	}
	for _, e := range s.events[id] {
		if e.State != domain.OntologyDraftPending {
			d.State, d.StateBy, d.Detail, d.AppliedVersion, d.StateAt = e.State, e.Actor, e.Detail, e.AppliedVersion, e.At
			if d.ApprovalID == "" {
				d.ApprovalID = e.ApprovalID
			}
		}
	}
	return d, true
}

func (s *memDraftStore) getDraft(_ context.Context, id string) (domain.OntologyDraft, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.fold(id)
	return d, ok, nil
}

func (s *memDraftStore) appendEvent(_ context.Context, e draftEvent) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.drafts[e.DraftID]; !ok {
		return false, errors.New("foreign key: no draft " + e.DraftID)
	}
	for _, x := range s.events[e.DraftID] {
		if x.State == e.State || (x.State != domain.OntologyDraftPending && e.State != domain.OntologyDraftPending) {
			return false, nil
		}
	}
	s.events[e.DraftID] = append(s.events[e.DraftID], e)
	return true, nil
}

func (s *memDraftStore) awaiting(_ context.Context) ([]domain.OntologyDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.OntologyDraft
	for id := range s.drafts {
		if d, _ := s.fold(id); d.State == domain.OntologyDraftPending && d.ApprovalID != "" {
			out = append(out, d)
		}
	}
	return out, nil
}

// fakeHub raises pending items (or fails them closed with failState) and
// answers them the way AddressedHub.Answer does: record, then run the handler
// for an approved async item.
type fakeHub struct {
	mu        sync.Mutex
	raised    []domain.ApprovalRequest
	items     map[string]domain.ApprovalItem
	handlers  map[string]domain.ApprovalHandler
	failState domain.ApprovalState
	lastErr   error
}

func newFakeHub() *fakeHub {
	return &fakeHub{items: map[string]domain.ApprovalItem{}, handlers: map[string]domain.ApprovalHandler{}}
}

func (h *fakeHub) Raise(_ context.Context, req domain.ApprovalRequest) (domain.ApprovalItem, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.raised = append(h.raised, req)
	it := domain.ApprovalItem{ID: "ap-" + req.Object, Kind: req.Kind, Object: req.Object, State: domain.ApprovalPending, Async: true}
	if h.failState != "" {
		it.State, it.Detail = h.failState, "no approval rule covers action class "+req.ActionClass
	}
	h.items[it.ID] = it
	return it, nil
}

func (h *fakeHub) HandleKind(kind string, fn domain.ApprovalHandler) {
	h.mu.Lock()
	h.handlers[kind] = fn
	h.mu.Unlock()
}

func (h *fakeHub) Get(_ context.Context, id string) (domain.ApprovalItem, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	it, ok := h.items[id]
	if !ok {
		return it, domain.ErrApprovalNotFound
	}
	return it, nil
}

func (h *fakeHub) resolve(t *testing.T, id string, state domain.ApprovalState, by string) {
	t.Helper()
	h.mu.Lock()
	it := h.items[id]
	it.State, it.ResolvedBy, it.ResolvedAt = state, by, time.Now()
	h.items[id] = it
	fn := h.handlers[it.Kind]
	h.mu.Unlock()
	if state == domain.ApprovalApproved && fn != nil {
		h.lastErr = fn(context.Background(), it)
	}
}

// fakeApplier classifies with the real Ontology (memOntology) and "writes" by
// swapping its snapshot; apply runs the check exactly where the real body does.
type fakeApplier struct {
	o       *Ontology
	mu      sync.Mutex
	version int64
	authors []string
}

func newFakeApplier(t *testing.T) *fakeApplier {
	return &fakeApplier{o: memOntology(t), version: 1}
}

func (f *fakeApplier) Apply(ctx context.Context, ns string, change domain.OntologyDoc, author string, dryRun bool) (domain.OntologyApplyResult, error) {
	return f.apply(ctx, ns, change, author, dryRun, nil)
}

func (f *fakeApplier) apply(ctx context.Context, ns string, change domain.OntologyDoc, author string, dryRun bool, check applyCheck) (domain.OntologyApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res, err := f.o.apply(ctx, ns, change, author, true, check) // the real classification, refusal and check
	if err != nil || dryRun || len(res.Changes) == 0 {
		return res, err
	}
	f.version++
	snap, err := domain.NewOntologySnapshot(orDefault(ns), f.version, f.o.Current(ns).Doc.Overlay(change), authorities())
	if err != nil {
		return res, err
	}
	f.o.swap(orDefault(ns), snap)
	f.authors = append(f.authors, author)
	res.Version, res.Applied = f.version, true
	return res, nil
}

type auditLog struct {
	mu      sync.Mutex
	entries []domain.AuditEntry
}

func (a *auditLog) record(_ context.Context, e domain.AuditEntry) error {
	a.mu.Lock()
	a.entries = append(a.entries, e)
	a.mu.Unlock()
	return nil
}

func (a *auditLog) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.entries {
		out = append(out, e.ActionType+"@"+e.Actor)
	}
	return out
}

type draftRig struct {
	d     *Drafts
	store *memDraftStore
	hub   *fakeHub
	onto  *fakeApplier
	audit *auditLog
}

func newDraftRig(t *testing.T) draftRig {
	t.Helper()
	r := draftRig{store: newMemDraftStore(), hub: newFakeHub(), onto: newFakeApplier(t), audit: &auditLog{}}
	r.d = newDrafts(r.onto, r.store, r.hub, r.audit.record, nil)
	return r
}

var (
	agentP = domain.AgentPrincipal("planner")
	aliceP = domain.UserPrincipal("alice")
)

func invoiceChange() domain.OntologyDoc {
	return domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "invoice",
		Predicates: map[string]domain.ValueSpec{"amount": {Type: "number"}}}}}
}

func TestDrafts_ProposeRecordsTheDraftAndRaisesAnApprovalNamingIt(t *testing.T) {
	t.Parallel()
	r := newDraftRig(t)
	res, err := r.d.Propose(context.Background(), agentP, aliceP, DraftProposal{Namespace: "ns1", Change: invoiceChange(), Reason: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	dr := res.Draft
	if !res.Created || dr.State != domain.OntologyDraftPending || dr.Proposer != "agent:planner" || dr.BaseVersion != 1 ||
		dr.Class != domain.ChangeAdditive || len(dr.Changes) != 1 || dr.ApprovalID == "" {
		t.Fatalf("draft: %+v", res)
	}
	if len(r.hub.raised) != 1 {
		t.Fatalf("one approval raised, got %d", len(r.hub.raised))
	}
	req := r.hub.raised[0]
	if req.Kind != domain.ApprovalKindOntologyChange || req.ActionClass != domain.ApprovalKindOntologyChange ||
		req.Object != dr.ID || req.AgentID != "planner" || req.Beneficiary != aliceP || req.Resource != "ontology/ns1" ||
		!strings.Contains(req.ArgsPreview, "kind invoice") || !strings.Contains(req.ArgsPreview, "additive") ||
		!strings.Contains(req.ArgsPreview, "agent:planner") || !strings.Contains(req.ArgsPreview, "billing") {
		t.Fatalf("approval request: %+v", req)
	}
	if got := r.audit.actions(); len(got) != 1 || got[0] != "ontology_draft_pending@agent:planner" {
		t.Fatalf("audit: %v", got)
	}
	if len(r.onto.authors) != 0 {
		t.Fatal("drafting must apply nothing")
	}
}

func TestDrafts_ARetriedDraftIsTheSameDraftAndOneApproval(t *testing.T) {
	t.Parallel()
	r := newDraftRig(t)
	ctx := context.Background()
	first, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Change: invoiceChange()})
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Change: invoiceChange()})
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.Draft.ID != first.Draft.ID || again.Draft.ApprovalID != first.Draft.ApprovalID ||
		again.ApprovalState != domain.ApprovalPending || len(r.hub.raised) != 1 || len(r.store.drafts) != 1 {
		t.Fatalf("retry: %+v (raised %d, drafts %d)", again, len(r.hub.raised), len(r.store.drafts))
	}
	// A new client ref is a new draft.
	other, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Change: invoiceChange(), ClientRef: "r2"})
	if err != nil || other.Draft.ID == first.Draft.ID || !other.Created {
		t.Fatalf("new ref: %+v %v", other, err)
	}
}

func TestDrafts_BreakingInvalidOrEmptyIsRefusedAtDraftTime(t *testing.T) {
	t.Parallel()
	r := newDraftRig(t)
	ctx := context.Background()
	for name, change := range map[string]domain.OntologyDoc{
		"breaking": {Kinds: []domain.KindSpec{{Kind: "order", Predicates: map[string]domain.ValueSpec{
			"status": {Type: "number"}, "total": {Type: "number"}}}}},
		"invalid": {Kinds: []domain.KindSpec{{Kind: "invoice", Predicates: map[string]domain.ValueSpec{
			"n": {Type: "number", Enum: []string{"a"}}}}}},
		"empty": seedDoc(), // declares exactly what is there
	} {
		_, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: "ns1", Change: change})
		if !errors.Is(err, domain.ErrOntologyDraftRefused) {
			t.Fatalf("%s: want a refusal, got %v", name, err)
		}
		if name == "breaking" && !strings.Contains(err.Error(), "kind order.status: value type text -> number") {
			t.Fatalf("the refusal must carry Apply's reason: %v", err)
		}
	}
	if len(r.store.drafts) != 0 || len(r.hub.raised) != 0 {
		t.Fatalf("a refused draft records nothing and raises nothing (%d drafts, %d raised)", len(r.store.drafts), len(r.hub.raised))
	}
	if _, err := r.d.Propose(ctx, domain.PrincipalRef{}, aliceP, DraftProposal{Change: invoiceChange()}); !errors.Is(err, domain.ErrOntologyDraftRefused) {
		t.Fatalf("no proposer: %v", err)
	}
}

func TestDrafts_ApproveAppliesAsTheApproverNeverTheAgent(t *testing.T) {
	t.Parallel()
	r := newDraftRig(t)
	ctx := context.Background()
	res, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: "ns1", Change: invoiceChange()})
	if err != nil {
		t.Fatal(err)
	}
	r.hub.resolve(t, res.Draft.ApprovalID, domain.ApprovalApproved, "bob")
	if r.hub.lastErr != nil {
		t.Fatal(r.hub.lastErr)
	}
	if len(r.onto.authors) != 1 || r.onto.authors[0] != "user:bob" {
		t.Fatalf("applied as %v, want [user:bob]", r.onto.authors)
	}
	if _, ok := r.onto.o.Current("ns1").Kinds.Spec("invoice"); !ok {
		t.Fatal("the change must be live after approval")
	}
	dr, _, _ := r.store.getDraft(ctx, res.Draft.ID)
	if dr.State != domain.OntologyDraftApplied || dr.StateBy != "user:bob" || dr.AppliedVersion != 2 {
		t.Fatalf("draft after approval: %+v", dr)
	}
	// A re-run handler is a replay: nothing applied twice.
	r.hub.resolve(t, res.Draft.ApprovalID, domain.ApprovalApproved, "bob")
	if r.hub.lastErr != nil || len(r.onto.authors) != 1 {
		t.Fatalf("replay applied again (%v, %v)", r.hub.lastErr, r.onto.authors)
	}
	if got := r.audit.actions(); fmt.Sprint(got) != "[ontology_draft_pending@agent:planner ontology_draft_applied@user:bob]" {
		t.Fatalf("audit: %v", got)
	}
}

func TestDrafts_StandingApprovalAppliesNothing(t *testing.T) {
	t.Parallel()
	r := newDraftRig(t)
	res, err := r.d.Propose(context.Background(), agentP, aliceP, DraftProposal{Change: invoiceChange()})
	if err != nil {
		t.Fatal(err)
	}
	r.hub.resolve(t, res.Draft.ApprovalID, domain.ApprovalApproved, "standing:sa_1")
	if r.hub.lastErr == nil || !strings.Contains(r.hub.lastErr.Error(), "needs an answer") || len(r.onto.authors) != 0 {
		t.Fatalf("standing: %v, applied %v", r.hub.lastErr, r.onto.authors)
	}
}

func TestDrafts_DenyAndExpiryWriteNoDeclaration(t *testing.T) {
	t.Parallel()
	r := newDraftRig(t)
	ctx := context.Background()
	denied, _ := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Change: invoiceChange()})
	expired, _ := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Change: invoiceChange(), ClientRef: "x"})
	r.hub.resolve(t, denied.Draft.ApprovalID, domain.ApprovalDenied, "bob")
	r.hub.resolve(t, expired.Draft.ApprovalID, domain.ApprovalExpired, "")
	if err := r.d.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.onto.authors) != 0 {
		t.Fatal("deny/expiry applied a change")
	}
	d1, _, _ := r.store.getDraft(ctx, denied.Draft.ID)
	d2, _, _ := r.store.getDraft(ctx, expired.Draft.ID)
	if d1.State != domain.OntologyDraftRejected || d1.StateBy != "bob" || d2.State != domain.OntologyDraftExpired || d2.StateBy != "kernel" {
		t.Fatalf("states: %+v / %+v", d1, d2)
	}
	// Reconcile is idempotent; a late approval of a decided draft applies nothing.
	if err := r.d.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	r.hub.resolve(t, denied.Draft.ApprovalID, domain.ApprovalApproved, "bob")
	if r.hub.lastErr == nil || len(r.onto.authors) != 0 {
		t.Fatalf("a decided draft applied (%v)", r.hub.lastErr)
	}
	// Re-proposing the decided draft says so; a new ref asks anew.
	again, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Change: invoiceChange()})
	if !errors.Is(err, domain.ErrOntologyDraftRefused) || !strings.Contains(err.Error(), "already rejected") || again.Draft.ID != denied.Draft.ID {
		t.Fatalf("re-propose decided: %+v %v", again, err)
	}
	if n := len(r.audit.actions()); n != 4 {
		t.Fatalf("audit: 2 pending + 2 terminal, got %v", r.audit.actions())
	}
}

func TestDrafts_UnroutableApprovalRejectsAtOnce(t *testing.T) {
	t.Parallel()
	r := newDraftRig(t)
	r.hub.failState = domain.ApprovalUnroutable
	res, err := r.d.Propose(context.Background(), agentP, aliceP, DraftProposal{Change: invoiceChange()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Draft.State != domain.OntologyDraftRejected || !strings.Contains(res.Draft.Detail, "unroutable") ||
		res.ApprovalState != domain.ApprovalUnroutable {
		t.Fatalf("unroutable: %+v", res)
	}
}

func TestDrafts_StaleBase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The namespace moved in a way that changes what the draft does: superseded,
	// nothing applied, the reason names both classifications.
	r := newDraftRig(t)
	tighten := domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "order", Predicates: map[string]domain.ValueSpec{
		"status": {Type: "text", Enum: []string{"open", "shipped"}}, "total": {Type: "number"}}}}}
	res, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: "ns1", Change: tighten})
	if err != nil || res.Draft.Class != domain.ChangeTightening {
		t.Fatalf("draft: %+v %v", res, err)
	}
	// An operator applies the same tightening first: the draft now changes nothing.
	if _, err := r.onto.Apply(ctx, "ns1", tighten, "user:op", false); err != nil {
		t.Fatal(err)
	}
	r.hub.resolve(t, res.Draft.ApprovalID, domain.ApprovalApproved, "bob")
	if r.hub.lastErr == nil || !strings.Contains(r.hub.lastErr.Error(), "moved from version 1 to 2") ||
		!strings.Contains(r.hub.lastErr.Error(), "no change instead of tightening") {
		t.Fatalf("stale approval: %v", r.hub.lastErr)
	}
	dr, _, _ := r.store.getDraft(ctx, res.Draft.ID)
	if dr.State != domain.OntologyDraftSuperseded || !strings.Contains(dr.Detail, "approved by user:bob") || len(r.onto.authors) != 1 {
		t.Fatalf("superseded: %+v (authors %v)", dr, r.onto.authors)
	}

	// The namespace moved, but not under this change: it still classifies the
	// same, so it applies.
	r2 := newDraftRig(t)
	res2, err := r2.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: "ns1", Change: invoiceChange()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.onto.Apply(ctx, "ns1", domain.OntologyDoc{Relations: []domain.RelationSpec{{Name: "ships_to",
		Family: domain.LinkFamilyRelation}}}, "user:op", false); err != nil {
		t.Fatal(err)
	}
	r2.hub.resolve(t, res2.Draft.ApprovalID, domain.ApprovalApproved, "bob")
	if r2.hub.lastErr != nil || fmt.Sprint(r2.onto.authors) != "[user:op user:bob]" {
		t.Fatalf("unrelated move: %v %v", r2.hub.lastErr, r2.onto.authors)
	}

	// The namespace moved INTO a conflict: now breaking — superseded, not applied.
	r3 := newDraftRig(t)
	addRate := domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "invoice", Predicates: map[string]domain.ValueSpec{"rate": {Type: "number"}}}}}
	res3, err := r3.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: "ns1", Change: addRate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r3.onto.Apply(ctx, "ns1", domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "invoice",
		Predicates: map[string]domain.ValueSpec{"rate": {Type: "text"}}}}}, "user:op", false); err != nil {
		t.Fatal(err)
	}
	r3.hub.resolve(t, res3.Draft.ApprovalID, domain.ApprovalApproved, "bob")
	if r3.hub.lastErr == nil || !strings.Contains(r3.hub.lastErr.Error(), "breaking") || len(r3.onto.authors) != 1 {
		t.Fatalf("moved into breaking: %v %v", r3.hub.lastErr, r3.onto.authors)
	}
}

func TestDrafts_StartStopIsClean(t *testing.T) {
	t.Parallel()
	r := newDraftRig(t)
	r.d.every = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.d.Start(ctx)
	time.Sleep(5 * time.Millisecond)
	r.d.Stop()
	r.d.Stop() // idempotent
	newDraftRig(t).d.Stop()
}
