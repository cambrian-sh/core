package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// The chat lane's own ledger entries (kind computed, stamped with the
// conversation and the turn). Tool names are the entry's call key, so each is
// found by a ledger query and never collides with a tool a worker called.
const (
	toolTurnIndex   = "chat_turn_index"
	toolSummary     = "chat_conversation_summary"
	toolReplyCheck  = "chat_reply_check"
	toolRequirement = "requirement_coverage"
	// stateQueryLimit bounds one query for the chat lane's own entries of a
	// conversation (one per delegated turn, per summary): far above any
	// conversation's count, and a query never spans conversations.
	stateQueryLimit = 1000
)

// recordState writes one chat-lane entry for the turn. derived names the ledger
// entries its content was built from, so it is never easier to read than they are
// (domain.LedgerEntryReadable). Best-effort: a failed write is logged and the turn
// goes on.
func (s *TurnService) recordState(ctx context.Context, conv *domain.Conversation, turnID, tool string, v any, derived []string) (string, bool) {
	if !s.ledgerOn() {
		return "", false
	}
	body, err := json.Marshal(v)
	if err != nil {
		slog.Warn("chat: turn state not encoded", "tool", tool, "err", err)
		return "", false
	}
	e, err := s.g.ledgerW.Record(ctx, domain.LedgerRecord{
		Kind: domain.LedgerComputed,
		Provenance: domain.LedgerProvenance{
			ConversationID: conv.ID, TurnID: turnID, StepIndex: -1, Owner: conv.OwnerID,
		},
		ToolName:    tool,
		Writer:      domain.SystemPrincipal,
		DerivedFrom: derived,
		ContentType: "application/json",
		Body:        body,
	})
	if err != nil {
		slog.Warn("chat: turn state not recorded in the ledger", "tool", tool, "conversation", conv.ID, "err", err)
		return "", false
	}
	return e.ID, true
}

// findState returns the conversation's chat-lane entries of one tool, oldest
// first, with their bodies.
func (s *TurnService) findState(ctx context.Context, conv *domain.Conversation, tool string) []domain.LedgerEntry {
	if !s.ledgerOn() {
		return nil
	}
	rctx := ownerScope(ctx, conv.OwnerID)
	list, err := s.g.ledgerR.Find(rctx, domain.LedgerQuery{
		ConversationID: conv.ID, ToolName: tool, Kind: domain.LedgerComputed, Limit: stateQueryLimit,
	})
	if err != nil {
		slog.Warn("chat: turn state query failed", "tool", tool, "conversation", conv.ID, "err", err)
		return nil
	}
	return list
}

// readBody reads one entry's body through the enforced reader.
func (s *TurnService) readBody(ctx context.Context, owner, id string) (*domain.LedgerEntry, error) {
	e, err := s.g.ledgerR.Get(ownerScope(ctx, owner), id)
	if err != nil {
		return nil, fmt.Errorf("ledger get %s: %w", id, err)
	}
	return e, nil
}

// turnEntries returns the ledger entries a turn produced (every tool result, step
// output and calculation of the runs it delegated), oldest first, at most limit.
func (s *TurnService) turnEntries(ctx context.Context, conv *domain.Conversation, turnID string, limit int) []domain.LedgerEntry {
	if !s.ledgerOn() || turnID == "" {
		return nil
	}
	list, err := s.g.ledgerR.Find(ownerScope(ctx, conv.OwnerID), domain.LedgerQuery{
		ConversationID: conv.ID, TurnID: turnID, Limit: limit,
	})
	if err != nil {
		slog.Warn("chat: the turn's ledger entries could not be read", "conversation", conv.ID, "turn", turnID, "err", err)
		return nil
	}
	return list
}
