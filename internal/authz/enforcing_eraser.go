package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/cambrian-sh/core/domain"
)

// ErrEraseDenied is domain.ErrEraseDenied, named here beside the other chokepoint
// sentinels (ErrScopeMissing, ErrWriteDenied). The RPC boundary maps it to
// PermissionDenied; ErrNothingToErase maps to NotFound (ADR-0137 D3).
var ErrEraseDenied = domain.ErrEraseDenied

// EraserPolicy is the kernel's erasure configuration the chokepoint applies
// (execution.erasure, read in kernel.NewMemoryStack).
type EraserPolicy struct {
	// AgentErase lets agent (and person) principals erase what they wrote. Off: only
	// operators and the kernel erase (ADR-0137 D10 agent_erase_enabled, default off).
	AgentErase bool
	// MaxTargets bounds the ids one request may name (max_targets_per_request); 0 = no
	// bound.
	MaxTargets int
}

// EnforcingEraser is the ONE erasure chokepoint (ADR-0137 D3, S3). It holds the only
// reference to the raw domain.MemoryEraser and is built only in kernel.NewMemoryStack,
// which exposes nothing else that can erase. Constructor-injected (AGENTS.md §2.3): it
// cannot be built without the decision point.
//
// Per request:
//
//   - WHO asks comes from ctx (domain.PrincipalFromContext, the roles the operator
//     interceptor stamped), never from the request: the request's Principal and
//     PrincipalKind are overwritten (INV-5). The kernel (PrincipalSystem) is "kernel";
//     a person carrying the operator/admin role is "operator"; everyone else is an
//     "agent" principal.
//   - The read predicate must be on ctx (domain.WithScope), exactly as by-id reads:
//     absent → ErrScopeMissing (fail closed). A record the predicate does not admit is
//     ABSENT to this caller — not denied — so a denial never confirms that someone
//     else's record exists (ADR-0137 D3, ReasonNotOwner's rule).
//   - An agent principal may erase a record only when (a) the predicate admits it AND
//     (b) its kernel-stamped writer of record equals the caller's WriterID. A record
//     with no writer of record (legacy rows, experiences, substrate entities) is
//     operator-only. Entity and experience selectors are operator/kernel-only.
//   - Every admitted record is then put to Authorizer.AuthorizeErase, which can only
//     narrow (OSS allows; premium checks its policy).
//   - All-or-nothing: one denied record fails the request and nothing is erased; the
//     raw eraser runs with only the admitted ids, so a hidden id never reaches it.
type EnforcingEraser struct {
	inner  domain.MemoryEraser
	authz  domain.Authorizer
	policy EraserPolicy
	logger *slog.Logger
}

var _ domain.MemoryEraser = (*EnforcingEraser)(nil)

// NewEnforcingEraser wraps the raw eraser. A nil inner or authorizer panics: an eraser
// without a decision point must not exist (kernel.NewMemoryStack passes the resolved
// one). A nil logger falls back to slog.Default().
func NewEnforcingEraser(inner domain.MemoryEraser, a domain.Authorizer, policy EraserPolicy, logger *slog.Logger) *EnforcingEraser {
	if inner == nil || a == nil {
		panic("authz.NewEnforcingEraser: nil eraser or authorizer")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcingEraser{inner: inner, authz: a, policy: policy, logger: logger}
}

// eraseCaller is who asks, as the chokepoint established it.
type eraseCaller struct {
	principal domain.PrincipalRef
	kind      domain.ErasurePrincipalKind
	pred      *domain.TagPredicate
	surface   domain.SurfaceRef
}

func (e *EnforcingEraser) caller(ctx context.Context) (eraseCaller, error) {
	p := domain.PrincipalFromContext(ctx)
	pred, present := domain.ScopeFromContext(ctx)
	if p.IsZero() || !present {
		return eraseCaller{}, ErrScopeMissing
	}
	c := eraseCaller{principal: p, pred: pred, surface: domain.SurfaceFromContext(ctx), kind: domain.ErasureByAgent}
	switch {
	case p.Kind == domain.PrincipalSystem:
		c.kind = domain.ErasureByKernel
	case p.Kind == domain.PrincipalUser && hasOperatorRole(domain.PrincipalRolesFromContext(ctx)):
		c.kind = domain.ErasureByOperator
	}
	if pred == nil {
		// A present-but-nil predicate: no read is authorized at all, so nothing is
		// visible and nothing erasable.
		return c, fmt.Errorf("%w: no read is authorized for %s", ErrEraseDenied, p)
	}
	return c, nil
}

// hasOperatorRole mirrors the operator plane's role rule (operator or admin).
func hasOperatorRole(roles []string) bool {
	return slices.Contains(roles, "operator") || slices.Contains(roles, "admin")
}

// Erase implements domain.MemoryEraser.
func (e *EnforcingEraser) Erase(ctx context.Context, req domain.ErasureRequest) (domain.ErasureReceipt, error) {
	c, err := e.caller(ctx)
	if err != nil {
		return domain.ErasureReceipt{}, err
	}
	req.Principal, req.PrincipalKind = c.principal.WriterID(), c.kind
	if err := req.Validate(); err != nil {
		return domain.ErasureReceipt{}, err
	}
	req.Selector = req.Selector.Normalized()
	if e.policy.MaxTargets > 0 && req.Selector.Count() > e.policy.MaxTargets {
		return domain.ErasureReceipt{}, fmt.Errorf("%w: %d ids exceed execution.erasure.max_targets_per_request (%d)",
			domain.ErrInvalidErasureRequest, req.Selector.Count(), e.policy.MaxTargets)
	}
	if c.kind == domain.ErasureByAgent {
		if !e.policy.AgentErase {
			return domain.ErasureReceipt{}, e.deny(ctx, c, "agent erasure is disabled (execution.erasure.agent_erase_enabled)")
		}
		if len(req.Selector.EntityRefs) > 0 || len(req.Selector.ExperienceIDs) > 0 {
			return domain.ErasureReceipt{}, e.deny(ctx, c, "entity and experience selectors are operator-only")
		}
	}

	// D6 idempotency through the chokepoint (ENG-411 stage C): a retry of a request
	// that already executed must return its receipt, but re-resolving would find the
	// erased records gone and answer NothingToErase. The id is the caller's own (its
	// WriterID and kind were stamped above, never the request's), so a stored receipt
	// under it is the caller's own act; anything else falls through to the full check.
	if prior, found, err := e.inner.Receipt(ctx, req.ID()); err != nil {
		return domain.ErasureReceipt{}, fmt.Errorf("erase: receipt lookup: %w", err)
	} else if found && prior.Principal == req.Principal && prior.PrincipalKind == req.PrincipalKind {
		prior.Replayed = true
		return prior, nil
	}

	if c.kind != domain.ErasureByKernel {
		admitted, err := e.admit(ctx, c, req.Selector)
		if err != nil {
			return domain.ErasureReceipt{}, err
		}
		if admitted.IsEmpty() {
			return domain.ErasureReceipt{}, &domain.NothingToEraseError{}
		}
		req.Selector = admitted
	}

	rec, err := e.inner.Erase(ctx, req)
	var nothing *domain.NothingToEraseError
	if errors.As(err, &nothing) && nothing.EarlierRequestID != "" && c.kind == domain.ErasureByAgent {
		// D6: the earlier request is named only when the caller may see it.
		if earlier, found, rerr := e.inner.Receipt(ctx, nothing.EarlierRequestID); rerr != nil || !found || earlier.Principal != req.Principal {
			return domain.ErasureReceipt{}, &domain.NothingToEraseError{}
		}
	}
	if err == nil {
		e.logger.InfoContext(ctx, "authz: erasure executed",
			slog.String("event", "authz_erase"),
			slog.String("principal", c.principal.String()),
			slog.String("principal_kind", string(c.kind)),
			slog.String("request_id", rec.RequestID),
			slog.Bool("replayed", rec.Replayed),
			slog.Int("targets", rec.Targets))
	}
	return rec, err
}

// admit resolves the selector and returns the selector narrowed to the ids this caller
// may see, or ErrEraseDenied when it may see one it may not erase.
func (e *EnforcingEraser) admit(ctx context.Context, c eraseCaller, sel domain.ErasureSelector) (domain.ErasureSelector, error) {
	cands, err := e.inner.Resolve(ctx, sel)
	if err != nil {
		return domain.ErasureSelector{}, fmt.Errorf("erase: resolve: %w", err)
	}
	// A selector entry (a document, a chunk, a thread) is kept when at least one of its
	// records is visible; a thread is all-or-nothing over its documents: a thread
	// holding a document the caller cannot see is erased only as far as it can see,
	// which the raw eraser cannot express, so the thread's visible documents are
	// named instead of the thread.
	//
	// A document candidate whose Via is a chunk id (not a thread) is a chunk selector
	// widened to its document (ENG-1947): it is named by its document id.
	var out domain.ErasureSelector
	threads := map[string]bool{}
	for _, th := range sel.ThreadIDs {
		threads[th] = true
	}
	keptThreadDocs := map[string]bool{}
	for _, cand := range cands {
		if _, ok := c.pred.Check(cand.Tags); !ok {
			continue // hidden = absent
		}
		if c.kind == domain.ErasureByAgent {
			if cand.WrittenBy == "" || cand.WrittenBy != c.principal.WriterID() {
				return domain.ErasureSelector{}, e.denyRecord(ctx, c, cand, domain.ReasonNotWriter,
					"the caller is not the record's writer of record")
			}
		}
		if dec := e.authz.AuthorizeErase(ctx, c.principal, c.surface, cand); !dec.Allowed {
			return domain.ErasureSelector{}, e.denyRecord(ctx, c, cand, dec.Reason, dec.Detail)
		}
		switch cand.Kind {
		case domain.ErasureTargetDocument:
			if cand.Via != cand.ID && threads[cand.Via] {
				keptThreadDocs[cand.ID] = true
				continue
			}
			out.DocumentIDs = append(out.DocumentIDs, cand.ID)
		case domain.ErasureTargetChunk:
			out.ChunkIDs = append(out.ChunkIDs, cand.ID)
		case domain.ErasureTargetExperience:
			out.ExperienceIDs = append(out.ExperienceIDs, cand.ID)
		case domain.ErasureTargetEntity:
			out.EntityRefs = append(out.EntityRefs, cand.ID)
		}
	}
	// Thread selectors: keep the thread itself when every document it resolves to was
	// admitted (its tombstone then names the thread); otherwise name the admitted
	// documents.
	for _, th := range sel.ThreadIDs {
		all, seen := true, false
		for _, cand := range cands {
			if cand.Via != th || cand.Kind != domain.ErasureTargetDocument {
				continue
			}
			if keptThreadDocs[cand.ID] {
				seen = true
			} else {
				all = false
			}
		}
		switch {
		case seen && all:
			out.ThreadIDs = append(out.ThreadIDs, th)
		case seen:
			for _, cand := range cands {
				if cand.Via == th && keptThreadDocs[cand.ID] {
					out.DocumentIDs = append(out.DocumentIDs, cand.ID)
				}
			}
		}
	}
	return out.Normalized(), nil
}

func (e *EnforcingEraser) deny(ctx context.Context, c eraseCaller, detail string) error {
	e.logger.WarnContext(ctx, "authz: erasure denied",
		slog.String("event", "authz_erase_deny"),
		slog.String("principal", c.principal.String()),
		slog.String("detail", detail))
	return fmt.Errorf("%w: %s", ErrEraseDenied, detail)
}

func (e *EnforcingEraser) denyRecord(ctx context.Context, c eraseCaller, cand domain.ErasureCandidate, reason domain.DecisionReason, detail string) error {
	e.logger.WarnContext(ctx, "authz: erasure denied",
		slog.String("event", "authz_erase_deny"),
		slog.String("principal", c.principal.String()),
		slog.String("resource", cand.AuthzRef().String()),
		slog.String("reason", string(reason)),
		slog.String("detail", detail))
	return fmt.Errorf("%w: %s %s (%s)", ErrEraseDenied, cand.Kind, cand.ID, reason)
}

// Receipt implements domain.MemoryEraser: an operator or the kernel sees every
// receipt; any other principal only its own (another's reads as not found, D8).
func (e *EnforcingEraser) Receipt(ctx context.Context, requestID string) (domain.ErasureReceipt, bool, error) {
	c, err := e.caller(ctx)
	if err != nil {
		return domain.ErasureReceipt{}, false, err
	}
	rec, found, err := e.inner.Receipt(ctx, requestID)
	if err != nil || !found {
		return rec, found, err
	}
	if c.kind == domain.ErasureByAgent && rec.Principal != c.principal.WriterID() {
		return domain.ErasureReceipt{}, false, nil
	}
	return rec, true, nil
}

// Resolve implements domain.MemoryEraser: the candidates this caller can see (hidden =
// absent), the basis of a preview (job C). It does not decide erasability.
func (e *EnforcingEraser) Resolve(ctx context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	c, err := e.caller(ctx)
	if err != nil {
		return nil, err
	}
	cands, err := e.inner.Resolve(ctx, sel)
	if err != nil {
		return nil, err
	}
	out := cands[:0]
	for _, cand := range cands {
		if _, ok := c.pred.Check(cand.Tags); ok {
			out = append(out, cand)
		}
	}
	return out, nil
}

// ListReceipts implements domain.MemoryEraser: an operator or the kernel lists every
// receipt (or those of the principal it names); any other principal only its own,
// whatever filter it asked for (ADR-0137 D8).
func (e *EnforcingEraser) ListReceipts(ctx context.Context, f domain.ErasureReceiptFilter) ([]domain.ErasureReceipt, error) {
	c, err := e.caller(ctx)
	if err != nil {
		return nil, err
	}
	if c.kind == domain.ErasureByAgent {
		f.Principal = c.principal.WriterID()
	}
	return e.inner.ListReceipts(ctx, f)
}
