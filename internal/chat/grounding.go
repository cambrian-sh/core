package chat

import (
	"context"
	"sync/atomic"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0145 phase 3, the chat side (T-20261108-chat-reply-is-composed-over-the-structured,
// T-20261108-chat-replies-state-figures-dates-and-ids-no,
// T-20261108-chat-agent-answers-organisation-questions-from,
// T-20261108-chat-context-is-a-fixed-40-message-window-with,
// T-20261108-a-delegated-request-that-needs-the-person-s).
//
// Every behaviour here is behind a Grounding key that defaults off, so a
// TurnService with a zero Grounding is the pre-phase-3 turn path unchanged.
//
//   - D2 conversation context (transcript.go, turnindex.go): the transcript is
//     budgeted by estimated tokens, older messages are condensed by a summary call
//     the KERNEL triggers when the transcript exceeds the budget, and every
//     delegated turn leaves an index entry (runs, checklist statuses, values by
//     name with ledger refs, actions with ids) the next turns are shown.
//   - D5a ask-back (turnindex.go): a delegated run that returned "input required"
//     is saved with the turn; the next message resumes it through the turn lease
//     (LeaseBinding.AskBackRound / AskBackPendingRef), never through agent text.
//   - D8 reply check (replyguard.go): the reply's figures, dates and ids are
//     checked against the turn's ledger entries (internal/replycheck), observe-only
//     or enforced with one retry, and the evidence markers are stripped before the
//     person sees the reply.
//
// Turn state (index entries, summaries, check reports) is stored as `computed`
// entries of the run ledger, the conversation-lifetime, scope-enforced store
// ADR-0145 D1 built for exactly this — no second store. Reads go through the
// enforced reader under the conversation owner's bypass-with-owner-fence
// predicate, the same one the DAG executor reads its run under.

// Grounding is the phase-3 chat configuration (execution.chat, config.ChatConfig).
type Grounding struct {
	// TranscriptBudgetTokens > 0 replaces the HistoryLimit message window with a
	// token budget and turns on the rolling summary.
	TranscriptBudgetTokens int
	TurnIndex              bool
	TurnIndexTurns         int
	GroundedReply          bool
	// ReplyCheck is "off", "observe" or "enforce".
	ReplyCheck           string
	ReplyCheckMinInteger int
	ReplyCheckMaxEntries int
	DelegationRule       bool
	LedgerTools          bool
	AskBack              bool
}

// Reply check modes.
const (
	ReplyCheckOff     = "off"
	ReplyCheckObserve = "observe"
	ReplyCheckEnforce = "enforce"
)

func (g Grounding) replyCheckMode() string {
	switch g.ReplyCheck {
	case ReplyCheckObserve, ReplyCheckEnforce:
		return g.ReplyCheck
	}
	return ReplyCheckOff
}

func (g Grounding) indexTurns() int {
	if g.TurnIndexTurns > 0 {
		return g.TurnIndexTurns
	}
	return 8
}

func (g Grounding) minInteger() int {
	if g.ReplyCheckMinInteger > 0 {
		return g.ReplyCheckMinInteger
	}
	return 10
}

func (g Grounding) maxEntries() int {
	if g.ReplyCheckMaxEntries > 0 {
		return g.ReplyCheckMaxEntries
	}
	return 200
}

// Summarizer condenses older conversation messages, given the previous summary
// (D2.3). It is a kernel organ call (summary.go); the chat agent never asks for it.
type Summarizer func(ctx context.Context, previous string, msgs []domain.Message) (string, error)

// TurnResultSource returns the structured results of the runs a turn delegated
// directly (network.TurnDelegations.TurnResults).
type TurnResultSource func(originMessageID string) []domain.ExecuteResult

// groundingDeps are the ports the phase-3 behaviours read, set at the
// composition root (app/chat_grounding.go).
type groundingDeps struct {
	ledgerW   domain.LedgerWriter
	ledgerR   domain.LedgerReader
	results   TurnResultSource
	summarize Summarizer
	tracer    domain.RunTracer

	// Reply check counters (ReplyCheckStats).
	checkedTurns atomic.Int64
	violations   atomic.Int64
	retries      atomic.Int64
}

// SetLedger wires the run ledger's classified writer and ENFORCED reader
// (kernel.NewLedgerStack). Without them the ledger-backed behaviours are off.
func (s *TurnService) SetLedger(w domain.LedgerWriter, r domain.LedgerReader) {
	s.g.ledgerW, s.g.ledgerR = w, r
}

// SetTurnResults wires the source of a turn's delegated run results.
func (s *TurnService) SetTurnResults(f TurnResultSource) { s.g.results = f }

// SetSummarizer wires the rolling-summary organ call.
func (s *TurnService) SetSummarizer(f Summarizer) { s.g.summarize = f }

// SetRunTracer wires the run trace the reply check reports to (ADR-0145 D10), so
// the harness collects the check's events beside the turn's model calls.
func (s *TurnService) SetRunTracer(t domain.RunTracer) { s.g.tracer = t }

// ReplyCheckStats reports how many replies were checked, how many violations the
// checks found in total, and how many enforced retries ran, since start.
func (s *TurnService) ReplyCheckStats() (checked, violations, retries int64) {
	return s.g.checkedTurns.Load(), s.g.violations.Load(), s.g.retries.Load()
}

func (s *TurnService) ledgerOn() bool { return s.g.ledgerW != nil && s.g.ledgerR != nil }

// ownerScope is the predicate the chat lane reads the conversation's ledger
// entries under: the kernel acting for the conversation owner (the explicit,
// greppable bypass of AGENTS.md §2.4, with the ADR-0142 owner fence applied).
func ownerScope(ctx context.Context, owner string) context.Context {
	p := &domain.TagPredicate{Bypass: true}
	if owner != "" {
		p.Owners = []string{owner}
	}
	return domain.WithScope(ctx, p)
}
