package domain

import (
	"context"
	"time"
)

// The CL-2 consent/ladder prompt seam (ADR-0127 D6/D7). Two questions can need
// a human on the initiating conversation surface before a contributed step may
// dispatch:
//
//   - "approve <tool> on <machine>?" — the D7 consent prompt for an effectful
//     step (owner ruling 2026-08-20: reads run silently but receipted; anything
//     effectful gets a one-tap approval naming the exact object; deny is the
//     safe default);
//   - "which machine?" — ladder rung 4 (D6), when a bare capability resolves to
//     no single machine.
//
// Since ADR-0142 D11 the seam is a view over the ONE addressed approval hub
// (AddressedHub.Consent): the kernel raises a prompt, the hub persists it,
// routes it (default rule: self — the beneficiary), delivers it to whatever
// surfaces can reach an eligible approver, blocks, and is resolved by an
// answer the hub authorises — failing CLOSED on no eligible subscriber, on
// timeout, and on cancellation. The controller is
// surface-agnostic; a premium bridge (Telegram via the ADR-0098 conversation
// lane, the operator console) subscribes through Watch and answers through
// Submit. OSS core ships the seam and the in-memory hub; with nothing
// subscribed every prompt-requiring step refuses, which is the sealed
// fail-closed default, not a degradation.
//
// The parking notification ("machine offline — queued until <deadline>") rides
// the SAME stream as a non-blocking notice rather than the ADR-0098
// ProgressSink, because progress phases are a CLOSED vocabulary (D7 there) and
// a notice naming a machine and a deadline cannot lawfully cross that seam; a
// subscriber that owns a conversation surface forwards it however it likes.

// ConsentPromptKind is the closed set of things the seam can put in front of a
// surface.
type ConsentPromptKind string

const (
	// ConsentPromptApprove asks "approve <tool> on <machine>?" and blocks for a
	// yes/no. Deny is the default on every non-answer.
	ConsentPromptApprove ConsentPromptKind = "approve"
	// ConsentPromptChooseMachine asks "which machine?" and blocks for a machine
	// name from Candidates. No answer refuses the step (never a guess).
	ConsentPromptChooseMachine ConsentPromptKind = "choose_machine"
	// ConsentPromptNotice is informational (e.g. the parking notification). It
	// never blocks and takes no answer.
	ConsentPromptNotice ConsentPromptKind = "notice"
)

// ConsentPrompt is one question (or notice) for the initiating surface. Args
// are surfaced RAW and verbatim (bounded) — the kernel does not parse their
// semantics; Object is the one convenience extraction (a top-level "path" or
// "url" string argument, verbatim) so a one-tap prompt can name the exact
// object without the reader parsing JSON.
type ConsentPrompt struct {
	ID   string
	Kind ConsentPromptKind
	// Machine is the target machine (approve / notice).
	Machine string
	// Candidates are the live capable machines (choose_machine).
	Candidates []string
	// Tool is the namespaced local:<machine>/<tool> name (approve) or the bare
	// capability (choose_machine).
	Tool string
	// Object names the exact object acted on, where derivable from the args —
	// the raw top-level "path" (or "url") string, verbatim. Empty otherwise.
	Object string
	// ArgsJSON is the step's raw arguments, verbatim, bounded for display.
	ArgsJSON string
	// TaskID correlates the prompt with the task (the ADR-0126 task id / session
	// id); AgentID names the attending agent; Beneficiary the owner principal
	// whose fleet is in play.
	TaskID      string
	AgentID     string
	Beneficiary PrincipalRef
	// ConversationID routes the prompt to the conversation that ordered the
	// work, when the kernel could attribute one (ADR-0098). Empty means the
	// subscriber decides where to render it.
	ConversationID string
	// Notice is the informational text of a ConsentPromptNotice.
	Notice string
}

// ConsentAnswer is a surface's ruling on one prompt.
type ConsentAnswer struct {
	// Approved answers an approve prompt; a choose_machine answer sets it true
	// alongside Machine.
	Approved bool
	// Machine is the chosen machine (choose_machine answers).
	Machine string
	// AnsweredBy identifies who answered, for the decision record (set by the hub
	// from the verified answerer).
	AnsweredBy string
	// By is the verified answerer a surface submits as. The hub refuses an
	// answer whose By satisfies none of the prompt's approver rules (default:
	// self — the beneficiary), ADR-0142 D11.
	By Approver
}

// ConsentOutcome distinguishes WHY a prompt resolved the way it did, so each
// outcome lands on the decision seam under its own reason (the CL-2 receipts
// requirement: auto, approved, denied, timeout, no-subscriber are all distinct).
type ConsentOutcome string

const (
	// ConsentAnswered: a subscriber submitted an answer (inspect ConsentAnswer).
	ConsentAnswered ConsentOutcome = "answered"
	// ConsentTimedOut: nobody answered within the prompt window — fail-closed.
	ConsentTimedOut ConsentOutcome = "timeout"
	// ConsentNoSubscriber: no surface is listening at all — fail-closed.
	ConsentNoSubscriber ConsentOutcome = "no_subscriber"
)

// ConsentController is what the ToolExecutor consults pre-dispatch. nil ⇒ every
// prompt-requiring step refuses (fail-closed); Notify is best-effort.
type ConsentController interface {
	// Request blocks for an answer, failing closed (unapproved) on timeout, on
	// no-subscriber, and on ctx cancellation (err non-nil only for the latter).
	Request(ctx context.Context, p ConsentPrompt) (ConsentAnswer, ConsentOutcome, error)
	// Notify broadcasts an informational notice. Never blocks, never errors.
	Notify(ctx context.Context, p ConsentPrompt)
}

// WorkerConsentHub is the full surface a conversation bridge drives: the
// controller plus Watch (subscribe) and Submit (answer as ans.By). Since
// ADR-0142 D11 it is a VIEW over the one AddressedHub (AddressedHub.Consent),
// not a second implementation: prompts are durable hub items, and an answer is
// authorised by the hub's answer-time check.
type WorkerConsentHub interface {
	ConsentController
	Watch() (<-chan ConsentPrompt, func())
	// ctx is the answerer's request (ENG-1467 item 3): the hub's answer-time
	// check and policy routing run on it.
	Submit(ctx context.Context, id string, ans ConsentAnswer) bool
}

// defaultConsentGrace bounds a prompt awaiting a human answer when no window is
// configured (mcp.worker_consent_timeout_ms). Two minutes: long enough for a
// one-tap answer on a phone, short enough that an unattended plan fails rather
// than hangs.
const defaultConsentGrace = 120 * time.Second

// DefaultConsentWindow is defaultConsentGrace, for surfaces that render it.
const DefaultConsentWindow = defaultConsentGrace

// ContributedStepEffectful classifies a contributed step's effect class for the
// D7 consent gate. Read and egress are the lane's silent baseline — egress is
// stamped on EVERY contributed tool at attachment (owner ruling 2026-08-20), so
// judging by it would make every read prompt; write, spend and admin make the
// step effectful, and an effectful step never dispatches without an affirmative
// consent path.
func ContributedStepEffectful(effects []ToolEffect) bool {
	for _, e := range effects {
		switch e {
		case EffectWrite, EffectSpend, EffectAdmin:
			return true
		}
	}
	return false
}

// conversationCtxKey carries the conversation that ordered the current work
// (ADR-0098), seeded KERNEL-side from the caller's lease binding — never from a
// request payload (INV-5). The consent seam reads it to route prompts to the
// initiating conversation surface; empty means "unattributed".
type conversationCtxKey struct{}

// WithConversationID returns a child context carrying the ordering conversation.
func WithConversationID(ctx context.Context, conversationID string) context.Context {
	return context.WithValue(ctx, conversationCtxKey{}, conversationID)
}

// ConversationIDFromContext returns the ordering conversation, or "".
func ConversationIDFromContext(ctx context.Context) string {
	s, _ := ctx.Value(conversationCtxKey{}).(string)
	return s
}
