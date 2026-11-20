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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentpool"
	"github.com/cambrian-sh/core/internal/replycheck"
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
	// DefaultTurnTimeout bounds one dispatched turn (tool calls + LLM) when the
	// operator sets none: the composition root sets TurnTimeout from
	// execution.chat.chat_turn_timeout_seconds (default 240) and StallTimeout from
	// chat_stall_timeout_seconds (default 90).
	DefaultTurnTimeout = 240 * time.Second
	// DefaultStallTimeout is how long a turn may report NOTHING before it is treated as
	// wedged.
	//
	// Sized against what it is measuring: the emission points are memory retrieval and
	// tool execution, and a healthy turn hits one every few seconds. A model call in
	// flight on the turn's lease also counts as reporting (app.go composes the probe with
	// LatestActivity), so the window never has to cover one long generation. Ninety seconds of
	// complete silence is not slow work, it is stopped work — while still leaving ample
	// room for a single long retrieval or a slow tool.
	DefaultStallTimeout = 90 * time.Second
	// DefaultRestartNoticeWindow is how recent a turn cut off by a kernel stop must
	// be for the boot sweep to tell the person through the conversation's ingress
	// (execution.chat.restart_notice_window_seconds, ENG-2146): short, so only a
	// person who is plausibly still waiting is told.
	DefaultRestartNoticeWindow = 300 * time.Second
	// DefaultHistoryLimit is how many prior messages are threaded into a turn. Bounded
	// because prompt size — and therefore cost and latency — otherwise grows without limit
	// as a conversation lengthens.
	DefaultHistoryLimit = 40
	// tokenLimit / tokenTTL bound the per-turn managed-LLM lease.
	tokenLimit = 8192
)

// HandoffKeyTurnBudgetMs carries the turn's work budget (TurnTimeout, ms) to the
// session worker when the RPC deadline also covers time waiting on a person
// (PersonWaitAllowance): the agent plans its rounds and its wall cap against it,
// not against the longer RPC deadline.
const HandoffKeyTurnBudgetMs = "_turn_budget_ms"

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

// DelegationEnder ends the work a turn delegated (network.TurnDelegations):
// its live plans are cancelled, its leases revoked and the tool calls in flight on
// them — with any approval they wait on — cancelled. Called on EVERY exit of a turn
// (T-20261106-a-chat-turn-that-fails-deadline-exceeded-or).
type DelegationEnder interface {
	EndTurn(conversationID, originMessageID string, turnLease domain.LeaseID, cause error)
}

type TurnService struct {
	store domain.ConversationStore
	// turnCalls is the record of the side-effecting calls each turn begins (the
	// tool executor's, domain.TurnCalls; ENG-1634). With it a turn whose worker is
	// killed is re-driven once when the killed attempt began none; nil ⇒ such a
	// turn ends at once as assistant_restarted, as before.
	turnCalls    *domain.TurnCalls
	pool         Dispatcher
	acquireToken TokenAcquirer
	// deliverer sends a message outward when the conversation came through an
	// ingress (ADR-0090 D8). nil ⇒ store-only.
	deliverer Deliverer
	// leases binds the turn's conversation onto its BudgetLease so that work the turn
	// delegates to the planner can be attributed back to the conversation server-side
	// (ADR-0084 D2). Optional; without it a delegated run simply carries no conversation.
	leases domain.LeaseResolver
	// leaseAgentID is the agent every pool worker runs; it is bound onto the turn's
	// lease so that only that agent can present it (ADR-0139 D8). Empty ⇒ the lease
	// stays agent-unbound, as before.
	leaseAgentID string
	// progress receives supersedable "what is happening now" snapshots for the turn
	// (ADR-0098). nil ⇒ nothing is listening, which is the OSS default: the kernel
	// emits unconditionally and a premium bridge decides what becomes of it.
	progress domain.ProgressSink
	// liveness reports when a conversation last showed signs of work. Supplied by the
	// composition root; nil disables stall detection.
	liveness func(conversationID string) time.Time
	// personWait reports whether the turn ordered by a message is waiting on a person
	// (a human approval its tool call blocks on) and how long it has waited on one in
	// all (network.TurnDelegations.PersonWait). With it and a PersonWaitAllowance,
	// TurnTimeout counts only the time a turn spends NOT waiting on a person; nil ⇒
	// TurnTimeout is wall-clock.
	personWait func(originMessageID string) (waiting bool, waited time.Duration)
	// leaseFault reports the provider failure the LLM gateway recorded on a lease
	// (SubstrateLLMGateway.LeaseFault). Read when a turn fails, before its lease is
	// released, so the person is told the provider's failure class in plain words
	// (ENG-2124). nil ⇒ classified from the worker's error alone.
	leaseFault func(domain.LeaseID) domain.ProviderFault
	// delegations ends a turn's delegated work when the turn ends. nil ⇒ no
	// kernel-side delegation tracking (a test, or an embedder without a planner).
	delegations DelegationEnder
	// workSystems renders the MCP systems the planner's agents can call
	// (awareness.Planner.WorkSystems), so the front desk delegates a work-system ask
	// instead of answering "I have no tool for that"
	// (T-20261107-chat-agent-tells-the-user-it-has-no-tool-for). nil ⇒ no summary.
	workSystems func(ctx context.Context) string

	// calendar is the kernel's one "today" (domain.Calendar), sent on every turn as
	// the "current_date" metadata so the front desk resolves "the last two weeks"
	// against the organisation's date (T-20261107-planner-and-chat-agent-are-not-told-
	// today-s). nil ⇒ the wall clock in UTC.
	calendar *domain.Calendar

	// active holds the turns running per conversation, so closing a conversation
	// cancels them (CloseConversation).
	activeMu sync.Mutex
	active   map[string]map[*activeTurn]struct{}

	// now and stallPoll are the stall watchdog's clock and poll interval; zero means
	// time.Now and StallTimeout/4 (floor 1 s). Tests drive them with a fake clock.
	now       func() time.Time
	stallPoll time.Duration

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
	// RestartNoticeWindow: a turn the last stop cut off whose message arrived at most
	// this long before the boot is also delivered through the conversation's ingress
	// (MarkInterruptedTurns, ENG-2146); <= 0 delivers none (transcript only).
	RestartNoticeWindow time.Duration
	// PersonWaitAllowance is how much time waiting on a person (a human approval a
	// tool call of the turn blocks on) a turn may spend on top of TurnTimeout. The
	// composition root sets it to the longest approval window the kernel grants
	// (approvals default, schedules, skill access, machine consent), so any one
	// approval can run its full window from chat. 0, or no person-wait probe ⇒
	// TurnTimeout is wall-clock, as before ENG-2119.
	//
	// The turn's context deadline — which is the deadline the session worker's RPC
	// carries — is TurnTimeout + PersonWaitAllowance, because a deadline cannot be
	// extended once sent; the kernel enforces the real budget itself
	// (watchDeadline), and tells the worker its work budget as "_turn_budget_ms" so
	// the agent still plans its own work inside TurnTimeout.
	PersonWaitAllowance time.Duration
	// DefaultLanguage is the language of the kernel's fixed failure sentences when nothing
	// the user wrote identifies one (execution.chat.chat_default_language). Empty or a
	// language the catalogue lacks means English (locale.go).
	DefaultLanguage string

	// Grounding is the ADR-0145 phase-3 chat configuration (grounding.go); the
	// zero value is the pre-phase-3 turn path.
	Grounding Grounding
	g         groundingDeps

	// Lane is the ADR-0150 single-agent lane of the chat path (lane.go); the
	// zero value is the planner lane's front desk.
	Lane LaneTurn
	// composeTurn binds the person's permitted tools to a single-agent turn's
	// lease (lane.go). nil ⇒ the chat agent's own grants.
	composeTurn TurnComposer
}

// SetDelegationEnder wires the end of a turn to the end of the work it delegated.
func (s *TurnService) SetDelegationEnder(d DelegationEnder) { s.delegations = d }

// activeTurn is one running turn, cancellable by CloseConversation.
type activeTurn struct {
	cancel context.CancelFunc
	closed atomic.Bool
	// turnID is the message the turn answers and given what it was given
	// (turnContext.given): a message Emit posts into the conversation while the turn
	// runs rests on what of it the message repeats (ENG-2155, ENG-2165).
	turnID string
	given  []givenSource
	// lang is the turn's language, for the content test of the inheritance.
	lang string
}

func (s *TurnService) trackActive(conversationID, turnID string, given []givenSource, lang string, cancel context.CancelFunc) (*activeTurn, func()) {
	at := &activeTurn{cancel: cancel, turnID: turnID, given: given, lang: lang}
	s.activeMu.Lock()
	if s.active == nil {
		s.active = map[string]map[*activeTurn]struct{}{}
	}
	if s.active[conversationID] == nil {
		s.active[conversationID] = map[*activeTurn]struct{}{}
	}
	s.active[conversationID][at] = struct{}{}
	s.activeMu.Unlock()
	return at, func() {
		s.activeMu.Lock()
		delete(s.active[conversationID], at)
		if len(s.active[conversationID]) == 0 {
			delete(s.active, conversationID)
		}
		s.activeMu.Unlock()
	}
}

// CloseConversation closes a conversation and cancels the turns running in it. A
// cancelled turn ends like any other — through its own exit path, which ends the
// work it delegated (DelegationEnder) — and reports ErrConversationClosed. Closing
// is idempotent.
func (s *TurnService) CloseConversation(ctx context.Context, conversationID string) error {
	if err := s.store.SetConversationStatus(ctx, conversationID, domain.ConversationClosed); err != nil {
		return err
	}
	s.activeMu.Lock()
	running := make([]*activeTurn, 0, len(s.active[conversationID]))
	for at := range s.active[conversationID] {
		running = append(running, at)
	}
	s.activeMu.Unlock()
	for _, at := range running {
		at.closed.Store(true)
		at.cancel()
	}
	return nil
}

func (s *TurnService) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// SetLivenessProbe wires stall detection. Optional: without it a turn is bounded only by
// TurnTimeout.
func (s *TurnService) SetLivenessProbe(f func(conversationID string) time.Time) { s.liveness = f }

// SetTurnCalls gives the lane the tool executor's record of what each turn's calls
// began (domain.TurnCalls, ENG-1634; owner decision 2026-11-20): the turn's lease is
// tracked while it runs, and a turn whose worker is killed is re-driven ONCE, on the
// same lease and handoff, only when the killed attempt began no side-effecting call.
// Otherwise it fails loudly (assistant_restarted_after_action), and a second kill
// ends it too (assistant_restarted_twice).
func (s *TurnService) SetTurnCalls(t *domain.TurnCalls) { s.turnCalls = t }

// SetLeaseFaultProbe lets a failed turn read the provider failure the LLM gateway
// recorded on the turn's lease (ENG-2124): the kernel's own observation of why the
// model call failed, independent of what the worker reports.
func (s *TurnService) SetLeaseFaultProbe(f func(domain.LeaseID) domain.ProviderFault) {
	s.leaseFault = f
}

// SetPersonWaitProbe makes the turn deadline pause while the turn waits on a person
// (T-20261118-a-chat-turn-waiting-on-a-human-approval-is). A human approval has its
// own window (execution.schedules.approval_timeout_seconds, the approvals default
// timeout), routinely longer than TurnTimeout: a wall-clock deadline would cut the
// turn before the person could answer, and the person would be told the request
// failed while the approval still waited for them. The wait stays bounded — by the
// approval's window, which ends it as expired — and TurnTimeout still bounds the work.
func (s *TurnService) SetPersonWaitProbe(f func(originMessageID string) (bool, time.Duration)) {
	s.personWait = f
}

// LatestActivity combines liveness sources into one probe: a conversation was last
// active at the LATEST time any source reports. The composition root uses it to
// join progress emissions with in-flight model work (the LLM gateway reports "now"
// while a call is outstanding on the turn's lease), because a long unary generation
// emits no progress until it returns, and the progress signal alone read a healthy
// 60-90 s generation as a stall (T-20261105-slow-model-reliability-hardcoded-20-s).
// Nil sources are skipped.
func LatestActivity(sources ...func(conversationID string) time.Time) func(conversationID string) time.Time {
	return func(conversationID string) time.Time {
		var last time.Time
		for _, src := range sources {
			if src == nil {
				continue
			}
			if t := src(conversationID); t.After(last) {
				last = t
			}
		}
		return last
	}
}

// watchStall cancels the turn when it stops reporting progress.
//
// It deliberately measures SILENCE rather than elapsed time: a turn legitimately running
// for three minutes while retrieving and calling tools keeps reporting, and must not be
// killed. One that has emitted nothing for the stall window has stopped, whatever the
// clock says. "Reporting" is whatever the liveness probe counts — in production both
// progress emissions and a model call in flight on the turn's lease (LatestActivity), so
// a turn waiting on one long generation is working, not silent. So is a tool call
// blocked on a human approval: network.TurnDelegations.ConversationActivity reports
// "now" while the approval hub says the turn waits on a person, which pauses this
// watchdog for as long as the approval's own window lets the wait run (ENG-2119).
func (s *TurnService) watchStall(ctx context.Context, conversationID string, cancel context.CancelFunc, stalled *atomic.Bool) {
	if s.liveness == nil || s.StallTimeout <= 0 {
		return
	}
	// Poll at a fraction of the window so detection is prompt without being chatty.
	tick := s.stallPoll
	if tick <= 0 {
		tick = s.StallTimeout / 4
		if tick < time.Second {
			tick = time.Second
		}
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
			if silent := s.clock().Sub(last); silent > s.StallTimeout {
				stalled.Store(true)
				slog.Warn("chat: turn stopped making progress; cancelling",
					"conversation", conversationID, "silent_for", silent.Round(time.Second))
				cancel()
				return
			}
		}
	}
}

// pausesForPerson reports whether the turn deadline pauses while the turn waits on
// a person (SetPersonWaitProbe + PersonWaitAllowance).
func (s *TurnService) pausesForPerson() bool {
	return s.TurnTimeout > 0 && s.personWait != nil && s.PersonWaitAllowance > 0
}

// watchDeadline ends the turn once it has spent TurnTimeout NOT waiting on a person
// (SetPersonWaitProbe). It replaces a wall-clock context deadline only when the probe
// is wired; the turn then reports context.DeadlineExceeded exactly as before. It
// sleeps until the earliest moment the budget could run out and re-reads the probe
// then: a wait that began meanwhile has moved that moment later.
func (s *TurnService) watchDeadline(ctx context.Context, origin string, start time.Time, cancel context.CancelFunc, timedOut *atomic.Bool) {
	poll := s.stallPoll
	if poll <= 0 {
		poll = time.Second
	}
	for {
		waiting, waited := s.personWait(origin)
		if waited >= s.PersonWaitAllowance {
			// The allowance is spent: from here the wait is ordinary turn time.
			waited, waiting = s.PersonWaitAllowance, false
		}
		remaining := s.TurnTimeout - (s.clock().Sub(start) - waited)
		if !waiting && remaining <= 0 {
			timedOut.Store(true)
			cancel()
			return
		}
		next := remaining
		if waiting || next > poll {
			// While a person is awaited the budget is not being spent; re-check at
			// the poll interval for the wait's end. Otherwise never sleep past one
			// poll either, so a fake clock in tests is honoured promptly.
			next = poll
		}
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
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
// lang is the turn's language: the line is rendered in it (domain.ProgressUpdate.Language).
// turnID is the user message that opened the turn (ProgressUpdate.TurnID).
func (s *TurnService) emitProgress(ctx context.Context, conversationID, turnID string, phase domain.ProgressPhase, lang string) {
	domain.EmitProgress(ctx, s.progress, domain.ProgressUpdate{
		ConversationID: conversationID,
		TurnID:         turnID,
		Phase:          phase,
		Language:       lang,
	})
}

// NewTurnService wires the service. store and pool are required.
// SetLeaseBinder wires the lease registry so each turn's lease carries its conversation,
// and binds the lease to agentID — the agent the pool's workers run — so a turn lease
// presented by any other agent is refused (ADR-0139 D8). agentID may be empty.
func (s *TurnService) SetLeaseBinder(r domain.LeaseResolver, agentID string) {
	s.leases = r
	s.leaseAgentID = agentID
}

// SetWorkSystems wires the capability summary each turn carries as the
// "work_systems" handoff key: the MCP systems the planner's agents can call, read
// from the planner's own live tool menu (awareness.Planner.WorkSystems), so the
// front desk and the planner see one set. nil leaves turns without it.
func (s *TurnService) SetWorkSystems(f func(ctx context.Context) string) { s.workSystems = f }

// SetCalendar wires the kernel's calendar the turn handoff is dated with (nil ⇒ the
// wall clock in UTC).
func (s *TurnService) SetCalendar(c *domain.Calendar) { s.calendar = c }

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
//
// The message records the memory records it may rest on (domain.Message.SourceRefs,
// ENG-2155): posted while a turn runs in the conversation — the consent bridge's
// machine prompt, built from a step of that turn — it rests on whatever that turn
// has recalled and been given so far, so forgetting any of it redacts the message
// too. Posted with no turn running, it was built from nothing the chat lane
// recalled and records none. (Scheduled tasks and watches speak through a whole
// turn, RunOwnedTurn, so their replies carry the turn's sources.)
func (s *TurnService) Emit(ctx context.Context, conversationID, text string) (domain.Message, error) {
	refs, own := s.runningTurnRefs(ctx, conversationID, text)
	return s.emit(ctx, conversationID, text, refs, own)
}

// runningTurnRefs is the union of the sources of the turns running in the
// conversation for a message of text: each one's ledger entries so far (own) and
// what of its input the text inherits (replyRefs).
func (s *TurnService) runningTurnRefs(ctx context.Context, conversationID, text string) (refs, own []string) {
	s.activeMu.Lock()
	running := make([]*activeTurn, 0, len(s.active[conversationID]))
	for at := range s.active[conversationID] {
		running = append(running, at)
	}
	s.activeMu.Unlock()
	var sets, owns [][]string
	for _, at := range running {
		r, o := s.replyRefs(ctx, conversationID, at.turnID, at.given, text, at.lang)
		sets, owns = append(sets, r), append(owns, o)
	}
	return unionRefs(sets...), unionRefs(owns...)
}

// replyRefs is what a reply text of the turn rests on (ENG-2109, ENG-2155, ENG-2165):
// own is every record the turn recalled or called (turnSourceRefs); refs adds what the
// reply inherits from what it was given — the preceding reply's own records always,
// the rest only when the reply repeats one of their distinctive values
// (inheritedRefs; owner decision 2026-11-19, option B). A reply that repeats an earlier
// reply's value is redacted with it; an unrelated later reply is not.
func (s *TurnService) replyRefs(ctx context.Context, conversationID, turnID string, given []givenSource, reply, lang string) (refs, own []string) {
	own = s.turnSourceRefs(ctx, conversationID, turnID)
	opt := replycheck.Options{MinInteger: s.Grounding.minInteger(), Lang: lang}
	return unionRefs(own, inheritedRefs(reply, given, opt)), own
}

// emit is Emit with the memory records the message rests on (domain.Message.
// SourceRefs; a turn's reply carries its turn's, ENG-2109).
func (s *TurnService) emit(ctx context.Context, conversationID, text string, sourceRefs, ownRefs []string) (domain.Message, error) {
	if strings.TrimSpace(text) == "" {
		return domain.Message{}, errors.New("chat: emitted text is required")
	}
	msg, err := s.store.AppendMessage(ctx, domain.Message{
		ID:             newID(),
		ConversationID: conversationID,
		Role:           domain.MessageRoleAgent,
		Content:        text,
		SourceRefs:     sourceRefs,
		OwnSourceRefs:  ownRefs,
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
	// An interruption marker is not a reply: a retry of an interrupted turn runs it again,
	// as it did before the marker existed (ENG-2124).
	if req.ClientID != "" {
		if later, lerr := s.store.ListMessages(ctx, req.ConversationID, userMsg.Seq, replayScan); lerr == nil {
			for _, m := range later {
				if m.Role != domain.MessageRoleInterruption {
					return m, nil
				}
			}
		}
	}

	// The user is now waiting. Say something immediately: the gap between "sent" and the
	// first sign of life is what makes a working system indistinguishable from a hung one.
	// The language the status line and the failure line are said in. Refined once the
	// history is loaded (an "ok" in a Turkish conversation is still Turkish); the deferred
	// emission below reads whatever it holds when the turn ends.
	lang := s.turnLanguage(req.Text, nil)
	s.emitProgress(ctx, req.ConversationID, userMsg.ID, domain.PhaseUnderstanding, lang)

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
			TurnID:         userMsg.ID,
			Final:          true,
			Note:           humanFailure(turnErr, lang), // empty on success ⇒ the line is cleared
			Language:       lang,
		})
	}()

	// ENG-2124 (owner decision 2026-11-18): a turn that ends without a reply returns a
	// *domain.TurnInterruption — a plain sentence for the person, the raw cause for the
	// logs only — and leaves an interruption marker in the transcript, so the person sees
	// on reopening that the turn did not complete and why. Deferred after the progress
	// line's defer so it runs first: the status line ends on the same sentence. The
	// marker is the person's trace, never model context (history and summaries skip
	// it), which keeps the older invariant that a failed turn feeds nothing to the model.
	var fault domain.ProviderFault
	defer func() {
		if turnErr == nil || errors.Is(turnErr, domain.ErrConversationClosed) {
			return
		}
		ti := interruption(turnErr, fault, lang)
		slog.Warn("chat: turn ended without a reply",
			"conversation", req.ConversationID, "message", userMsg.ID,
			"reason", ti.Reason, "provider_fault", fault, "err", ti.Cause)
		s.markInterrupted(context.WithoutCancel(ctx), userMsg, ti.Message)
		turnErr = ti
	}()

	// The transcript: the fixed message window, or (ADR-0145 D2) the token-budgeted
	// one with the rolling summary of what no longer fits.
	var history []domain.Message
	tc := &turnContext{}
	if s.Grounding.TranscriptBudgetTokens > 0 {
		var sum conversationSummary
		history, sum, err = s.loadBudgetedHistory(ctx, conv, userMsg.Seq, userMsg.ID)
		tc.summary = sum.Text
		tc.given = append(tc.given, givenSource{text: sum.Text, onOverlap: sum.refs})
	} else {
		history, err = s.loadHistory(ctx, req.ConversationID, userMsg.Seq)
	}
	if err != nil {
		return domain.Message{}, err
	}
	lang = s.turnLanguage(req.Text, history)
	s.loadTurnContext(ctx, conv, history, tc)
	// ENG-2155/ENG-2165: the transcript the turn is given is something its reply may
	// repeat (historySources: the preceding reply's own records always, the rest on a
	// shared value).
	tc.given = append(tc.given, historySources(history)...)

	turnCtx := ctx
	turnCancel := context.CancelFunc(func() {})
	var timedOut atomic.Bool
	pauseForPerson := s.pausesForPerson()
	switch {
	case pauseForPerson:
		turnCtx, turnCancel = context.WithTimeout(ctx, s.TurnTimeout+s.PersonWaitAllowance)
	case s.TurnTimeout > 0:
		turnCtx, turnCancel = context.WithTimeout(ctx, s.TurnTimeout)
	default:
		turnCtx, turnCancel = context.WithCancel(ctx)
	}
	defer turnCancel()
	turn, untrack := s.trackActive(req.ConversationID, userMsg.ID, tc.given, lang, turnCancel)
	defer untrack()
	// The deadline counts only time not spent waiting on a person (SetPersonWaitProbe).
	if pauseForPerson {
		go s.watchDeadline(turnCtx, userMsg.ID, s.clock(), turnCancel, &timedOut)
	}

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
		tok, release, terr := s.acquireToken(turnCtx, tokenLimit, ttl)
		if terr != nil || tok == "" {
			// AGENTS.md §3.2: never a silent fallback. The turn still runs (the
			// behaviour callers rely on), but with NO lease: no conversation/run
			// binding, no act-for actor, no ask-back state, so the agent's model
			// calls or delegations may fail later with an unrelated-looking error.
			// This line is what ties that failure back to its cause (an exhausted
			// session/plan budget, a lease that could not be bound)
			// (T-20261113-chat-turn-silently-runs-with-no-llm-lease-when).
			slog.Warn("chat: turn runs WITHOUT an LLM lease; token acquisition failed",
				"conversation", req.ConversationID, "err", terr, "empty_token", terr == nil && tok == "")
		} else {
			sessionToken = tok
			defer release()
			// Stamp the conversation and the ordering turn onto the lease. If the worker
			// delegates to the planner, Execute resolves this lease and links the session
			// it opens back to this exchange — without the agent naming it.
			if s.leases != nil {
				b := domain.LeaseBinding{
					ConversationID:  conv.ID,
					OriginMessageID: userMsg.ID,
					AgentID:         s.leaseAgentID,
					// Operator act-for: who opened this turn for the owner (audit).
					ActedBy: domain.ActForActorFromContext(ctx),
				}
				// ADR-0145 D5a: this message answers the previous turn's ask-back;
				// Execute resumes from the pending state named here, by the kernel.
				if tc.pending != nil {
					b.AskBackRound = tc.pending.Round
					if b.AskBackRound <= 0 {
						b.AskBackRound = 1
					}
					b.AskBackPendingRef = tc.pendingRef
				}
				s.leases.BindLease(domain.LeaseID(tok), b)
			}
			// ADR-0150 D3: the merged agent holds the person's permitted tools,
			// bound to this turn's lease.
			if s.Lane.SingleAgent && s.composeTurn != nil {
				if cerr := s.composeTurn(turnCtx, domain.LeaseID(tok), req.Text); cerr != nil {
					slog.Warn("chat: single-agent turn not composed; the agent runs on its own grants",
						"conversation", req.ConversationID, "err", cerr)
				}
			}
		}
	}

	// Whatever ends this turn — a reply, the deadline, a stall, a failure, a closed
	// conversation — ends the work it delegated: its plans are cancelled, its leases
	// revoked, and approvals they wait on withdrawn, so nothing goes on acting for a
	// request the user has already been answered on. Deferred AFTER the lease release
	// so it runs BEFORE it: the turn lease is still bound while its work is ended.
	if s.delegations != nil {
		defer func() {
			s.delegations.EndTurn(req.ConversationID, userMsg.ID, domain.LeaseID(sessionToken), turnErr)
		}()
	}

	// Everything after this point is the agent's own loop, which is where the minutes go.
	s.emitProgress(ctx, req.ConversationID, userMsg.ID, domain.PhaseWorking, lang)

	h := s.buildHandoff(turnCtx, conv, req, history, sessionToken)
	s.addTurnContext(h, tc)
	lease := domain.LeaseID(sessionToken)
	if s.turnCalls != nil && lease != "" {
		s.turnCalls.Track(lease)
		defer s.turnCalls.Forget(lease)
	}
	var resp *domain.Handoff
	// attempt counts the dispatches that REACHED a worker; redriving is set once the
	// turn is being re-sent after a loss (ENG-1634/ENG-2266).
	attempt, redriving := 0, false
	for {
		resp, err = s.pool.Dispatch(turnCtx, h)
		// Read while the turn's lease is still held: its release forgets the record.
		if s.leaseFault != nil && lease != "" {
			fault = s.leaseFault(lease)
		}
		if err == nil || stalled.Load() || turn.closed.Load() || timedOut.Load() || turnCtx.Err() != nil {
			break
		}
		// ENG-2266: a re-sent turn that never reached a worker (the dead worker's socket
		// could not be dialled, or the pool found it gone) is not a second loss under
		// the turn: wait for the worker's replacement again and re-send, within the
		// turn's deadline.
		if redriving && agentpool.NotDelivered(err) {
			if werr := s.awaitWorker(turnCtx, err); werr != nil {
				if end := workerOutOfService(req.ConversationID, userMsg.ID, werr, err, lang); end != nil {
					err = end
				}
				break
			}
			continue
		}
		attempt++
		redrive, end := s.afterWorkerLoss(req.ConversationID, userMsg.ID, lease, attempt, err, fault, lang)
		if !redrive {
			if end != nil {
				err = end
			}
			break
		}
		// ENG-2266: the killed worker stays registered until the crash watcher sees its
		// exit and is absent until its replacement boots; re-sending at once dials a dead
		// socket. Wait for the replacement first, bounded by the turn's deadline.
		if werr := s.awaitWorker(turnCtx, err); werr != nil {
			if end := workerOutOfService(req.ConversationID, userMsg.ID, werr, err, lang); end != nil {
				err = end
			}
			break
		}
		redriving = true
		s.emitProgress(ctx, req.ConversationID, userMsg.ID, domain.PhaseWorking, lang)
	}
	if err != nil {
		// Distinguish "we gave up on a wedged turn" from an ordinary cancellation, so the
		// user is told what actually happened rather than a generic failure.
		if stalled.Load() {
			return domain.Message{}, ErrTurnStalled
		}
		if turn.closed.Load() {
			return domain.Message{}, domain.ErrConversationClosed
		}
		if timedOut.Load() {
			return domain.Message{}, fmt.Errorf("chat: turn deadline: %w", context.DeadlineExceeded)
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
	// ADR-0145 D8: the reply check (observe or enforce) over the turn's evidence,
	// then the evidence markers are stripped. The runs the turn delegated are read
	// kernel-side, before the turn's end forgets them.
	var results []domain.ExecuteResult
	if s.g.results != nil {
		results = s.g.results(userMsg.ID)
	}
	reply = s.groundReply(turnCtx, ctx, conv, userMsg, history, h, reply, results, tc, lang)
	if reply == "" {
		return domain.Message{}, ErrEmptyReply
	}
	// The reply disclosure guard (disclosure.go): a deterministic check, after whatever the
	// agent's own prompts and guards did, that the reply does not state a personal
	// identifier it declares withheld, nor a strong identifier to a customer. Values the
	// user wrote themselves are never masked.
	if guarded, held := guardDisclosure(reply, conv.Profile, userTexts(req.Text, history), lang); held.total() > 0 {
		slog.Warn("chat: reply disclosure guard held back personal identifiers",
			"conversation", req.ConversationID, "held", held)
		reply = guarded
	}

	// Through Emit, so a reply to an ingress-borne conversation reaches the person
	// who asked. A DELIVERY failure does not fail the turn: the reply is already
	// stored, the synchronous caller still gets it, and the undeliverable message is
	// dead-lettered by the delivery path rather than swallowed here.
	// ADR-0098 D3: the answer supersedes the progress. A user must never be left looking at
	// "working on it" after the reply has arrived.
	s.emitProgress(ctx, req.ConversationID, userMsg.ID, domain.PhaseWriting, lang)

	// ENG-2109/ENG-2155: the reply records what the turn recalled and called and what
	// it was given, so forgetting any of those records later redacts it.
	refs, own := s.replyRefs(ctx, conv.ID, userMsg.ID, tc.given, reply, lang)
	msg, derr := s.emit(ctx, req.ConversationID, reply, refs, own)
	if derr != nil {
		slog.Warn("ADR-0090: turn reply stored but not delivered",
			"conversation", req.ConversationID, "message", msg.ID, "err", derr)
	}
	if msg.ID == "" {
		return domain.Message{}, derr
	}
	// ADR-0145 D2/D5a: the turn's index entry (and any ask-back it left open).
	// The index entry keeps the broad refs (what the turn recalled and everything it was
	// given, ENG-2155), so an erase of any of them deletes it; a later turn shown it
	// inherits them only on a shared value (ENG-2165).
	s.recordTurnIndex(context.WithoutCancel(ctx), conv, userMsg, results, unionRefs(refs, broadRefs(tc.given)))
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
	// Interruption markers are the person's trace, not model context (ENG-2124).
	out := make([]domain.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Seq < upToSeq && m.Role != domain.MessageRoleInterruption {
			out = append(out, m)
		}
	}
	return out, nil
}

// buildHandoff produces the per-turn contract the session agent already expects (ADR-0080):
// the message as the payload, with policy + transcript in metadata and the managed-LLM lease
// on the handoff context. ctx is the turn's context; it reaches the work-systems summary,
// whose tool menu lookups are I/O.
func (s *TurnService) buildHandoff(ctx context.Context, conv *domain.Conversation, req TurnRequest, history []domain.Message, sessionToken string) *domain.Handoff {
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

	md := map[string]string{
		"_conversation_id": conv.ID,
		"policy":           policy,
		"transcript":       renderTranscript(history),
		// The conversation's posture travels with the turn so a worker cannot be
		// pointed at shared memory just because some agent default says so
		// (ADR-0084 D7).
		"profile":       string(conv.Profile),
		"recall_lookup": fmt.Sprintf("%t", conv.Profile.Recall()),
		// The conversation's language, so the worker's own fixed fallback sentence is
		// said in it (agents/chat_agent.py). Detected here once, deterministically, from
		// what the user wrote — never by a model (locale.go).
		"language": s.turnLanguage(req.Text, history),
		// Today, in the organisation's timezone, with the reference dates of the
		// common relative asks — computed by the kernel (domain.Calendar), so the
		// front desk neither guesses the date nor does the date arithmetic; it
		// subtracted "two weeks" forward once (o1, 2026-11-07).
		"current_date": s.calendar.Block(),
	}
	// The systems the planner's agents can reach, so a "what's in my CRM?" ask is
	// delegated rather than answered "I have no tool" from the front desk's own
	// skills menu (T-20261107-chat-agent-tells-the-user-it-has-no-tool-for). Not for
	// a customer-facing conversation: the summary names the deployment's internal
	// systems, which an external end user is not told about; such a turn can still
	// delegate, it is only not shown the list.
	// ADR-0150 D3/D4: in the single-agent lane the agent holds the tools itself
	// (no work-systems list of what a planner could do) and runs under the lane's
	// stopping rule.
	if s.Lane.SingleAgent {
		s.Lane.Limits.Stamp(handoffCtx)
	}
	// The worker's RPC deadline includes the person-wait allowance; its own work
	// budget is TurnTimeout (agents/chat_agent.py budgets against the smaller).
	if s.pausesForPerson() {
		handoffCtx[HandoffKeyTurnBudgetMs] = strconv.FormatInt(s.TurnTimeout.Milliseconds(), 10)
	}
	if s.workSystems != nil && conv.Profile != domain.ProfileCustomer && !s.Lane.SingleAgent {
		if ws := strings.TrimSpace(s.workSystems(ctx)); ws != "" {
			md["work_systems"] = ws
		}
	}

	return &domain.Handoff{
		FromAgent: "chat_ingress",
		Context:   handoffCtx,
		Payload: &domain.Payload{
			Type:     "text",
			Data:     []byte(req.Text),
			Metadata: md,
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

// afterWorkerLoss decides what a failed dispatch leads to when the worker was
// killed under the turn (ENG-1634; owner decision 2026-11-20). redrive=true: the
// killed attempt began no side-effecting call, this was the first loss, and the
// lane can tell (it tracks the turn's calls) — dispatch the same handoff again on
// the same lease. Otherwise end, when non-nil, is the loud failure that replaces
// err: the attempt had already begun an action (a re-run could do it twice), or
// the re-drive's worker was killed too. A failure that is not a worker loss
// returns (false, nil): the turn ends with its own error, as before.
func (s *TurnService) afterWorkerLoss(conversationID, turnID string, lease domain.LeaseID, attempt int, err error,
	fault domain.ProviderFault, lang string) (redrive bool, end error) {
	if failureReason(err, fault) != domain.TurnInterruptAssistantRestarted {
		return false, nil
	}
	if s.turnCalls == nil || lease == "" {
		// Nothing tells what the attempt began: never re-run it blind.
		return false, nil
	}
	if attempt >= 2 {
		slog.Warn("chat: the re-driven turn's worker was lost too; ending the turn",
			"conversation", conversationID, "message", turnID, "err", err)
		return false, &domain.TurnInterruption{Reason: domain.TurnInterruptAssistantRestartedTwice,
			Message: Message(MsgAssistantRestartedTwice, lang), Cause: err}
	}
	if n, tools := s.turnCalls.Begun(lease); n > 0 {
		slog.Warn("chat: worker lost after the turn began side-effecting calls; not re-driving it",
			"conversation", conversationID, "message", turnID, "calls", n, "tools", tools, "err", err)
		return false, &domain.TurnInterruption{Reason: domain.TurnInterruptAssistantRestartedAfterAction,
			Message: Message(MsgAssistantRestartedAfterAction, lang),
			Cause:   fmt.Errorf("%w (the killed attempt began %d side-effecting call(s): %s)", err, n, strings.Join(tools, ", "))}
	}
	slog.Warn("chat: worker lost under the turn before it began any side-effecting call; re-driving it once",
		"conversation", conversationID, "message", turnID, "err", err)
	s.turnCalls.Redrive(lease)
	return true, nil
}

// workerOutOfService turns a wait for a replacement that ended because the worker's
// stream was quarantined as a crash-loop (agentpool.ErrWorkerQuarantined) into the
// turn's end: no replacement will boot until an operator acts, so the turn ends at
// once with a reason that says so, instead of waiting for the stall cut and telling
// the person it "stopped making progress" (ENG-2269; owner decision 2026-11-20).
// Any other wait error (the turn's own deadline) returns nil: the turn ends as before.
func workerOutOfService(conversationID, turnID string, werr, err error, lang string) error {
	if !errors.Is(werr, agentpool.ErrWorkerQuarantined) {
		return nil
	}
	slog.Warn("chat: the turn's worker stream was quarantined as a crash-loop; ending the turn",
		"conversation", conversationID, "message", turnID, "wait", werr, "err", err)
	return &domain.TurnInterruption{Reason: domain.TurnInterruptAssistantUnavailable,
		Message: Message(MsgAssistantUnavailable, lang), Cause: fmt.Errorf("%w (after: %w)", werr, err)}
}

// workerAwaiter is the optional half of the pool a re-drive waits on
// (agentpool.Pool.AwaitReplacement, ENG-2266).
type workerAwaiter interface {
	AwaitReplacement(ctx context.Context, err error) error
}

// awaitWorker waits, bounded by ctx (the turn's deadline), until the worker the
// failed dispatch named can serve again. A pool that cannot tell returns at once.
func (s *TurnService) awaitWorker(ctx context.Context, err error) error {
	w, ok := s.pool.(workerAwaiter)
	if !ok {
		return nil
	}
	return w.AwaitReplacement(ctx, err)
}
