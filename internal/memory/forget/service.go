// Package forget is the two-phase principal-initiated forget of ADR-0137 D2 (ENG-411
// stage C): Preview resolves a selector to the concrete records the caller can see and
// hands back a deterministic token; Forget executes exactly those records (or explicit
// ids) through the ONE erasure chokepoint, MemoryStack.Eraser() (authz.EnforcingEraser).
//
// It is the single service behind every forget door — the agent-plane PreviewForget /
// Forget / ListMyErasures RPCs, the operator-plane PreviewForgetMemory / ForgetMemory /
// ListErasures / GetErasure RPCs and the forget_memory MCP tool — so the two-phase rule
// and the disabled answer are the same everywhere (AGENTS.md §4.3).
//
// It decides nothing about who may erase what: the caller's principal and read
// predicate ride ctx (AGENTS.md §2.4) and the enforcing eraser applies them, at preview
// (hidden = absent) and again at execution (a preview is never a capability). The kernel
// never asks a model which rows to delete: a natural-language forget is the agent's own
// search followed by a preview of the ids it found.
package forget

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Config is the forget service's configuration (execution.erasure).
type Config struct {
	// PreviewTTL is how long a preview token stays executable (preview_ttl_seconds).
	PreviewTTL time.Duration
	// PolicyFingerprint identifies the erasure policy in force; it is part of every
	// token, so a policy change invalidates outstanding previews.
	PolicyFingerprint string
	// MaxPreviews bounds the outstanding previews held in memory; 0 ⇒ 4096. When full,
	// expired previews go first, then the soonest to expire.
	MaxPreviews int
	// Now is the clock for preview expiry only (never an identifier); nil ⇒ time.Now.
	Now func() time.Time
}

// Preview is one resolved forget preview: content-free (ids and kinds only).
type Preview struct {
	// Token executes exactly Selector for the same caller until ExpiresAt.
	Token string
	// Candidates are the records the caller can see that the selector names.
	Candidates []domain.ErasureCandidate
	// Selector is the concrete selector Forget will execute for Token.
	Selector  domain.ErasureSelector
	ExpiresAt time.Time
}

// Request is one Forget: a preview token, or explicit ids — exactly one.
type Request struct {
	PreviewToken string
	Selector     domain.ErasureSelector
	Reason       domain.ErasureReason
}

type preview struct {
	principal string
	selector  domain.ErasureSelector
	expires   time.Time
}

// Service is the forget service. The zero value is not usable; build with New.
type Service struct {
	eraser domain.MemoryEraser
	cfg    Config

	mu       sync.Mutex
	previews map[string]preview
}

// New builds the service over the enforcing eraser (MemoryStack.Eraser()). A nil
// eraser is legal — erasure is off — and every method then answers
// domain.ErrErasureDisabled.
func New(eraser domain.MemoryEraser, cfg Config) *Service {
	if cfg.PreviewTTL <= 0 {
		cfg.PreviewTTL = 10 * time.Minute
	}
	if cfg.MaxPreviews <= 0 {
		cfg.MaxPreviews = 4096
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{eraser: eraser, cfg: cfg, previews: map[string]preview{}}
}

// Enabled reports whether an eraser is composed.
func (s *Service) Enabled() bool { return s != nil && s.eraser != nil }

func (s *Service) eraserOrDisabled() (domain.MemoryEraser, error) {
	if !s.Enabled() {
		return nil, domain.ErrErasureDisabled
	}
	return s.eraser, nil
}

// Preview resolves sel to the records the caller (principal + predicate on ctx) can
// see, and returns them with a token that executes exactly those records. Nothing
// visible → domain.ErrNothingToErase; no principal or predicate → the chokepoint's
// ErrScopeMissing.
func (s *Service) Preview(ctx context.Context, sel domain.ErasureSelector) (Preview, error) {
	e, err := s.eraserOrDisabled()
	if err != nil {
		return Preview{}, err
	}
	sel = sel.Normalized()
	if sel.IsEmpty() {
		return Preview{}, fmt.Errorf("%w: the selector names nothing", domain.ErrInvalidErasureRequest)
	}
	cands, err := e.Resolve(ctx, sel)
	if err != nil {
		return Preview{}, err
	}
	concrete := domain.SelectorOfCandidates(cands)
	if concrete.IsEmpty() {
		return Preview{}, &domain.NothingToEraseError{}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Kind != cands[j].Kind {
			return cands[i].Kind < cands[j].Kind
		}
		return cands[i].ID < cands[j].ID
	})
	principal := domain.PrincipalFromContext(ctx).String()
	token := domain.ErasurePreviewToken(principal, concrete, s.cfg.PolicyFingerprint)
	expires := s.cfg.Now().Add(s.cfg.PreviewTTL)
	s.remember(token, preview{principal: principal, selector: concrete, expires: expires})
	return Preview{Token: token, Candidates: cands, Selector: concrete, ExpiresAt: expires}, nil
}

func (s *Service) remember(token string, p preview) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	if _, ok := s.previews[token]; !ok && len(s.previews) >= s.cfg.MaxPreviews {
		for t, old := range s.previews {
			if !now.Before(old.expires) {
				delete(s.previews, t)
			}
		}
		for len(s.previews) >= s.cfg.MaxPreviews {
			var soonest string
			for t, old := range s.previews {
				if soonest == "" || old.expires.Before(s.previews[soonest].expires) {
					soonest = t
				}
			}
			delete(s.previews, soonest)
		}
	}
	s.previews[token] = p
}

// lookup returns the preview of token for principal, or ErrPreviewUnknown (unknown,
// expired, or another caller's — indistinguishable on purpose).
func (s *Service) lookup(token, principal string) (domain.ErasureSelector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.previews[token]
	if ok && !s.cfg.Now().Before(p.expires) {
		delete(s.previews, token)
		ok = false
	}
	if !ok || p.principal != principal {
		return domain.ErasureSelector{}, domain.ErrPreviewUnknown
	}
	return p.selector, nil
}

// Forget executes one erasure: the records a preview token resolved (for the caller
// that previewed them, before it expired) or explicit ids. Re-authorized in full by the
// chokepoint. A retry of the same act returns the same receipt (Replayed), because the
// request id is deterministic (ADR-0137 D6) — the token stays valid until it expires so
// a retry after a lost answer works.
func (s *Service) Forget(ctx context.Context, req Request) (domain.ErasureReceipt, error) {
	e, err := s.eraserOrDisabled()
	if err != nil {
		return domain.ErasureReceipt{}, err
	}
	sel := req.Selector.Normalized()
	switch {
	case req.PreviewToken != "" && !sel.IsEmpty():
		return domain.ErasureReceipt{}, fmt.Errorf("%w: name a preview token or explicit ids, not both", domain.ErrInvalidErasureRequest)
	case req.PreviewToken != "":
		sel, err = s.lookup(req.PreviewToken, domain.PrincipalFromContext(ctx).String())
		if err != nil {
			return domain.ErasureReceipt{}, err
		}
	case sel.IsEmpty():
		return domain.ErasureReceipt{}, fmt.Errorf("%w: a preview token or explicit ids are required", domain.ErrInvalidErasureRequest)
	}
	reason := req.Reason
	if reason == "" {
		reason = domain.ErasureReasonUserRequest
	}
	// Principal and kind are overwritten from ctx by the chokepoint (never trusted from
	// a request); Validate needs them set to pass, so they are filled from ctx here.
	rec, err := e.Erase(ctx, domain.ErasureRequest{
		Principal:     domain.PrincipalFromContext(ctx).WriterID(),
		PrincipalKind: domain.ErasureByAgent,
		Reason:        reason,
		Selector:      sel,
	})
	if err != nil {
		return domain.ErasureReceipt{}, err
	}
	return rec, nil
}

// Receipt returns one receipt the caller may see (found=false otherwise).
func (s *Service) Receipt(ctx context.Context, requestID string) (domain.ErasureReceipt, bool, error) {
	e, err := s.eraserOrDisabled()
	if err != nil {
		return domain.ErasureReceipt{}, false, err
	}
	if requestID == "" {
		return domain.ErasureReceipt{}, false, fmt.Errorf("%w: request id is required", domain.ErrInvalidErasureRequest)
	}
	return e.Receipt(ctx, requestID)
}

// List lists the receipts the caller may see, newest first.
func (s *Service) List(ctx context.Context, f domain.ErasureReceiptFilter) ([]domain.ErasureReceipt, error) {
	e, err := s.eraserOrDisabled()
	if err != nil {
		return nil, err
	}
	return e.ListReceipts(ctx, f)
}

// IsCallerError reports whether err is the caller's fault (a malformed request or an
// unknown preview) rather than a decision or a store failure — the RPC boundary's
// InvalidArgument / FailedPrecondition split.
func IsCallerError(err error) bool {
	return errors.Is(err, domain.ErrInvalidErasureRequest) || errors.Is(err, domain.ErrPreviewUnknown)
}
