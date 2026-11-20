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

// EvidenceReader is the slice of the evidence store the bridge uses to check that
// the evidence a proposal cites is evidence the proposer may read.
type EvidenceReader interface {
	Get(ctx context.Context, id domain.EvidenceID) (*domain.Evidence, error)
}

// Option configures the bridge at construction.
type Option func(*Service)

// WithEvidence gives the bridge the evidence store it checks cited evidence against
// (T-20261107-propose-knowledge-link-proposals-do-not-check). Without it a proposal
// citing evidence is refused: the bridge cannot tell the proposer may read it.
func WithEvidence(ev EvidenceReader) Option {
	return func(s *Service) { s.evidence = ev }
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
	store domain.KnowledgeStore
	kinds domain.KindSource
	authz domain.Authorizer
	hub   Hub
	links LinkWriter
	// evidence resolves cited evidence for the readability check (citedEvidence).
	evidence EvidenceReader
	now      func() time.Time
	logger   *slog.Logger
}

// New builds the bridge and registers its approval handler on hub. store is the
// RAW kernel knowledge store: the bridge IS the kernel path that stamps actors, so it
// must not sit behind the plugin-seam guard it exists to satisfy. links may be nil
// (ProposeLink then refuses). A nil authorizer is the OSS allow-all default.
//
// opts must carry WithEvidence for any proposal that cites evidence to be accepted.
func New(store domain.KnowledgeStore, kinds domain.KindSource, a domain.Authorizer, hub Hub, links LinkWriter, logger *slog.Logger, opts ...Option) (*Service, error) {
	if store == nil || kinds == nil || hub == nil {
		return nil, errors.New("knowledgecommit.New: store, kind registry and approval hub are required")
	}
	if a == nil {
		a = domain.AllowAllAuthorizer{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{store: store, kinds: kinds, authz: a, hub: hub, links: links, now: time.Now, logger: logger}
	for _, o := range opts {
		o(s)
	}
	hub.HandleKind(domain.ApprovalKindKnowledgeCommit, s.Apply)
	return s, nil
}

// designatedSpec returns the spec of a kind that accepts proposals.
func (s *Service) designatedSpec(namespace, kind string) (domain.KindSpec, error) {
	spec, ok := s.kinds.KindsFor(namespace).Spec(kind)
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
	spec, err := s.designatedSpec(p.NamespaceID, p.Kind)
	if err != nil {
		return Result{}, err
	}
	if err := s.citedEvidence(ctx, by, p.EvidenceID); err != nil {
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
	// ENG-2115: fail closed — a proposal whose requested classification cannot be
	// carried in full is refused (the error names the tags; it wraps ErrWriteDenied).
	final, _, err := authz.ClassifyWriteStrict(ctx, s.authz, by, domain.ClassificationHint(p.Classification))
	if err != nil {
		return Result{}, fmt.Errorf("knowledge proposal for %s: %w", p.EntityID, err)
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
//
// The candidate must be readable (ENG-1768, owner decision 2026-11-20): by the
// requester `by` AND, when the request is made for a person, by that person —
// each under their OWN read predicate (authz.MayReadKnowledgeItem: the
// Authorizer's ReadFilter, then rowfence.Readable on the item). The store read
// below is unscoped (the bridge holds the raw store), so this check is what keeps
// anyone from raising a commit approval for a candidate they cannot see. It lives
// here, not in a transport, so every surface that reaches RequestCommit inherits
// it. An unreadable candidate answers exactly as an unknown one (no existence
// oracle).
func (s *Service) RequestCommit(ctx context.Context, by, forPerson domain.PrincipalRef, candidate domain.KnowledgeItemID, reason string) (domain.ApprovalItem, error) {
	if by.IsZero() {
		return domain.ApprovalItem{}, fmt.Errorf("%w: no requester could be established", ErrNotProposable)
	}
	if candidate == "" {
		return domain.ApprovalItem{}, fmt.Errorf("%w: a candidate id is required", ErrNotProposable)
	}
	refuse := fmt.Errorf("%w: candidate %q is unknown or not readable by %s", ErrNotProposable, candidate, by.String())
	it, err := s.store.GetItem(ctx, candidate)
	if err != nil {
		// The store names no not-found sentinel, so every failed read answers as
		// "unknown": fail closed, and never an existence oracle. Logged with the
		// cause so an outage is visible to the operator.
		s.logger.Info("knowledge commit request refused: candidate not read", "event", "knowledge_commit_request_refused",
			"by", by.String(), "candidate", string(candidate), "err", err)
		return domain.ApprovalItem{}, refuse
	}
	for _, reader := range []domain.PrincipalRef{by, forPerson} {
		if reader.IsZero() {
			continue
		}
		if !authz.MayReadKnowledgeItem(ctx, s.authz, reader, it, s.logger) {
			s.logger.Info("knowledge commit request refused: candidate not readable", "event", "knowledge_commit_request_refused",
				"by", by.String(), "reader", reader.String(), "candidate", string(candidate))
			return domain.ApprovalItem{}, refuse
		}
	}
	spec, err := s.designatedSpec(it.NamespaceID, it.Kind)
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
	spec, err := s.designatedSpec(cand.NamespaceID, cand.Kind)
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
	if err := s.citedEvidence(ctx, by, p.EvidenceID); err != nil {
		return false, err
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

// citedEvidence refuses a proposal whose cited evidence the proposer may not read
// (T-20261107-propose-knowledge-link-proposals-do-not-check): a claim or a link may
// stand only on evidence its asserter could see. An unknown id, an unreadable one
// and one the bridge cannot look up all get the SAME refusal, so the answer is not
// an oracle for which evidence ids exist; the cause is logged for the operator.
// No evidence cited is not this check's business (the link store's admissibility
// rule still requires evidence for a link).
func (s *Service) citedEvidence(ctx context.Context, by domain.PrincipalRef, id domain.EvidenceID) error {
	if id == "" {
		return nil
	}
	refuse := fmt.Errorf("%w: evidence %q is unknown or not readable by %s", ErrNotProposable, id, by.String())
	if s.evidence == nil {
		s.logger.Warn("knowledge proposal refused: no evidence store to check the citation against",
			"event", "knowledge_proposal_evidence_unchecked", "by", by.String())
		return refuse
	}
	ev, err := s.evidence.Get(ctx, id)
	if err != nil {
		s.logger.Info("knowledge proposal refused: cited evidence not found", "event", "knowledge_proposal_evidence_refused",
			"by", by.String(), "evidence", string(id), "err", err)
		return refuse
	}
	if !authz.MayReadEvidence(ctx, s.authz, by, ev, s.logger) {
		s.logger.Info("knowledge proposal refused: cited evidence not readable", "event", "knowledge_proposal_evidence_refused",
			"by", by.String(), "evidence", string(id))
		return refuse
	}
	return nil
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
