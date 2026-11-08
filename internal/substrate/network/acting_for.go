package network

import (
	"context"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// PrincipalNameSource is the narrowing of domain.PrincipalRegistry the step
// dispatcher reads: a principal's display name, for the acting-for keys
// (domain.HandoffKeyActingForName). *identity registries implement it.
type PrincipalNameSource interface {
	GetPrincipal(ctx context.Context, id string) (domain.Principal, error)
}

// actingForRun resolves whom a plan run's steps act for: the run principal of a
// step lease of this run — the binding stepFn stamps on every step lease
// ({SessionID, RunID}) — through the SAME RunPrincipal function ExecuteTool uses
// to choose a per_user credential (T-20261107-delegated-steps-name-the-conversation-owner-in).
// So the name a worker is shown is, by construction, the principal its tools act
// as. Resolved once per Execute: the session's conversation link is written
// before planning and a run's principal does not change mid-run.
//
// Returns ("", "") with no resolver or no principal; the display name is "" when
// no registry is wired or the registry does not know the id (best-effort: a
// missing name never withholds the id).
func (s *Server) actingForRun(ctx context.Context, sessionID domain.SessionID, runID domain.RunID) (id, name string) {
	if s.RunPrincipal == nil {
		return "", ""
	}
	id = strings.TrimSpace(s.RunPrincipal(ctx, domain.LeaseBinding{SessionID: sessionID, RunID: runID, StepIndex: -1}))
	if id == "" {
		return "", ""
	}
	if s.PrincipalNames != nil {
		if p, err := s.PrincipalNames.GetPrincipal(ctx, id); err == nil {
			name = strings.TrimSpace(p.DisplayName)
		}
	}
	return id, name
}
