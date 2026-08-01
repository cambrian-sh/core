// Package chat implements the OSS conversational turn path (ADR-0084 D4).
//
// The kernel owns the whole loop — load the conversation, append the user's message, thread
// the recent history, dispatch one turn to a pooled stateless worker, append the reply —
// because both the OSS chat lane and a premium manager must produce identical, durable
// conversation state. A premium manager adds auth, ownership, tenanting and cost control in
// FRONT of this; it does not reimplement it.
//
// The planner is not involved. A conversational turn is owned by exactly one agent loop and
// reaches the planner only when the session agent explicitly yields a subgoal (ADR-0080) —
// the property that took the airline benchmark from hollow passes to competent solves.
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Dispatcher runs one turn on some worker. Satisfied by *agentpool.Pool.
type Dispatcher interface {
	Dispatch(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error)
}

// TokenAcquirer provisions a managed-LLM budget lease (ADR-0018) for a turn dispatched
// OUTSIDE the planner path, where leases are normally issued. Without it the session agent's
// first generate call is rejected UNAUTHENTICATED. May be nil (no gateway configured).
type TokenAcquirer func(ctx context.Context, tokenLimit int, ttl time.Duration) (tokenID string, release func(), err error)

// Defaults chosen to mirror the premium manager's proven values.
const (
	// DefaultTurnTimeout bounds one dispatched turn (tool calls + LLM).
	DefaultTurnTimeout = 240 * time.Second
	// DefaultStallTimeout is how long a turn may report NOTHING before it is treated as
	// wedged.
	//
	// Sized against what it is measuring: the emission points are memory retrieval and
	// tool execution, and a healthy turn hits one every few seconds. Ninety seconds of
	// complete silence is not slow work, it is stopped work — while still leaving ample
	// room for a single long retrieval or a slow tool.
	DefaultStallTimeout = 90 * time.Second
	// DefaultHistoryLimit is how many prior messages are threaded into a turn. Bounded
	// because prompt size — and therefore cost and latency — otherwise grows without limit
	// as a conversation lengthens.
	DefaultHistoryLimit = 40
	// tokenLimit / tokenTTL bound the per-turn managed-LLM lease.
	tokenLimit = 8192
)

// ErrEmptyReply reports that the worker returned nothing to say. Surfaced rather than stored,
// because persisting an empty agent message would corrupt the transcript for every later turn.
var ErrEmptyReply = errors.New("chat: empty reply from session worker")

// TurnRequest is one inbound user turn.
type TurnRequest struct {
	ConversationID string
	Text           string
	// ClientID is an optional idempotency key. A retried turn returns the original reply
	// instead of running the turn a second time.
	ClientID string
	// Policy is the system/policy prompt threaded to the session agent.
	Policy string
}

// TurnService executes conversational turns against a pool of stateless workers.
// Deliverer sends an outbound message to whoever a conversation belongs to
// (ADR-0090 D8). Satisfied by internal/ingress.DeliveryService.
//
// Taken as an interface so the chat tier depends on the ABILITY to deliver rather
// than on the delivery implementation — and so a conversation with no ingress
// behind it needs no delivery machinery at all.
type Deliverer interface {
	Deliver(ctx context.Context, conversationID, text, txnID string) error
}

type TurnService struct {
	store        domain.ConversationStore
	pool         Dispatcher
	acquireToken TokenAcquirer
	// deliverer sends a message outward when the conversation came through an
	// ingress (ADR-0090 D8). nil ⇒ store-only.
	deliverer Deliverer
	// leases binds the turn's conversation onto its BudgetLease so that work the turn
	// delegates to the planner can be attributed back to the conversation server-side
	// (ADR-0084 D2). Optional; without it a delegated run simply carries no conversation.
	leases domain.LeaseResolver
	// progress receives supersedable "what is happening now" snapshots for the turn
	// (ADR-0098). nil ⇒ nothing is listening, which is the OSS default: the kernel
	// emits unconditionally and a premium bridge decides what becomes of it.
	progress domain.ProgressSink
	// liveness reports when a conversation last showed signs of work. Supplied by the
	// composition root; nil disables stall detection.
	liveness func(conversationID string) time.Time

	TurnTimeout  time.Duration
	HistoryLimit int
	// StallTimeout fails a turn that has stopped making progress, well before
	// TurnTimeout would.
	//
	// The two bound different things. TurnTimeout caps how long a turn may legitimately
	// take; StallTimeout catches one that is no longer doing anything at all. Without it
	// a wedged turn holds its worker, its lease and the user's attention for the full
	// TurnTimeout while reporting nothing — truthfully, and uselessly.
	StallTimeout time.Duration
}

// SetLivenessProbe wires stall detection. Optional: without it a turn is bounded only by
// TurnTimeout.
func (s *TurnService) SetLivenessProbe(f func(conversationID string) time.Time) { s.liveness = f }

// watchStall cancels the turn when it stops reporting progress.
//
// It deliberately measures SILENCE rather than elapsed time: a turn legitimately running
// for three minutes while retrieving and calling tools keeps reporting, and must not be
// killed. One that has emitted nothing for the stall window has stopped, whatever the
// clock says.
func (s *TurnService) watchStall(ctx context.Context, conversationID string, cancel context.CancelFunc, stalled *atomic.Bool) {
	if s.liveness == nil || s.StallTimeout <= 0 {
		return
	}
	// Poll at a fraction of the window so detection is prompt without being chatty.
	tick := s.StallTimeout / 4
	if tick < time.Second {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			last := s.liveness(conversationID)
			if last.IsZero() {
				continue // nothing has reported yet; the turn has only just begun
			}
			if time.Since(last) > s.StallTimeout {
				stalled.Store(true)
				slog.Warn("chat: turn stopped making progress; cancelling",
					"conversation", conversationID, "silent_for", time.Since(last).Round(time.Second))
				cancel()
				return
			}
		}
	}
}

// SetProgressSink wires the ADR-0098 progress channel. Optional: without it the emission
// sites are inert.
func (s *TurnService) SetProgressSink(sink domain.ProgressSink) { s.progress = sink }

// emitProgress is the local shorthand for one supersedable snapshot on this turn.
//
// Best-effort by construction (ADR-0098 D5) — it cannot return an error, so a progress
// problem can never fail the turn it describes.
func (s *TurnService) emitProgress(ctx context.Context, conversationID string, phase domain.ProgressPhase) {
	domain.EmitProgress(ctx, s.progress, domain.ProgressUpdate{
		ConversationID: conversationID,
		Phase:          phase,
	})
}

// NewTurnService wires the service. store and pool are required.
// SetLeaseBinder wires the lease registry so each turn's lease carries its conversation.
func (s *TurnService) SetLeaseBinder(r domain.LeaseResolver) { s.leases = r }

// SetDeliverer wires the outbound path (ADR-0090 D8). nil leaves conversations
// store-only, which is correct for a console-only deployment: the console reads
// messages back, it is not pushed to.
func (s *TurnService) SetDeliverer(d Deliverer) { s.deliverer = d }

// RunTurn is the inbound-ingress entry point: run a turn and let the reply find
// its own way out.
//
// It exists because an ingress caller has nobody to return a message to — the
// reply reaches the sender through delivery, not through this call's return
// value. Discarding the message here is therefore correct rather than lossy: it
// is stored, and it has already been delivered by Emit.
func (s *TurnService) RunTurn(ctx context.Context, conversationID, text, clientID string) error {
	_, err := s.Turn(ctx, TurnRequest{
		ConversationID: conversationID,
		Text:           text,
		ClientID:       clientID,
	})
	return err
}

// Emit is the speak primitive: append a message to a conversation and, when that
// conversation arrived through an ingress, send it out.
//
// This is what makes the duplex model possible. `Turn` returns exactly one
// message because a synchronous caller is waiting for exactly one; Emit has no
// such constraint, so an agent may say "checking..." and then answer, and a watch
// may speak into a conversation nobody is currently talking in. One inbound
// message no longer implies one outbound message — or any.
//
// The stored message is the source of truth and is durable BEFORE delivery is
// attempted, so a delivery failure never loses what was said. The returned error
// is therefore a DELIVERY error: the message is valid whether or not it is nil.
func (s *TurnService) Emit(ctx context.Context, conversationID, text string) (domain.Message, error) {
	if strings.TrimSpace(text) == "" {
		return domain.Message{}, errors.New("chat: emitted text is required")
	}
	msg, err := s.store.AppendMessage(ctx, domain.Message{
		ID:             newID(),
		ConversationID: conversationID,
		Role:           domain.MessageRoleAgent,
		Content:        text,
	})
	if err != nil {
		return domain.Message{}, err
	}
	if s.deliverer == nil {
		return msg, nil
	}
	// The message id is the idempotency key: delivering message M is the same act
	// however many times it is retried (ADR-0090 D8).
	if derr := s.deliverer.Deliver(ctx, conversationID, text, msg.ID); derr != nil {
		if errors.Is(derr, domain.ErrNoDeliveryAddress) {
			// Nothing arrived through an ingress for this conversation, so there is
			// nowhere to push. Not a failure — it is every console conversation.
			return msg, nil
		}
		return msg, derr
	}
	return msg, nil
}

func NewTurnService(store domain.ConversationStore, pool Dispatcher, acquire TokenAcquirer) *TurnService {
	return &TurnService{
		store:        store,
		pool:         pool,
		acquireToken: acquire,
		TurnTimeout:  DefaultTurnTimeout,
		HistoryLimit: DefaultHistoryLimit,
	}
}

// Turn appends the user's message, dispatches it, and appends the reply — returning the
// stored agent message.
//
// Retry safety: the user message is appended FIRST, with the caller's ClientID. If that
// append turns out to be a replay (the store returns the message already stored) and a reply
// already follows it, the original reply is returned and no second turn is dispatched. This
// is what stops a client retry from double-charging an LLM call or re-running side-effecting
// tools.
func (s *TurnService) Turn(ctx context.Context, req TurnRequest) (_ domain.Message, turnErr error) {
	if strings.TrimSpace(req.Text) == "" {
		return domain.Message{}, errors.New("chat: turn text is required")
	}
	conv, err := s.store.GetConversation(ctx, req.ConversationID)
	if err != nil {
		return domain.Message{}, err // ErrConversationNotFound passes through
	}
	if conv.Status != domain.ConversationOpen {
		return domain.Message{}, domain.ErrConversationClosed
	}

	userMsg, err := s.store.AppendMessage(ctx, domain.Message{
		ID:             newID(),
		ConversationID: req.ConversationID,
		Role:           domain.MessageRoleUser,
		Content:        req.Text,
		ClientID:       req.ClientID,
	})
	if err != nil {
		return domain.Message{}, err
	}

	// Replay detection: anything already stored after this user message means the turn was
	// processed on an earlier attempt.
	if req.ClientID != "" {
		if later, lerr := s.store.ListMessages(ctx, req.ConversationID, userMsg.Seq, 1); lerr == nil && len(later) > 0 {
			return later[0], nil
		}
	}

	// The user is now waiting. Say something immediately: the gap between "sent" and the
	// first sign of life is what makes a working system indistinguishable from a hung one.
	s.emitProgress(ctx, req.ConversationID, domain.PhaseUnderstanding)

	// ADR-0098 D3, on EVERY exit path. The happy path clears itself — the reply supersedes
	// the status line — but a turn that fails before replying delivers nothing, and an
	// uncleared line strands the user on "working on it" forever, which reads as a hang.
	// Deferring is what makes this true for the error returns as well as the success one.
	// A failure rides out on the SAME line rather than becoming a message. The user needs
	// to know what happened — silence is indistinguishable from a hang — but a failure
	// notice does not belong in the transcript: persisting it would feed "something went
	// wrong" back into the model's context next turn, and would break the invariant that a
	// failed turn stores nothing. So the status line ends as the explanation.
	defer func() {
		domain.EmitProgress(context.WithoutCancel(ctx), s.progress, domain.ProgressUpdate{
			ConversationID: req.ConversationID,
			Final:          true,
			Note:           humanFailure(turnErr), // empty on success ⇒ the line is cleared
		})
	}()

	history, err := s.loadHistory(ctx, req.ConversationID, userMsg.Seq)
	if err != nil {
		return domain.Message{}, err
	}

	turnCtx := ctx
	turnCancel := context.CancelFunc(func() {})
	if s.TurnTimeout > 0 {
		turnCtx, turnCancel = context.WithTimeout(ctx, s.TurnTimeout)
	} else {
		turnCtx, turnCancel = context.WithCancel(ctx)
	}
	defer turnCancel()

	// A turn that goes quiet is cut loose long before TurnTimeout would notice.
	var stalled atomic.Bool
	go s.watchStall(turnCtx, req.ConversationID, turnCancel, &stalled)

	// Acquire the managed-LLM lease HERE, not inside buildHandoff: the release must live
	// until the turn has been dispatched and answered. Releasing it when buildHandoff returns
	// (an earlier bug) completed the token before the agent's first generate call, so the
	// agent saw "session not found" and every reply fell back.
	sessionToken := ""
	if s.acquireToken != nil {
		ttl := s.TurnTimeout
		if ttl <= 0 {
			ttl = DefaultTurnTimeout
		}
		if tok, release, terr := s.acquireToken(turnCtx, tokenLimit, ttl); terr == nil && tok != "" {
			sessionToken = tok
			defer release()
			// Stamp the conversation and the ordering turn onto the lease. If the worker
			// delegates to the planner, Execute resolves this lease and links the session
			// it opens back to this exchange — without the agent naming it.
			if s.leases != nil {
				s.leases.BindLease(domain.LeaseID(tok), domain.LeaseBinding{
					ConversationID:  conv.ID,
					OriginMessageID: userMsg.ID,
				})
			}
		}
	}

	// Everything after this point is the agent's own loop, which is where the minutes go.
	s.emitProgress(ctx, req.ConversationID, domain.PhaseWorking)

	h := s.buildHandoff(conv, req, history, sessionToken)
	resp, err := s.pool.Dispatch(turnCtx, h)
	if err != nil {
		// Distinguish "we gave up on a wedged turn" from an ordinary cancellation, so the
		// user is told what actually happened rather than a generic failure.
		if stalled.Load() {
			return domain.Message{}, ErrTurnStalled
		}
		return domain.Message{}, err // ErrPoolBusy / ErrWorkerLost pass through
	}

	reply := ""
	if resp != nil && resp.Payload != nil {
		reply = strings.TrimSpace(string(resp.Payload.Data))
	}
	// An agent that falls back can hand its ReAct envelope through verbatim. Nobody
	// should ever be shown raw JSON, so unwrap it here — the last point before the text
	// becomes a message.
	reply = unwrapFinalAnswer(reply)
	// A control envelope that survived the agent loop is not an answer. Treating it as an
	// empty reply routes it through the normal failure path — the user gets a sentence
	// they can act on instead of machine JSON, and the turn is recorded as failed, which
	// it was.
	if looksLikeControlEnvelope(reply) {
		slog.Warn("chat: agent returned a control envelope instead of an answer; suppressing",
			"conversation", req.ConversationID)
		reply = ""
	}
	if reply == "" {
		return domain.Message{}, ErrEmptyReply
	}

	// Through Emit, so a reply to an ingress-borne conversation reaches the person
	// who asked. A DELIVERY failure does not fail the turn: the reply is already
	// stored, the synchronous caller still gets it, and the undeliverable message is
	// dead-lettered by the delivery path rather than swallowed here.
	// ADR-0098 D3: the answer supersedes the progress. A user must never be left looking at
	// "working on it" after the reply has arrived.
	s.emitProgress(ctx, req.ConversationID, domain.PhaseWriting)

	msg, derr := s.Emit(ctx, req.ConversationID, reply)
	if derr != nil {
		slog.Warn("ADR-0090: turn reply stored but not delivered",
			"conversation", req.ConversationID, "message", msg.ID, "err", derr)
	}
	if msg.ID == "" {
		return domain.Message{}, derr
	}
	return msg, nil
}

// loadHistory returns the messages preceding upToSeq, bounded to HistoryLimit. Because Seq
// is contiguous, the window is a simple arithmetic offset — no extra count query.
func (s *TurnService) loadHistory(ctx context.Context, conversationID string, upToSeq int64) ([]domain.Message, error) {
	limit := s.HistoryLimit
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	after := upToSeq - int64(limit) - 1
	if after < 0 {
		after = 0
	}
	msgs, err := s.store.ListMessages(ctx, conversationID, after, 0)
	if err != nil {
		return nil, err
	}
	// Drop the message being answered; it travels as the payload, not as history.
	out := make([]domain.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Seq < upToSeq {
			out = append(out, m)
		}
	}
	return out, nil
}

// buildHandoff produces the per-turn contract the session agent already expects (ADR-0080):
// the message as the payload, with policy + transcript in metadata and the managed-LLM lease
// on the handoff context.
func (s *TurnService) buildHandoff(conv *domain.Conversation, req TurnRequest, history []domain.Message, sessionToken string) *domain.Handoff {
	handoffCtx := map[string]string{}
	if sessionToken != "" {
		handoffCtx["_session_token_id"] = sessionToken
	}

	// Policy precedence: an explicit per-turn policy wins (a manager may override), else
	// the policy stored on the conversation at open (ADR-0084 D9).
	policy := req.Policy
	if policy == "" {
		policy = conv.Policy
	}

	return &domain.Handoff{
		FromAgent: "chat_ingress",
		Context:   handoffCtx,
		Payload: &domain.Payload{
			Type: "text",
			Data: []byte(req.Text),
			Metadata: map[string]string{
				"_conversation_id": conv.ID,
				"policy":           policy,
				"transcript":       renderTranscript(history),
				// The conversation's posture travels with the turn so a worker cannot be
				// pointed at shared memory just because some agent default says so
				// (ADR-0084 D7).
				"profile":       string(conv.Profile),
				"recall_lookup": fmt.Sprintf("%t", conv.Profile.Recall()),
			},
		},
	}
}

// renderTranscript formats prior turns the way the session agent reads them.
func renderTranscript(history []domain.Message) string {
	if len(history) == 0 {
		return ""
	}
	var b strings.Builder
	for i, m := range history {
		if i > 0 {
			b.WriteByte('\n')
		}
		switch m.Role {
		case domain.MessageRoleAgent:
			b.WriteString("agent: ")
		case domain.MessageRoleSystem:
			b.WriteString("system: ")
		default:
			b.WriteString("customer: ")
		}
		b.WriteString(m.Content)
	}
	return b.String()
}
