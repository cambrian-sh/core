package domain

import (
	"context"
	"fmt"
	"sync"
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
// The seam mirrors InMemoryApprovalController (ADR-0039 D10) deliberately: the
// kernel raises a prompt, streams it to whatever surfaces subscribed, blocks on
// a per-prompt channel, and is resolved by a separate Submit — failing CLOSED
// on no-subscriber, on timeout, and on cancellation. The controller is
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
	// AnsweredBy identifies who answered, for the decision record.
	AnsweredBy string
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

// WorkerConsentHub is the full surface a conversation bridge drives:
// the controller plus Watch (subscribe) and Submit (answer). Mirrors
// domain.ApprovalHub.
type WorkerConsentHub interface {
	ConsentController
	Watch() (<-chan ConsentPrompt, func())
	Submit(id string, ans ConsentAnswer) bool
}

// defaultConsentGrace bounds a prompt awaiting a human answer when no window is
// configured (mcp.worker_consent_timeout_ms). Two minutes: long enough for a
// one-tap answer on a phone, short enough that an unattended plan fails rather
// than hangs.
const defaultConsentGrace = 120 * time.Second

// InMemoryConsentController is the default WorkerConsentHub. Same shape and
// same fail-closed posture as InMemoryApprovalController: no subscriber and
// timeout both deny.
type InMemoryConsentController struct {
	grace   time.Duration
	mu      sync.Mutex
	counter int
	pending map[string]chan ConsentAnswer
	subs    map[int]chan ConsentPrompt
	nextSub int
}

// NewInMemoryConsentController constructs the hub. grace ≤ 0 falls back to the
// package default (120s).
func NewInMemoryConsentController(grace time.Duration) *InMemoryConsentController {
	if grace <= 0 {
		grace = defaultConsentGrace
	}
	return &InMemoryConsentController{
		grace:   grace,
		pending: map[string]chan ConsentAnswer{},
		subs:    map[int]chan ConsentPrompt{},
	}
}

var _ WorkerConsentHub = (*InMemoryConsentController)(nil)

// Request blocks for a surface's answer; fails closed on no-subscriber and on
// timeout. ctx cancellation returns ctx's error with an unapproved answer.
func (c *InMemoryConsentController) Request(ctx context.Context, p ConsentPrompt) (ConsentAnswer, ConsentOutcome, error) {
	c.mu.Lock()
	if len(c.subs) == 0 {
		c.mu.Unlock()
		return ConsentAnswer{}, ConsentNoSubscriber, nil // fail-closed: nobody to ask
	}
	c.counter++
	p.ID = fmt.Sprintf("consent-%d", c.counter)
	ansCh := make(chan ConsentAnswer, 1)
	c.pending[p.ID] = ansCh
	subs := make([]chan ConsentPrompt, 0, len(c.subs))
	for _, s := range c.subs {
		subs = append(subs, s)
	}
	c.mu.Unlock()

	for _, s := range subs {
		select {
		case s <- p:
		default: // a saturated subscriber is skipped, never waited on
		}
	}

	defer func() {
		c.mu.Lock()
		delete(c.pending, p.ID)
		c.mu.Unlock()
	}()

	timer := time.NewTimer(c.grace)
	defer timer.Stop()
	select {
	case ans := <-ansCh:
		return ans, ConsentAnswered, nil
	case <-timer.C:
		return ConsentAnswer{}, ConsentTimedOut, nil // fail-closed: unanswered
	case <-ctx.Done():
		return ConsentAnswer{}, ConsentTimedOut, ctx.Err()
	}
}

// Notify broadcasts a notice to every subscriber, non-blocking.
func (c *InMemoryConsentController) Notify(_ context.Context, p ConsentPrompt) {
	c.mu.Lock()
	c.counter++
	p.ID = fmt.Sprintf("consent-%d", c.counter)
	if p.Kind == "" {
		p.Kind = ConsentPromptNotice
	}
	subs := make([]chan ConsentPrompt, 0, len(c.subs))
	for _, s := range c.subs {
		subs = append(subs, s)
	}
	c.mu.Unlock()
	for _, s := range subs {
		select {
		case s <- p:
		default:
		}
	}
}

// Watch subscribes a surface to prompts and notices. The returned cancel
// removes the subscription.
func (c *InMemoryConsentController) Watch() (<-chan ConsentPrompt, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextSub
	c.nextSub++
	ch := make(chan ConsentPrompt, 16)
	c.subs[id] = ch
	return ch, func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

// Submit resolves a pending prompt. false when the id is unknown (already
// resolved, timed out, or never existed).
func (c *InMemoryConsentController) Submit(id string, ans ConsentAnswer) bool {
	c.mu.Lock()
	ansCh, ok := c.pending[id]
	c.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ansCh <- ans:
		return true
	default:
		return false
	}
}

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
