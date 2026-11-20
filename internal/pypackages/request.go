package pypackages

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/pyenv"
)

// Request results (ADR-0157 D7.9.3): what request_python_package tells the agent.
const (
	RequestInstalled        = "installed"
	RequestPending          = "pending"
	RequestDenied           = "denied"
	RequestUnresolvable     = "unresolvable"
	RequestAlreadyInstalled = "already_installed"
)

// RequestResult is one runtime request's answer.
type RequestResult struct {
	Status     string `json:"status"`
	ApprovalID string `json:"approval_id,omitempty"`
	Detail     string `json:"detail"`
}

// Request is the runtime path behind the request_python_package kernel tool
// (ADR-0157 D7.9, P4). It resolves the package against the base env (no card for
// an unresolvable one), opens or reuses the deterministic record for (agent,
// package, version, lock) — so a repeat while a card is pending returns that same
// card, never a second — raises its python_package card for the Operator role, and
// waits up to wait for the answer. Approved inside the window ⇒ the agent's layer
// is installed now ("installed": usable from the agent's next instance; a live one
// is recycled when idle). Still unanswered ⇒ "pending": the card stays open and an
// approval later applies to the agent's later runs. The agent never runs pip; the
// install goes through the same hash-checked path as a declaration (D7.7).
func (s *Service) Request(ctx context.Context, agentID string, pkg domain.PythonPackage, rationale, requestedBy string, wait time.Duration) (RequestResult, error) {
	if err := domain.ValidatePythonPackages([]domain.PythonPackage{pkg}); err != nil {
		return RequestResult{Status: RequestUnresolvable, Detail: err.Error()}, nil
	}
	if s.store == nil {
		return RequestResult{}, fmt.Errorf("this kernel keeps no durable package approvals (migration 0057_python_package_approvals not applied)")
	}
	if s.cfg.Base == nil {
		return RequestResult{}, fmt.Errorf("the managed Python env is not available on this kernel")
	}
	base, err := s.cfg.Base()
	if err != nil {
		return RequestResult{}, fmt.Errorf("the managed Python env is not ready: %w", err)
	}
	spec := pkg.Spec()
	// The layer the agent had before this request: installed vs
	// already_installed is judged against it (the hub handler may install
	// the approved package in the background before this call looks).
	before, known := s.deliveredLayer(agentID)
	if !known {
		before = ""
	}

	unlock := s.lockAgent(agentID)
	recs, err := s.store.ForTarget(ctx, agentID)
	if err != nil {
		unlock()
		return RequestResult{}, fmt.Errorf("python package approvals unreadable: %w", err)
	}
	o, err := s.ensurePackage(ctx, agentID, pkg, base, recs, requestedBy, rationale)
	unlock()
	switch {
	case errors.Is(err, ErrDeclaration):
		return RequestResult{Status: RequestUnresolvable, Detail: unresolvableDetail(spec, err)}, nil
	case err != nil:
		return RequestResult{}, err
	}
	switch o.state {
	case "base":
		return RequestResult{Status: RequestAlreadyInstalled, Detail: spec + " is already in the shared base environment; import it directly"}, nil
	case domain.PythonPackageDenied:
		return RequestResult{Status: RequestDenied, ApprovalID: o.card, Detail: deniedDetail(spec, o.rec)}, nil
	case domain.PythonPackageApproved:
		return s.install(ctx, agentID, base, spec, o.card, before)
	}
	// Pending: wait for the answer, never longer than the window.
	poll := s.cfg.RequestPoll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return pendingResult(spec, o.card), nil
		case <-deadline.C:
			return pendingResult(spec, o.card), nil
		case <-tick.C:
		}
		r, err := s.store.Get(ctx, o.rec.ID)
		if err != nil {
			continue
		}
		unlock := s.lockAgent(agentID)
		cur, aerr := s.act(ctx, r)
		unlock()
		if aerr != nil {
			continue
		}
		switch cur.state {
		case domain.PythonPackageApproved:
			return s.install(ctx, agentID, base, spec, cur.card, before)
		case domain.PythonPackageDenied:
			return RequestResult{Status: RequestDenied, ApprovalID: cur.card, Detail: deniedDetail(spec, cur.rec)}, nil
		}
		o.card = cur.card // an expired card was raised again
	}
}

// install builds (or finds) the agent's layer with every approved package and
// reports it. When the layer is the one the agent already had before the
// request (before), the package was already in it: already_installed.
func (s *Service) install(ctx context.Context, agentID string, base pyenv.Interpreter, spec, card, before string) (RequestResult, error) {
	var decl []domain.PythonPackage
	if s.cfg.Declared != nil {
		decl = s.cfg.Declared(ctx, agentID)
	}
	unlock := s.lockAgent(agentID)
	l, err := s.ensureLocked(ctx, agentID, decl, base)
	unlock()
	if err != nil {
		return RequestResult{Status: RequestPending, ApprovalID: card,
			Detail: fmt.Sprintf("%s is approved but its install has not completed: %v. The kernel retries at your next start.", spec, err)}, nil
	}
	if before != "" && l.Dir != "" && before == filepath.Clean(l.Dir) {
		return RequestResult{Status: RequestAlreadyInstalled, ApprovalID: card, Detail: spec + " is already installed in your layer"}, nil
	}
	return RequestResult{Status: RequestInstalled, ApprovalID: card,
		Detail: spec + " is approved and installed in your own layer; it is importable from your next run (this process keeps the environment it started with and is replaced when it is idle)"}, nil
}

func pendingResult(spec, card string) RequestResult {
	return RequestResult{Status: RequestPending, ApprovalID: card,
		Detail: "no operator decision yet on " + spec + "; the request stays open, and if it is approved the package is installed for your later runs. Asking again returns this same request."}
}

func deniedDetail(spec string, r domain.PythonPackageApproval) string {
	by := r.DecidedBy
	if by == "" {
		by = "an operator"
	}
	return fmt.Sprintf("%s was denied by %s; do the work without it, or ask the person to raise it with an operator", spec, by)
}

func unresolvableDetail(spec string, err error) string {
	return fmt.Sprintf("%s cannot be installed: %v (no approval was raised)", spec, err)
}
