package operator

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// Seams a premium operator plane needs to behave like an OperatorConsole
// command (ADR-0141; first consumer: premium's AccountsAdmin plane). A plugin
// cannot import this package, so app/ hands these to it as functions.

// RecordAudit writes one audit entry through the same store + feed as every
// operator write. For changes that happen OUTSIDE an RPC (an OAuth callback,
// a dead refresh grant): the caller supplies the actor and a deterministic
// command id.
func (s *Service) RecordAudit(ctx context.Context, e domain.AuditEntry) error {
	_, err := s.recordAndEmit(ctx, e)
	return err
}

// RunPluginMutation runs one plugin-plane command with the OperatorConsole
// mutation contract: command_id + reason required, the audit row written
// first (actor and role from the authenticated caller), a replayed
// command_id deduped (apply is not run again). It is runMutation, exported
// for plugin planes so they do not re-implement dedupe and audit.
func (s *Service) RunPluginMutation(ctx context.Context, commandID, reason, action, targetType, targetID string, apply func() error) (deduped bool, err error) {
	ack, err := s.runMutation(ctx, commandID, reason, action, targetType, targetID, "", apply)
	if err != nil {
		return false, err
	}
	return ack.GetDeduped(), nil
}

// CallerFromContext is the authenticated operator-plane caller: its principal
// id and whether it holds the Operator role. ok=false when the call carried
// no authenticated principal.
func CallerFromContext(ctx context.Context) (principal string, isOperator bool, ok bool) {
	p, role, ok := PrincipalFromContext(ctx)
	if !ok || p == "" {
		return "", false, false
	}
	return p, role == RoleOperator, true
}
