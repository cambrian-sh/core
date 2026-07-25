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
	"strings"
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
type TurnService struct {
	store        domain.ConversationStore
	pool         Dispatcher
	acquireToken TokenAcquirer

	TurnTimeout  time.Duration
	HistoryLimit int
}

// NewTurnService wires the service. store and pool are required.
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
func (s *TurnService) Turn(ctx context.Context, req TurnRequest) (domain.Message, error) {
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

	history, err := s.loadHistory(ctx, req.ConversationID, userMsg.Seq)
	if err != nil {
		return domain.Message{}, err
	}

	turnCtx := ctx
	if s.TurnTimeout > 0 {
		var cancel context.CancelFunc
		turnCtx, cancel = context.WithTimeout(ctx, s.TurnTimeout)
		defer cancel()
	}

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
		}
	}

	h := s.buildHandoff(conv, req, history, sessionToken)
	resp, err := s.pool.Dispatch(turnCtx, h)
	if err != nil {
		return domain.Message{}, err // ErrPoolBusy / ErrWorkerLost pass through
	}

	reply := ""
	if resp != nil && resp.Payload != nil {
		reply = strings.TrimSpace(string(resp.Payload.Data))
	}
	if reply == "" {
		return domain.Message{}, ErrEmptyReply
	}

	return s.store.AppendMessage(ctx, domain.Message{
		ID:             newID(),
		ConversationID: req.ConversationID,
		Role:           domain.MessageRoleAgent,
		Content:        reply,
	})
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
		FromAgent: "chat_manager",
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
