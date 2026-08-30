package ingress

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// The ADR-0127 CL-2 conversation consent bridge: the OSS production subscriber
// on the worker-consent hub. The CL-2 record shipped the seam with nothing
// subscribed — every prompt-requiring contributed step refused, the sealed
// fail-closed default. This bridge closes the loop for any deployment with a
// chat lane: an approve / choose_machine prompt carrying an ordering
// conversation is rendered as PLAIN TEXT into that conversation through the
// chat lane's own speak primitive (store first, then ingress delivery — the
// ADR-0090 D8 outbound seam), and the beneficiary's matching reply is consumed
// as the answer by the inbound hook (InboundService.SetConsentGate) before the
// turn machinery ever sees it.
//
// Plain text is the point: it works on every surface a conversation can live on
// (Telegram, chat console) with no premium anything. Button rendering is
// premium polish layered on the same hub later.
//
// Fail-closed properties, stated because each is load-bearing:
//   - a prompt with NO ordering conversation is left alone — never guessed into
//     someone's transcript; the controller times it out refused;
//   - only the prompt's BENEFICIARY — the sender whose resolved principal
//     binding equals the beneficiary owner principal — can answer; everyone
//     else's messages (unbound strangers included) flow to chat untouched;
//   - a non-matching reply is NOT consumed and approves nothing; the prompt
//     stays pending with the controller's timeout as the backstop;
//   - one prompt renders per conversation at a time; later ones queue and post
//     when the first resolves, so two questions never interleave their answers.

// ConversationPoster posts one agent-authored message into an existing
// conversation and lets it find its own way out — durable in the store first,
// then delivered through whichever ingress the conversation arrived on.
// Satisfied by *chat.TurnService (Emit). Deliberately NOT a new outbound path:
// the bridge speaks exactly the way a chat turn's reply does.
type ConversationPoster interface {
	Emit(ctx context.Context, conversationID, text string) (domain.Message, error)
}

// ConsentAnswerGate intercepts an admitted inbound chat message that answers a
// pending consent prompt. Implemented by *ConsentBridge; consumed by
// InboundService. The return reports whether the message WAS the answer — true
// means the turn must not run.
type ConsentAnswerGate interface {
	HandleReply(ctx context.Context, conversationID, text string, sender domain.PrincipalRef) bool
}

// bridgeDefaultWindow mirrors the controller's unexported 120 s default
// (domain.InMemoryConsentController), so an unconfigured window renders the
// same expiry the controller enforces.
const bridgeDefaultWindow = 120 * time.Second

// consentPending is one prompt the bridge is holding for a conversation.
type consentPending struct {
	prompt domain.ConsentPrompt
	// expiresAt is the bridge's local mirror of the controller's window,
	// measured from when the prompt ARRIVED (≈ when the controller started its
	// timer). The controller owns the authoritative timeout; this exists so a
	// dead prompt does not block the conversation's queue forever and so the
	// rendered "Expires in …" is honest for queued prompts posted late.
	expiresAt time.Time
	timer     *time.Timer // armed while active; nil while queued
}

// ConsentBridge watches the worker-consent hub and speaks into conversations.
type ConsentBridge struct {
	hub    domain.WorkerConsentHub
	poster ConversationPoster
	window time.Duration
	logger *slog.Logger
	now    func() time.Time

	mu     sync.Mutex
	active map[string]*consentPending   // conversation → the prompt awaiting an answer
	queue  map[string][]*consentPending // conversation → prompts waiting behind it
	// postCtx carries posts made from timer callbacks after Start; detached
	// from cancellation so a confirmation racing shutdown still lands.
	postCtx     context.Context
	cancelWatch func()

	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
}

var _ ConsentAnswerGate = (*ConsentBridge)(nil)

// NewConsentBridge wires the bridge. hub and poster are required; window ≤ 0
// falls back to the controller's own 120 s default so both sides of the
// timeout agree.
func NewConsentBridge(hub domain.WorkerConsentHub, poster ConversationPoster, window time.Duration) *ConsentBridge {
	if window <= 0 {
		window = bridgeDefaultWindow
	}
	return &ConsentBridge{
		hub:     hub,
		poster:  poster,
		window:  window,
		logger:  slog.Default(),
		now:     time.Now,
		active:  map[string]*consentPending{},
		queue:   map[string][]*consentPending{},
		postCtx: context.Background(),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// SetLogger overrides the default logger.
func (b *ConsentBridge) SetLogger(l *slog.Logger) {
	if l != nil {
		b.logger = l
	}
}

// Start subscribes to the hub and runs the render loop until ctx ends or Stop.
func (b *ConsentBridge) Start(ctx context.Context) {
	ch, cancel := b.hub.Watch()
	b.mu.Lock()
	b.cancelWatch = cancel
	b.postCtx = context.WithoutCancel(ctx)
	b.mu.Unlock()
	go b.run(ctx, ch)
}

// Stop unsubscribes and ends the render loop. Idempotent; pending local state
// simply ages out (the controller owns the authoritative refusals).
func (b *ConsentBridge) Stop() {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		cancel := b.cancelWatch
		b.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		close(b.stopCh)
	})
}

func (b *ConsentBridge) run(ctx context.Context, ch <-chan domain.ConsentPrompt) {
	defer close(b.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		case p := <-ch:
			b.handlePrompt(ctx, p)
		}
	}
}

// handlePrompt routes one hub emission: notices post immediately, questions
// activate or queue per conversation, and anything unattributed is left to the
// controller's fail-closed timeout.
func (b *ConsentBridge) handlePrompt(ctx context.Context, p domain.ConsentPrompt) {
	switch p.Kind {
	case domain.ConsentPromptApprove, domain.ConsentPromptChooseMachine:
		// fall through to the pending machinery below
	default:
		// A notice (parking etc.) never blocks and takes no answer — post it
		// verbatim and keep no state. Without a conversation there is nowhere
		// honest to say it.
		if p.ConversationID == "" || strings.TrimSpace(p.Notice) == "" {
			return
		}
		b.post(ctx, p.ConversationID, p.Notice)
		return
	}
	if p.ConversationID == "" {
		// Never guess a conversation: an unattributed prompt is not this
		// bridge's to render, and the controller times it out refused — the
		// same fail-closed outcome the seam shipped with.
		b.logger.Info("ADR-0127 CL-2: consent prompt has no ordering conversation; leaving it to the fail-closed timeout",
			"prompt", p.ID, "kind", string(p.Kind), "tool", p.Tool)
		return
	}
	e := &consentPending{prompt: p, expiresAt: b.now().Add(b.window)}
	b.mu.Lock()
	if b.active[p.ConversationID] == nil {
		b.active[p.ConversationID] = e
		b.armLocked(p.ConversationID, e)
		b.mu.Unlock()
		b.post(ctx, p.ConversationID, renderConsentPrompt(p, b.window))
		return
	}
	// One question at a time per conversation: this one posts when the
	// current one resolves (answered or expired).
	b.queue[p.ConversationID] = append(b.queue[p.ConversationID], e)
	b.mu.Unlock()
}

// HandleReply is the inbound hook (ConsentAnswerGate): decide whether this
// admitted message answers the conversation's pending prompt.
//
// The security invariant lives HERE and nowhere softer: the reply counts only
// when the sender's resolved bound principal EQUALS the prompt's beneficiary
// owner principal. A zero sender (unbound, group-bound, no identity resolver)
// never equals anything, so strangers cannot answer; a zero beneficiary is
// refused explicitly rather than letting two zeros match. Every refusal here
// returns false WITHOUT consuming the message — it flows to chat exactly as if
// no prompt were pending, and the prompt keeps waiting on the controller's
// timeout.
func (b *ConsentBridge) HandleReply(ctx context.Context, conversationID, text string, sender domain.PrincipalRef) bool {
	if b == nil || conversationID == "" {
		return false
	}
	b.mu.Lock()
	ap := b.active[conversationID]
	if ap == nil {
		b.mu.Unlock()
		return false
	}
	p := ap.prompt
	b.mu.Unlock()

	if sender == (domain.PrincipalRef{}) || p.Beneficiary == (domain.PrincipalRef{}) || sender != p.Beneficiary {
		return false // not the beneficiary: an ordinary chat message
	}
	ans, ok := matchConsentReply(p, text)
	if !ok {
		return false // not an answer: let it flow to chat; the prompt stays pending
	}
	ans.AnsweredBy = sender.String()

	// Submit BEFORE clearing local state: the controller is the authority on
	// whether the prompt is still alive.
	submitted := b.hub.Submit(p.ID, ans)

	b.mu.Lock()
	var next *consentPending
	if cur := b.active[conversationID]; cur != nil && cur.prompt.ID == p.ID {
		if cur.timer != nil {
			cur.timer.Stop()
		}
		delete(b.active, conversationID)
		next = b.promoteLocked(conversationID)
	}
	b.mu.Unlock()

	if submitted {
		b.post(ctx, conversationID, renderConsentConfirmation(p, ans))
	} else {
		// The controller already timed it out (or resolved it another way).
		// The message was unmistakably an answer, so consume it — but nothing
		// was approved, and the person is told why their tap did nothing.
		b.post(ctx, conversationID, "That request has already expired — nothing was approved.")
	}
	if next != nil {
		b.post(ctx, conversationID, renderConsentPrompt(next.prompt, time.Until(next.expiresAt)))
	}
	return true
}

// expire is the local backstop mirroring the controller's timeout: clear the
// conversation's slot so queued prompts are not blocked behind a dead one.
func (b *ConsentBridge) expire(conversationID, promptID string) {
	b.mu.Lock()
	cur := b.active[conversationID]
	if cur == nil || cur.prompt.ID != promptID {
		b.mu.Unlock()
		return
	}
	delete(b.active, conversationID)
	next := b.promoteLocked(conversationID)
	ctx := b.postCtx
	b.mu.Unlock()
	if next != nil {
		b.post(ctx, conversationID, renderConsentPrompt(next.prompt, time.Until(next.expiresAt)))
	}
}

// armLocked starts the active prompt's local expiry timer. Caller holds mu.
func (b *ConsentBridge) armLocked(conversationID string, e *consentPending) {
	d := time.Until(e.expiresAt)
	if d < 0 {
		d = 0
	}
	id := e.prompt.ID
	e.timer = time.AfterFunc(d, func() { b.expire(conversationID, id) })
}

// promoteLocked activates the next viable queued prompt, skipping any the
// controller has already timed out. Caller holds mu; the returned prompt (nil
// when none) is the caller's to post.
func (b *ConsentBridge) promoteLocked(conversationID string) *consentPending {
	q := b.queue[conversationID]
	for len(q) > 0 {
		e := q[0]
		q = q[1:]
		if !b.now().Before(e.expiresAt) {
			continue // already dead controller-side; posting it would be a lie
		}
		b.queue[conversationID] = q
		b.active[conversationID] = e
		b.armLocked(conversationID, e)
		return e
	}
	delete(b.queue, conversationID)
	return nil
}

// post speaks into the conversation, best-effort. Emit stores the message
// durably before attempting delivery, so a delivery error here has already
// preserved what was said; nothing on this path may fail a step or a turn.
func (b *ConsentBridge) post(ctx context.Context, conversationID, text string) {
	if b.poster == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := b.poster.Emit(ctx, conversationID, text); err != nil {
		b.logger.Warn("ADR-0127 CL-2: consent bridge could not post into the conversation",
			"conversation", conversationID, "err", err)
	}
}

// matchConsentReply decides whether text answers the prompt. The vocabulary is
// deliberately tiny and exact — after trimming, case-insensitive
// 'approve'/'yes' and 'deny'/'no' for an approve prompt, and an exact
// candidate name for choose_machine. Anything else is NOT an answer: a
// sentence containing the word "approve" must not approve an effect on
// someone's machine.
func matchConsentReply(p domain.ConsentPrompt, text string) (domain.ConsentAnswer, bool) {
	t := strings.TrimSpace(text)
	switch p.Kind {
	case domain.ConsentPromptApprove:
		switch strings.ToLower(t) {
		case "approve", "yes":
			return domain.ConsentAnswer{Approved: true}, true
		case "deny", "no":
			return domain.ConsentAnswer{Approved: false}, true
		}
	case domain.ConsentPromptChooseMachine:
		for _, c := range p.Candidates {
			if strings.EqualFold(t, c) {
				// The canonical candidate name is submitted, not the user's
				// casing — the ladder validates the answer against the live
				// candidate list by exact name.
				return domain.ConsentAnswer{Approved: true, Machine: c}, true
			}
		}
	}
	return domain.ConsentAnswer{}, false
}

// renderConsentPrompt is the plain-text rendering that works on every surface.
func renderConsentPrompt(p domain.ConsentPrompt, expiresIn time.Duration) string {
	if expiresIn < 0 {
		expiresIn = 0
	}
	exp := " Expires in " + expiresIn.Round(time.Second).String() + "."
	if p.Kind == domain.ConsentPromptChooseMachine {
		return "Which machine should run " + p.Tool + "? Reply with one of: " +
			strings.Join(p.Candidates, ", ") + "." + exp
	}
	obj := ""
	if p.Object != "" {
		obj = " — object: " + p.Object
	}
	return "Cambrian wants to " + p.Tool + " on " + p.Machine + obj +
		". Reply 'approve' or 'deny'." + exp
}

// renderConsentConfirmation acknowledges a consumed answer, so the person's
// reply visibly did something.
func renderConsentConfirmation(p domain.ConsentPrompt, ans domain.ConsentAnswer) string {
	if !ans.Approved {
		return "Denied."
	}
	machine := ans.Machine
	if machine == "" {
		machine = p.Machine
	}
	return "Approved — running on " + machine + "."
}
