package config

import "strings"

// The ADR-0145 phase-3 chat keys of execution.chat (ChatConfig, config.go):
// defaults and validation. Read once at the composition root
// (app/chat_grounding.go → chat.TurnService.Grounding).

// Reply check modes (execution.chat.reply_check).
const (
	ReplyCheckOff     = "off"
	ReplyCheckObserve = "observe"
	ReplyCheckEnforce = "enforce"
)

// ReplyCheckMode is ReplyCheck normalised: "" is off.
func (c ChatConfig) ReplyCheckMode() string {
	switch m := strings.ToLower(strings.TrimSpace(c.ReplyCheck)); m {
	case "", ReplyCheckOff:
		return ReplyCheckOff
	default:
		return m
	}
}

// TurnIndexWindow is TurnIndexTurns with its default applied.
func (c ChatConfig) TurnIndexWindow() int {
	if c.TurnIndexTurns > 0 {
		return c.TurnIndexTurns
	}
	return 8
}

// ReplyCheckMinInt is ReplyCheckMinInteger with its default applied.
func (c ChatConfig) ReplyCheckMinInt() int {
	if c.ReplyCheckMinInteger > 0 {
		return c.ReplyCheckMinInteger
	}
	return 10
}

// ReplyCheckEntries is ReplyCheckMaxEntries with its default applied.
func (c ChatConfig) ReplyCheckEntries() int {
	if c.ReplyCheckMaxEntries > 0 {
		return c.ReplyCheckMaxEntries
	}
	return 200
}

// NeedsLedger reports whether any key that reads or writes the run ledger is on.
func (c ChatConfig) NeedsLedger() bool {
	return c.TurnIndex || c.GroundedReply || c.ReplyCheckMode() != ReplyCheckOff || c.LedgerTools || c.AskBack
}

// validateGrounding checks the phase-3 chat keys. ledgerOn is execution.ledger.enabled.
func (c ChatConfig) validateGrounding(ledgerOn bool) []string {
	var errs []string
	switch c.ReplyCheckMode() {
	case ReplyCheckOff, ReplyCheckObserve, ReplyCheckEnforce:
	default:
		errs = append(errs, "execution.chat.reply_check must be off, observe or enforce")
	}
	if c.TranscriptBudgetTokens < 0 || c.TurnIndexTurns < 0 || c.ReplyCheckMinInteger < 0 || c.ReplyCheckMaxEntries < 0 {
		errs = append(errs, "execution.chat.transcript_budget_tokens, turn_index_turns, reply_check_min_integer and reply_check_max_entries must be >= 0")
	}
	if c.NeedsLedger() && !ledgerOn {
		errs = append(errs, "execution.chat.{turn_index, grounded_reply, reply_check, ledger_tools, ask_back} require execution.ledger.enabled: the turn's evidence and state live in the run ledger")
	}
	return errs
}
