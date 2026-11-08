// Package knowledgecommit is the kernel's proposal → approval → commit bridge for
// the knowledge substrate (T-20261107-knowledge-substrate-extensions-for-case;
// investigation 2026-11-07 §7 gaps 2 and 6).
//
// Three operations, one rule — the kernel stamps who asserted what:
//
//   - Propose writes a CANDIDATE item asserted by the proposer (the agent, or the
//     app, the transport authenticated). Only kinds resolved by designated_actor
//     accept proposals, and a proposer that is itself designated is refused, so a
//     proposal can never be the committed value.
//   - RequestCommit raises an async approval of kind domain.ApprovalKindKnowledgeCommit
//     naming the candidate (Object). The approval policy decides who may approve.
//   - Apply (the hub's handler for that kind) writes the COMMITTED item asserted by
//     the approver the hub authenticated — never a value from the request — with a
//     deterministic commit marker, so a re-run is a replay. Deny and expiry never
//     reach it: the hub runs a handler only for an approved item.
//
// ProposeLink writes a candidate link (mechanism scored — the trust ceiling caps it at
// candidate — and the registry's evidence rule applies), asserted by the proposer.
//
// Nothing here names a case type: kinds, lifecycles and approvers are data.
package knowledgecommit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// ErrNotProposable refuses a proposal the bridge cannot hold below a committed value.
var ErrNotProposable = errors.New("knowledge proposal refused")

// Hub is the slice of the approval hub the bridge uses.
type Hub interface {
	Raise(ctx context.Context, req domain.ApprovalRequest) (domain.ApprovalItem, error)
	HandleKind(kind string, fn domain.ApprovalHandler)
}

// LinkWriter is the slice of the link store the bridge uses.
type LinkWriter interface {
	PutLink(ctx context.Context, l domain.Link) (bool, error)
}

// Proposal is one proposed knowledge item. It carries no actor: the asserter is
// the principal the caller passes, which the transport resolved (INV-5).
type Proposal struct {
	NamespaceID string
	Kind        string
	EntityID    string
	Values      []domain.StatementValue
	ValidFrom   time.Time
	ValidTo     time.Time
	Negation    bool
	EvidenceID  domain.EvidenceID
	// Classification is a HINT; the decision point's ClassifyWrite answer replaces it.
	Classification []string
	// ClientRef is the caller's idempotency key, folded into the candidate's
	// deterministic SourceRef ("" = the content alone keys it).
	ClientRef string
}

// LinkProposal is one proposed candidate link.
type LinkProposal struct {
	NamespaceID string
	Family      string
	FromRef     string
	ToRef       string
	Relation    string
	EvidenceID  domain.EvidenceID
	Confidence  float64
	ClientRef   string
}

// Result reports a proposal.
type Result struct {
	CandidateID domain.KnowledgeItemID
	Inserted    bool
	// ApprovalID / ApprovalState are set when a commit was requested.
	ApprovalID    string
	ApprovalState domain.ApprovalState
}

// Service is the bridge. Construct with New.
type Service struct {
	store  domain.KnowledgeStore
	reg    *domain.KindRegistry
	authz  domain.Authorizer
	hub    Hub
	links  LinkWriter
	now    func() time.Time
	logger *slog.Logger
}

// New builds the bridge and registers its approval handler on hub. store is the
// RAW kernel knowledge store: the bridge IS the kernel path that stamps actors, so it
// must not sit behind the plugin-seam guard it exists to satisfy. links may be nil
// (ProposeLink then refuses). A nil authorizer is the OSS allow-all default.
func New(store domain.KnowledgeStore, reg *domain.KindRegistry, a domain.Authorizer, hub Hub, links LinkWriter, logger *slog.Logger) (*Service, error) {
	if store == nil || reg == nil || hub == nil {
		return nil, errors.New("knowledgecommit.New: store, kind registry and approval hub are required")
	}
	if a == nil {
		a = domain.AllowAllAuthorizer{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{store: store, reg: reg, authz: a, hub: hub, links: links, now: time.Now, logger: logger}
	hub.HandleKind(domain.ApprovalKindKnowledgeCommit, s.Apply)
	return s, nil
}

// designatedSpec returns the spec of a kind that accepts proposals.
func (s *Service) designatedSpec(kind string) (domain.KindSpec, error) {
	spec, ok := s.reg.Spec(kind)
	if !ok || spec.Policy != domain.ResolutionPolicyDesignatedActor || spec.Designated == nil {
		return domain.KindSpec{}, fmt.Errorf("%w: kind %q is not resolved by %s, so a proposal could not stay below a committed value",
			ErrNotProposable, kind, domain.ResolutionPolicyDesignatedActor)
	}
	return spec, nil
}

// Propose writes p as a candidate item asserted by `by`. When commit is true it also
// raises a knowledge_commit approval for `forPerson` (the self rule's subject).
func (s *Service) Propose(ctx context.Context, by, forPerson domain.PrincipalRef, p Proposal, commit bool, reason string) (Result, error) {
	if by.IsZero() {
		return Result{}, fmt.Errorf("%w: no proposer could be established", ErrNotProposable)
	}
	if p.Kind == "" || p.EntityID == "" || (len(p.Values) == 0 && !p.Negation) {
		return Result{}, fmt.Errorf("%w: kind, entity_id and at least one value are required", ErrNotProposable)
	}
	spec, err := s.designatedSpec(p.Kind)
	if err != nil {
		return Result{}, err
	}
	actor := by.String()
	item := domain.KnowledgeItem{
		NamespaceID: p.NamespaceID,
		Kind:        p.Kind,
		EvidenceID:  p.EvidenceID,
		EntityID:    p.EntityID,
		AssertedBy:  actor,
		AssertedAt:  s.now().UTC(),
		Negation:    p.Negation,
		ValidFrom:   p.ValidFrom,
		ValidTo:     p.ValidTo,
		Values:      p.Values,
	}
	item.SourceRef = domain.KnowledgeProposalSourceRefPrefix + actor + ":" + proposalHash(p)
	if spec.Designated.Eligible(item) {
		return Result{}, fmt.Errorf("%w: %s is a designated actor of kind %q, so its word would be the committed value; "+
			"commit through knowledge_commit instead", ErrNotProposable, actor, p.Kind)
	}
	final, dec := s.authz.ClassifyWrite(ctx, by, domain.ClassificationHint(p.Classification))
	if !dec.Allowed {
		return Result{}, fmt.Errorf("knowledge proposal for %s: %w", p.EntityID, authz.ErrWriteDenied)
	}
	item.Classification = domain.StampPrivateOwner(ctx, append([]string(nil), final...))
	id, inserted, err := s.store.PutItem(ctx, item)
	if err != nil {
		return Result{}, err
	}
	res := Result{CandidateID: id, Inserted: inserted}
	s.logger.Info("knowledge proposal recorded", "event", "knowledge_proposed",
		"kind", p.Kind, "entity", p.EntityID, "candidate", id, "by", actor, "inserted", inserted)
	if !commit {
		return res, nil
	}
	item.ID = id
	ap, err := s.raise(ctx, by, forPerson, spec, item, reason)
	if err != nil {
		return res, err
	}
	res.ApprovalID, res.ApprovalState = ap.ID, ap.State
	return res, nil
}

// RequestCommit raises a knowledge_commit approval for an existing candidate.
func (s *Service) RequestCommit(ctx context.Context, by, forPerson domain.PrincipalRef, candidate domain.KnowledgeItemID, reason string) (domain.ApprovalItem, error) {
	it, err := s.store.GetItem(ctx, candidate)
	if err != nil {
		return domain.ApprovalItem{}, err
	}
	spec, err := s.designatedSpec(it.Kind)
	if err != nil {
		return domain.ApprovalItem{}, err
	}
	if domain.IsKnowledgeCommitRef(it.SourceRef) {
		return domain.ApprovalItem{}, fmt.Errorf("%w: item %s is already a commit", ErrNotProposable, candidate)
	}
	return s.raise(ctx, by, forPerson, spec, *it, reason)
}

func (s *Service) raise(ctx context.Context, by, forPerson domain.PrincipalRef, spec domain.KindSpec, cand domain.KnowledgeItem, reason string) (domain.ApprovalItem, error) {
	class := spec.CommitClass
	if class == "" {
		class = domain.ApprovalKindKnowledgeCommit
	}
	req := domain.ApprovalRequest{
		Kind:        domain.ApprovalKindKnowledgeCommit,
		ActionClass: class,
		Resource:    spec.Kind + "/" + cand.EntityID,
		Object:      string(cand.ID),
		ToolName:    "knowledge_commit",
		ArgsPreview: preview(cand, reason),
		Beneficiary: forPerson,
	}
	if by.Kind == domain.PrincipalAgent {
		req.AgentID = by.ID
	}
	ap, err := s.hub.Raise(ctx, req)
	if err != nil {
		return ap, fmt.Errorf("knowledge commit: raise approval: %w", err)
	}
	s.logger.Info("knowledge commit requested", "event", "knowledge_commit_requested",
		"kind", spec.Kind, "entity", cand.EntityID, "candidate", cand.ID, "approval", ap.ID,
		"state", string(ap.State), "by", by.String())
	return ap, nil
}

// Apply is the knowledge_commit handler: it writes the committed item asserted by
// the APPROVER (it.ResolvedBy, set by the hub from the authenticated answer). The
// request's arguments are never read for identity.
func (s *Service) Apply(ctx context.Context, it domain.ApprovalItem) error {
	if it.State != domain.ApprovalApproved {
		return fmt.Errorf("knowledge commit %s: item is %s, not approved", it.ID, it.State)
	}
	approver := domain.ApproverActor(it.ResolvedBy)
	if approver == "" {
		// A standing approval names a grant, not the person who answered this one;
		// a commit is attributed to a person, so it needs a person's answer.
		return fmt.Errorf("knowledge commit %s: resolved by %q, which names no approving person; "+
			"a commit needs an answer, not a standing approval", it.ID, it.ResolvedBy)
	}
	cand, err := s.store.GetItem(ctx, domain.KnowledgeItemID(it.Object))
	if err != nil {
		return fmt.Errorf("knowledge commit %s: candidate %s: %w", it.ID, it.Object, err)
	}
	spec, err := s.designatedSpec(cand.Kind)
	if err != nil {
		return fmt.Errorf("knowledge commit %s: %w", it.ID, err)
	}
	at := it.ResolvedAt
	if at.IsZero() {
		at = s.now()
	}
	committed := domain.KnowledgeItem{
		NamespaceID:    cand.NamespaceID,
		Kind:           cand.Kind,
		EvidenceID:     cand.EvidenceID,
		EntityID:       cand.EntityID,
		AssertedBy:     approver,
		AssertedAt:     at.UTC(),
		SourceRef:      domain.KnowledgeCommitSourceRefPrefix + it.ID,
		Negation:       cand.Negation,
		Classification: cand.Classification,
		ValidFrom:      cand.ValidFrom,
		ValidTo:        cand.ValidTo,
		Values:         cand.Values,
	}
	if !spec.Designated.Eligible(committed) {
		// Writing it would record an approval that commits nothing — refuse loudly.
		return fmt.Errorf("knowledge commit %s: approver %s is not a designated actor of kind %q and the kind does not admit approved commits",
			it.ID, approver, cand.Kind)
	}
	id, inserted, err := s.store.PutItem(ctx, committed)
	if err != nil {
		return fmt.Errorf("knowledge commit %s: %w", it.ID, err)
	}
	s.logger.Info("knowledge committed", "event", "knowledge_committed",
		"kind", cand.Kind, "entity", cand.EntityID, "candidate", cand.ID, "item", id,
		"approval", it.ID, "approver", approver, "inserted", inserted)
	return nil
}

// ProposeLink writes a candidate link asserted by `by`. The mechanism is fixed to
// `scored` (a model proposed it), whose trust ceiling is candidate; the registry's
// admissibility rule still requires evidence.
func (s *Service) ProposeLink(ctx context.Context, by domain.PrincipalRef, p LinkProposal) (bool, error) {
	if s.links == nil {
		return false, fmt.Errorf("%w: no link store is wired", ErrNotProposable)
	}
	if by.IsZero() {
		return false, fmt.Errorf("%w: no proposer could be established", ErrNotProposable)
	}
	family := p.Family
	if family == "" {
		family = domain.LinkFamilyRelation
	}
	conf := p.Confidence
	if conf <= 0 || conf > 1 {
		conf = 1
	}
	h := sha256.Sum256([]byte(strings.Join([]string{family, p.FromRef, p.ToRef, p.Relation, string(p.EvidenceID), p.ClientRef}, "\x1f")))
	return s.links.PutLink(ctx, domain.Link{
		NamespaceID: p.NamespaceID,
		Family:      family,
		FromRef:     p.FromRef,
		ToRef:       p.ToRef,
		Relation:    p.Relation,
		State:       domain.LinkStateCandidate,
		Mechanism:   domain.LinkMechanismScored,
		Producer:    "propose_knowledge@1",
		Confidence:  conf,
		EvidenceID:  p.EvidenceID,
		AssertedBy:  by.String(),
		AssertedAt:  s.now().UTC(),
		SourceRef:   domain.KnowledgeProposalSourceRefPrefix + by.String() + ":" + hex.EncodeToString(h[:8]),
	})
}

// proposalHash keys a proposal by its content (and the caller's ref), so a retried
// call is a replay rather than a second candidate (AGENTS.md §2.6).
func proposalHash(p Proposal) string {
	vals := make([]string, 0, len(p.Values))
	for _, v := range p.Values {
		vals = append(vals, v.Predicate+"\x1e"+v.Type+"\x1e"+valueString(v))
	}
	sort.Strings(vals)
	parts := []string{p.Kind, p.EntityID, strings.Join(vals, "\x1d"),
		timeKey(p.ValidFrom), timeKey(p.ValidTo), strconv.FormatBool(p.Negation), string(p.EvidenceID), p.ClientRef}
	h := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(h[:12])
}

func timeKey(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func valueString(v domain.StatementValue) string {
	switch v.Type {
	case "date":
		return timeKey(v.Date)
	case "number":
		return strconv.FormatFloat(v.Number, 'g', -1, 64)
	case "entity":
		return v.EntityRef
	}
	return v.Text
}

// preview is the bounded, displayable form an approver sees.
func preview(it domain.KnowledgeItem, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "commit %s %s", it.Kind, it.EntityID)
	if it.Negation {
		b.WriteString(" (retract)")
	}
	vals := make([]string, 0, len(it.Values))
	for _, v := range it.Values {
		vals = append(vals, v.Predicate+"="+valueString(v))
	}
	sort.Strings(vals)
	if len(vals) > 0 {
		b.WriteString(": " + strings.Join(vals, ", "))
	}
	b.WriteString(" — proposed by " + it.AssertedBy)
	if reason != "" {
		b.WriteString(" — " + reason)
	}
	out := b.String()
	if r := []rune(out); len(r) > 200 {
		out = string(r[:199]) + "…"
	}
	return out
}
