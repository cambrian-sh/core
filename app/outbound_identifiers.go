package app

import (
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/identifiers"
)

// outboundIdentifierGuard reads execution.tools.outbound_identifier_guard into the
// tool executor's OutboundIdentifiers control (owner decision 2026-11-15,
// T-20261108-outbound-tool-writes-have-no-deterministic, ENG-917): the one
// identifier detector (internal/identifiers, shared with the chat disclosure guard)
// over the text arguments of every call that can send outside. Off ⇒ nil.
func outboundIdentifierGuard(cfg config.OutboundIdentifierGuardConfig) (*domain.OutboundIdentifierGuard, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	return domain.NewOutboundIdentifierGuard(identifiers.Detector{}, cfg.IdentifierKinds())
}
