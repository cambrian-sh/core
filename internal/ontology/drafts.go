package ontology

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// Agent-drafted ontology changes (ADR-0149 D2; owner option (a),
// T-20261113-agent-drafted-ontology-changes-have-no-approval). See
// domain/ontology_draft.go for the model. This file is the kernel side:
//
//   - Propose classifies a draft with Apply's own dry run (a breaking, invalid
//     or empty change is refused at once, with Apply's reason), persists it
//     (append-only), and raises an ASYNC ontology_change approval naming it.
//   - The approval handler (registered at construction, so an item approved
//     after a restart still applies) calls the same apply body as an operator's
//     ApplyOntologyChange, AS THE APPROVING PERSON, with one extra check under
//     its lock: if the namespace moved since the draft and the change no longer
//     classifies as drafted, it is refused and the draft is superseded.
//   - Reconcile records the resolutions the hub runs no handler for (denied,
//     withdrawn, unroutable → rejected; expired → expired). Start runs it at
//     boot and every interval.
//
// Every transition is written to the operator audit (the audit func) and logged.

// DraftHub is the slice of the approval hub drafts use (domain.AddressedHub).
type DraftHub interface {
	Raise(ctx context.Context, req domain.ApprovalRequest) (domain.ApprovalItem, error)
	HandleKind(kind string, fn domain.ApprovalHandler)
	Get(ctx context.Context, id string) (domain.ApprovalItem, error)
}

// draftApplier is the slice of *Ontology drafts use.
type draftApplier interface {
	Current(namespace string) *domain.OntologySnapshot
	Apply(ctx context.Context, namespace string, change domain.OntologyDoc, author string, dryRun bool) (domain.OntologyApplyResult, error)
	apply(ctx context.Context, namespace string, change domain.OntologyDoc, author string, dryRun bool, check applyCheck) (domain.OntologyApplyResult, error)
}

// DraftProposal is one draft as a caller submits it. It carries no proposer:
// that is the principal the transport resolved.
type DraftProposal struct {
	Namespace string
	Change    domain.OntologyDoc
	Reason    string
	// ClientRef is the caller's idempotency key, folded into the draft id
	// ("" = the content alone keys it). A new ref drafts the same change anew.
	ClientRef string
}

// DraftResult reports a proposal.
type DraftResult struct {
	Draft domain.OntologyDraft
	// Created is false when this exact draft was already recorded (a replay).
	Created bool
	// ApprovalState is the raised approval's state ("" when none was raised).
	ApprovalState domain.ApprovalState
}

// errSuperseded marks an approved draft that no longer applies as drafted.
var errSuperseded = errors.New("the draft is superseded")

// DefaultDraftReconcileInterval matches the approval hub's sweep cadence.
const DefaultDraftReconcileInterval = 30 * time.Second

// Drafts is the draft → approval → apply bridge. Build with NewDrafts.
type Drafts struct {
	onto   draftApplier
	store  draftStore
	hub    DraftHub
	audit  func(ctx context.Context, e domain.AuditEntry) error
	now    func() time.Time
	logger *slog.Logger
	every  time.Duration

	startOnce, stopOnce sync.Once
	stopCh, done        chan struct{}
	mu                  sync.Mutex
	started             bool
}

// NewDrafts builds the bridge over pool and registers its approval handler on
// hub. audit receives every transition (nil = logged only).
func NewDrafts(pool *pgxpool.Pool, onto *Ontology, hub DraftHub, audit func(ctx context.Context, e domain.AuditEntry) error, logger *slog.Logger) (*Drafts, error) {
	if pool == nil || onto == nil || hub == nil {
		return nil, errors.New("ontology.NewDrafts: pool, ontology and approval hub are required")
	}
	return newDrafts(onto, pgDraftStore{pool: pool}, hub, audit, logger), nil
}

func newDrafts(onto draftApplier, store draftStore, hub DraftHub, audit func(ctx context.Context, e domain.AuditEntry) error, logger *slog.Logger) *Drafts {
	if logger == nil {
		logger = slog.Default()
	}
	d := &Drafts{onto: onto, store: store, hub: hub, audit: audit, now: time.Now, logger: logger,
		every: DefaultDraftReconcileInterval, stopCh: make(chan struct{}), done: make(chan struct{})}
	hub.HandleKind(domain.ApprovalKindOntologyChange, d.handle)
	return d
}

// Propose drafts p for namespace as `by` (the agent the transport resolved);
// forPerson is the run principal, the approval's beneficiary. A refusal the
// proposer can act on wraps domain.ErrOntologyDraftRefused; the result still
// carries the draft when one exists (a replay of a decided draft).
func (d *Drafts) Propose(ctx context.Context, by, forPerson domain.PrincipalRef, p DraftProposal) (DraftResult, error) {
	if by.IsZero() {
		return DraftResult{}, fmt.Errorf("%w: no proposer could be established", domain.ErrOntologyDraftRefused)
	}
	proposer := by.String()
	ns := orDefault(p.Namespace)
	id, err := domain.OntologyDraftID(ns, p.Change, proposer, p.ClientRef)
	if err != nil {
		return DraftResult{}, fmt.Errorf("%w: %w", domain.ErrOntologyDraftRefused, err)
	}
	dr, found, err := d.store.getDraft(ctx, id)
	if err != nil {
		return DraftResult{}, err
	}
	created := false
	if !found {
		res, aerr := d.onto.Apply(ctx, ns, p.Change, proposer, true)
		if aerr != nil {
			return DraftResult{}, fmt.Errorf("%w: %w", domain.ErrOntologyDraftRefused, aerr)
		}
		if len(res.Changes) == 0 {
			return DraftResult{}, fmt.Errorf("%w: the change declares nothing that namespace %q does not already declare",
				domain.ErrOntologyDraftRefused, ns)
		}
		dr = domain.OntologyDraft{ID: id, Namespace: ns, Change: p.Change, Proposer: proposer,
			BaseVersion: res.Version, Class: res.Class, Changes: res.Changes, Reason: p.Reason,
			CreatedAt: d.now().UTC().Truncate(time.Microsecond), State: domain.OntologyDraftPending}
		inserted, err := d.store.insertDraft(ctx, dr)
		if err != nil {
			return DraftResult{}, err
		}
		if !inserted { // a concurrent retry recorded it first: use its row
			if dr, _, err = d.store.getDraft(ctx, id); err != nil {
				return DraftResult{}, err
			}
		}
		created = inserted
	}
	out := DraftResult{Draft: dr, Created: created}
	if dr.ApprovalID != "" {
		// Already raised: report where it stands, never raise a second approval.
		dr = d.reconcileOne(ctx, dr)
		out.Draft = dr
		if it, err := d.hub.Get(ctx, dr.ApprovalID); err == nil {
			out.ApprovalState = it.State
		}
		if dr.State != domain.OntologyDraftPending {
			return out, fmt.Errorf("%w: draft %s was already %s%s; draft it again with a new client_ref to ask anew",
				domain.ErrOntologyDraftRefused, dr.ID, dr.State, suffix(dr.Detail))
		}
		return out, nil
	}
	ap, err := d.raise(ctx, by, forPerson, dr)
	if err != nil {
		return out, err
	}
	dr.ApprovalID = ap.ID
	if _, err := d.store.appendEvent(ctx, draftEvent{DraftID: dr.ID, State: domain.OntologyDraftPending,
		ApprovalID: ap.ID, Actor: proposer, Detail: dr.Reason, At: dr.CreatedAt}); err != nil {
		return out, err
	}
	d.record(ctx, dr, domain.OntologyDraftPending, proposer, dr.Reason, ap.ID, 0)
	// A request the policy could not route fails closed at once; a standing
	// approval (if a deployment allows one) resolves at once too.
	dr = d.reconcileOne(ctx, dr)
	if cur, ok, gerr := d.store.getDraft(ctx, dr.ID); gerr == nil && ok {
		dr = cur
	}
	out.Draft, out.ApprovalState = dr, ap.State
	return out, nil
}

func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

func (d *Drafts) raise(ctx context.Context, by, forPerson domain.PrincipalRef, dr domain.OntologyDraft) (domain.ApprovalItem, error) {
	req := domain.ApprovalRequest{
		Kind:        domain.ApprovalKindOntologyChange,
		ActionClass: domain.ApprovalKindOntologyChange,
		Resource:    "ontology/" + dr.Namespace,
		Object:      dr.ID,
		ToolName:    domain.ApprovalKindOntologyChange,
		ArgsPreview: draftPreview(dr),
		Beneficiary: forPerson,
	}
	if by.Kind == domain.PrincipalAgent {
		req.AgentID = by.ID
	}
	ap, err := d.hub.Raise(ctx, req)
	if err != nil {
		return ap, fmt.Errorf("ontology draft %s: raise approval: %w", dr.ID, err)
	}
	d.logger.Info("ontology change drafted", "event", "ontology_draft_raised", "draft", dr.ID,
		"namespace", dr.Namespace, "class", string(dr.Class), "approval", ap.ID, "state", string(ap.State), "by", by.String())
	return ap, nil
}

// handle is the ontology_change approval handler: it applies the draft as the
// APPROVER (it.ResolvedBy, set by the hub from the authenticated answer).
func (d *Drafts) handle(ctx context.Context, it domain.ApprovalItem) error {
	if it.State != domain.ApprovalApproved {
		return fmt.Errorf("ontology change %s: item is %s, not approved", it.ID, it.State)
	}
	approver := domain.ApproverActor(it.ResolvedBy)
	if approver == "" {
		// A declarations change is attributed to a person, so it needs a
		// person's answer, not a standing grant.
		return fmt.Errorf("ontology change %s: resolved by %q, which names no approving person; "+
			"an ontology change needs an answer, not a standing approval", it.ID, it.ResolvedBy)
	}
	dr, found, err := d.store.getDraft(ctx, it.Object)
	if err != nil {
		return fmt.Errorf("ontology change %s: %w", it.ID, err)
	}
	if !found {
		return fmt.Errorf("ontology change %s: draft %s does not exist", it.ID, it.Object)
	}
	switch dr.State {
	case domain.OntologyDraftApplied:
		return nil // a re-run handler is a replay
	case domain.OntologyDraftPending:
	default:
		return fmt.Errorf("ontology change %s: draft %s is already %s", it.ID, dr.ID, dr.State)
	}
	res, err := d.onto.apply(ctx, dr.Namespace, dr.Change, approver, false, stillAsDrafted(dr))
	if err != nil {
		if errors.Is(err, domain.ErrOntologyChangeRefused) {
			detail := "approved by " + approver + ", but not applied: " + err.Error()
			d.transition(ctx, dr, domain.OntologyDraftSuperseded, "kernel", detail, it.ID, 0)
			return fmt.Errorf("ontology change %s: draft %s superseded: %w", it.ID, dr.ID, err)
		}
		// Infrastructure: nothing applied and nothing decided; the approval
		// records the failure (the hub writes it on the item's Detail).
		return fmt.Errorf("ontology change %s: draft %s: %w", it.ID, dr.ID, err)
	}
	if !res.Applied { // unreachable: stillAsDrafted refuses an empty change
		d.transition(ctx, dr, domain.OntologyDraftSuperseded, "kernel", "nothing left to apply", it.ID, 0)
		return fmt.Errorf("ontology change %s: draft %s: nothing left to apply", it.ID, dr.ID)
	}
	d.transition(ctx, dr, domain.OntologyDraftApplied, approver, "", it.ID, res.Version)
	return nil
}

// stillAsDrafted refuses a draft whose namespace moved since it was drafted when
// the change no longer classifies the same (a different class, or different
// differences — including none left). Unmoved, the classification is the
// drafted one by construction.
func stillAsDrafted(dr domain.OntologyDraft) applyCheck {
	return func(cur *domain.OntologySnapshot, changes []domain.OntologyChange) error {
		if cur.Version == dr.BaseVersion || sameChanges(changes, dr.Changes) {
			return nil
		}
		return fmt.Errorf("%w: namespace %q moved from version %d to %d since it was drafted, and the change now "+
			"classifies as %s instead of %s; draft it again against the current declarations",
			errSuperseded, dr.Namespace, dr.BaseVersion, cur.Version, classSummary(changes), classSummary(dr.Changes))
	}
}

func sameChanges(a, b []domain.OntologyChange) bool {
	return slices.Equal(a, b)
}

func classSummary(changes []domain.OntologyChange) string {
	if len(changes) == 0 {
		return "no change"
	}
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, string(c.Class)+" "+c.Subject+" ("+c.Detail+")")
	}
	return string(domain.MaxChangeClass(changes)) + " [" + strings.Join(parts, "; ") + "]"
}

// Reconcile records the resolution of every raised draft whose approval was
// denied, withdrawn, routed to nobody or expired — resolutions the hub runs no
// handler for. Idempotent.
func (d *Drafts) Reconcile(ctx context.Context) error {
	drafts, err := d.store.awaiting(ctx)
	if err != nil {
		return err
	}
	for _, dr := range drafts {
		d.reconcileOne(ctx, dr)
	}
	return nil
}

// reconcileOne moves a pending draft to the terminal state its approval
// reached, when the hub ran no handler for it. Returns the draft as it stands.
func (d *Drafts) reconcileOne(ctx context.Context, dr domain.OntologyDraft) domain.OntologyDraft {
	if dr.State != domain.OntologyDraftPending || dr.ApprovalID == "" {
		return dr
	}
	it, err := d.hub.Get(ctx, dr.ApprovalID)
	if err != nil {
		d.logger.Warn("ontology draft: approval unreadable", "draft", dr.ID, "approval", dr.ApprovalID, "err", err)
		return dr
	}
	var state domain.OntologyDraftState
	switch it.State {
	case domain.ApprovalDenied, domain.ApprovalWithdrawn, domain.ApprovalUnroutable, domain.ApprovalNeedsHierarchy:
		state = domain.OntologyDraftRejected
	case domain.ApprovalExpired:
		state = domain.OntologyDraftExpired
	default:
		return dr // pending, or approved (the handler decides)
	}
	actor := it.ResolvedBy
	if actor == "" {
		actor = "kernel"
	}
	detail := "approval " + string(it.State) + suffix(it.Detail)
	at := it.ResolvedAt
	if at.IsZero() {
		at = d.now()
	}
	if d.appendTerminal(ctx, dr, state, actor, detail, it.ID, 0, at) {
		dr.State, dr.StateBy, dr.Detail, dr.StateAt = state, actor, detail, at.UTC()
	} else if cur, ok, gerr := d.store.getDraft(ctx, dr.ID); gerr == nil && ok {
		dr = cur
	}
	return dr
}

func (d *Drafts) transition(ctx context.Context, dr domain.OntologyDraft, state domain.OntologyDraftState, actor, detail, approvalID string, version int64) {
	d.appendTerminal(ctx, dr, state, actor, detail, approvalID, version, d.now())
}

// appendTerminal records a terminal state once (the first terminal state wins)
// and audits it. Reports whether this call recorded it.
func (d *Drafts) appendTerminal(ctx context.Context, dr domain.OntologyDraft, state domain.OntologyDraftState,
	actor, detail, approvalID string, version int64, at time.Time) bool {
	inserted, err := d.store.appendEvent(ctx, draftEvent{DraftID: dr.ID, State: state, ApprovalID: approvalID,
		Actor: actor, Detail: detail, AppliedVersion: version, At: at.UTC()})
	if err != nil {
		d.logger.Warn("ontology draft: state not recorded", "draft", dr.ID, "state", string(state), "err", err)
		return false
	}
	if inserted {
		d.record(ctx, dr, state, actor, detail, approvalID, version)
	}
	return inserted
}

// record audits and logs one transition. The audit command id is
// deterministic, so a replayed transition is recorded once.
func (d *Drafts) record(ctx context.Context, dr domain.OntologyDraft, state domain.OntologyDraftState, actor, detail, approvalID string, version int64) {
	d.logger.Info("ontology draft "+string(state), "event", "ontology_draft_"+string(state), "draft", dr.ID,
		"namespace", dr.Namespace, "class", string(dr.Class), "approval", approvalID, "actor", actor,
		"version", version, "detail", detail)
	if d.audit == nil {
		return
	}
	after, _ := json.Marshal(map[string]any{"namespace": dr.Namespace, "class": dr.Class, "base_version": dr.BaseVersion,
		"proposer": dr.Proposer, "approval_id": approvalID, "state": state, "applied_version": version})
	if err := d.audit(context.WithoutCancel(ctx), domain.AuditEntry{
		CommandID:  "ontology_draft:" + dr.ID + ":" + string(state),
		At:         d.now().UTC(),
		Actor:      actor,
		ActionType: "ontology_draft_" + string(state),
		TargetType: "ontology_draft",
		TargetID:   dr.ID,
		After:      string(after),
		Reason:     detail,
		Result:     "ok",
	}); err != nil {
		d.logger.Warn("ontology draft: audit not written", "draft", dr.ID, "state", string(state), "err", err)
	}
}

var _ domain.OntologyDraftReader = (*Drafts)(nil)

// InspectDraft returns the full draft for an approver: the change and its
// classification as recorded, the current spec under every name the change
// names (the "old" side), and — while the draft is pending — a dry run against
// the current declarations through the SAME body and still-as-drafted check the
// approval handler runs, so what the approver reads is what approving would do.
// The dry run writes nothing and records nothing.
func (d *Drafts) InspectDraft(ctx context.Context, id string) (domain.OntologyDraftView, bool, error) {
	dr, found, err := d.store.getDraft(ctx, id)
	if err != nil || !found {
		return domain.OntologyDraftView{}, found, err
	}
	cur := d.onto.Current(dr.Namespace)
	view := domain.OntologyDraftView{Draft: dr, CurrentVersion: cur.Version, Current: namedIn(cur.Doc, dr.Change)}
	if dr.State == domain.OntologyDraftPending {
		res, aerr := d.onto.apply(ctx, dr.Namespace, dr.Change, "dry-run:"+dr.Proposer, true, stillAsDrafted(dr))
		view.DryRun = &res
		switch {
		case aerr != nil:
			view.DryRunRefusal = aerr.Error()
		case len(res.Changes) == 0:
			view.DryRunRefusal = "nothing left to apply: the current declarations already say this"
		}
	}
	return view, true, nil
}

// namedIn is the part of doc declared under the kind and relation names change
// names, in change's order.
func namedIn(doc, change domain.OntologyDoc) domain.OntologyDoc {
	var out domain.OntologyDoc
	for _, k := range change.Kinds {
		for _, c := range doc.Kinds {
			if c.Kind == k.Kind {
				out.Kinds = append(out.Kinds, c)
				break
			}
		}
	}
	for _, r := range change.Relations {
		for _, c := range doc.Relations {
			if c.Name == r.Name {
				out.Relations = append(out.Relations, c)
				break
			}
		}
	}
	return out
}

// draftPreview is the bounded line an approver sees: the namespace, the class
// and every classified difference, then who drafted it and why.
func draftPreview(dr domain.OntologyDraft) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s change to ontology %q (v%d): ", dr.Class, dr.Namespace, dr.BaseVersion)
	parts := make([]string, 0, len(dr.Changes))
	for _, c := range dr.Changes {
		parts = append(parts, c.Subject+" "+c.Detail)
	}
	b.WriteString(strings.Join(parts, "; "))
	b.WriteString(" — drafted by " + dr.Proposer)
	if dr.Reason != "" {
		b.WriteString(" — " + dr.Reason)
	}
	out := b.String()
	if r := []rune(out); len(r) > 200 {
		out = string(r[:199]) + "…"
	}
	return out
}

// Start reconciles once, then every interval, until Stop or ctx ends. One owner.
func (d *Drafts) Start(ctx context.Context) {
	d.startOnce.Do(func() {
		d.mu.Lock()
		d.started = true
		d.mu.Unlock()
		if err := d.Reconcile(ctx); err != nil {
			d.logger.Warn("ontology drafts: reconcile failed", "err", err)
		}
		go func() {
			defer close(d.done)
			t := time.NewTicker(d.every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-d.stopCh:
					return
				case <-t.C:
					if err := d.Reconcile(ctx); err != nil {
						d.logger.Warn("ontology drafts: reconcile failed", "err", err)
					}
				}
			}
		}()
	})
}

// Stop ends the reconcile loop. Idempotent; safe without Start.
func (d *Drafts) Stop() {
	d.stopOnce.Do(func() {
		close(d.stopCh)
		d.mu.Lock()
		started := d.started
		d.mu.Unlock()
		if started {
			<-d.done
		}
	})
}
