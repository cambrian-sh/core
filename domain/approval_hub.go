package domain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// AddressedHub is THE approval hub (ADR-0142 D11; owner acceptance 2026-11-06).
//
// It replaces two mechanisms in one change (AGENTS.md §4.3):
//   - the operator broadcast hub (InMemoryApprovalController, deleted): ids
//     appr-<n> restarted every boot, any operator could answer anything, and
//     nothing was persisted;
//   - the contribution consent controller (InMemoryConsentController, deleted),
//     where only the beneficiary could answer (the conversation bridge rule).
//
// What it does instead:
//   - every request is ROUTED by the ApprovalPolicy to approver rules (self, a
//     named person, a role, operator, relationship) stored on the item;
//   - items are PERSISTED (ApprovalStore) with deterministic ids from the
//     request's natural key and round, so a restart neither loses a pending
//     approval nor reuses an id; a decision recorded while no requester waited
//     is handed to the next identical request rather than asked again;
//   - the ANSWER is authorised here, at answer time, against the stored rules:
//     an answerer who satisfies none is refused — an operator included, unless
//     a rule admits operators. Surfaces may pre-filter for UX; this check is
//     the authority;
//   - DELIVERY is addressed: a subscriber declares who it delivers to (an
//     approver identity, or the conversation bridge which reaches the
//     beneficiary) and receives what that approver may answer; an audit
//     subscriber (operator view) receives everything, marked answerable or not;
//   - STANDING approvals pre-approve an (action class, resource, scope) until
//     revoked or expired, and can never cover a never-self-approvable class
//     unless a non-self approver granted them;
//   - every resolution — approved, denied, expired, withdrawn, needs_hierarchy —
//     is published (HITLResolvedEvent on the operator feed, and a resolved event
//     to subscribers), closing T-20261106-withdrawn-or-timed-out-tool-approvals-publish.
//
// Fail-closed throughout: no eligible approver connected, timeout, cancellation,
// an unresolvable relationship rule and an unroutable request all deny.
type AddressedHub struct {
	store         ApprovalStore
	policy        ApprovalPolicy
	relationships ApprovalRelationships
	toolTimeout   time.Duration
	consent       time.Duration
	sweepEvery    time.Duration
	bus           EventBus
	now           func() time.Time
	logger        *slog.Logger

	mu       sync.Mutex
	waiters  map[string][]chan ApprovalItem
	subs     map[int]*approvalSub
	nextSub  int
	handlers map[string]ApprovalHandler
	notices  int
	// dupNotices holds the DuplicateNotice of each pending item by id, for a store
	// that does not persist it (Postgres): Get and List overlay it, so a console
	// that lists pending approvals sees the notice the stream delivered.
	dupNotices map[string]string
	answerMu   sync.Mutex // serialises answer/resolve read-check-write per hub
	started    bool

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	done      chan struct{}
}

// AddressedHubConfig configures the hub. Every field is optional.
type AddressedHubConfig struct {
	// Store persists approvals; nil ⇒ an in-process MemoryApprovalStore.
	Store ApprovalStore
	// Policy routes requests; nil ⇒ ConfigApprovalPolicy over DefaultApprovalPolicyConfig.
	Policy ApprovalPolicy
	// Relationships resolves relationship rules (stage 2, premium); nil ⇒ "needs hierarchy".
	Relationships ApprovalRelationships
	// DefaultTimeout bounds a request with no per-request Timeout (default 60 s,
	// the operator hub's historical grace window).
	DefaultTimeout time.Duration
	// ConsentTimeout bounds machine-consent / choose-machine prompts (default
	// 120 s, the consent controller's historical window).
	ConsentTimeout time.Duration
	// SweepInterval is how often Start's sweeper expires orphaned pending items
	// (default 30 s).
	SweepInterval time.Duration
	// Bus publishes HITLRaisedEvent / HITLResolvedEvent to the operator feed.
	Bus    EventBus
	Now    func() time.Time
	Logger *slog.Logger
}

// ApprovalHandler applies an ASYNC item's effect once approved (account
// confirmation, publish). Registered per kind at boot, so an item answered after
// a restart still takes effect. An error is recorded on the item's Detail.
type ApprovalHandler func(ctx context.Context, it ApprovalItem) error

// ApprovalAnswer is one answer to one approval.
type ApprovalAnswer struct {
	Approved bool
	// Choice is the machine chosen for a choose_machine item.
	Choice string
	// By is the verified answerer. Required.
	By Approver
	// Standing asks for a standing approval of the item's (class, resource,
	// scope); StandingTTL bounds it (0 ⇒ the policy's maximum).
	Standing    bool
	StandingTTL time.Duration
	Reason      string
}

// ApprovalWatch declares what a subscriber delivers.
type ApprovalWatch struct {
	// For is the approver this subscriber delivers to (the console's logged-in
	// person, a per-principal stream's caller).
	For Approver
	// Bridge marks the conversation bridge: it reaches an item's beneficiary
	// through the ordering conversation, so it is eligible for self-ruled items
	// that name a beneficiary.
	Bridge bool
	// All also delivers items the subscriber cannot answer (the operator audit
	// view). Eligibility — what keeps a request from failing for want of an
	// approver — never counts these.
	All bool
	// Resolutions also delivers resolution events. Legacy streams that predate
	// them (WatchApprovals / WatchToolApprovals) leave this false; they learn of
	// resolutions from the operator feed.
	Resolutions bool
	// Notices delivers consent notices (machine parked, …).
	Notices bool
	// Kinds restricts delivery to these wire kinds (nil = all).
	Kinds []string
}

// ApprovalEventType distinguishes subscriber events.
type ApprovalEventType string

const (
	ApprovalEventRaised   ApprovalEventType = "raised"
	ApprovalEventResolved ApprovalEventType = "resolved"
	ApprovalEventNotice   ApprovalEventType = "notice"
)

// ApprovalEvent is one subscriber delivery.
type ApprovalEvent struct {
	Type ApprovalEventType
	Item ApprovalItem
	// Answerable reports whether the subscriber's approver may answer the item.
	Answerable bool
	// Notice is the text of a notice event (ConsentPromptNotice).
	Notice string
}

type approvalSub struct {
	spec ApprovalWatch
	ch   chan ApprovalEvent
}

// Answer errors.
var (
	// ErrApprovalNotAddressee: the answerer satisfies none of the item's rules.
	ErrApprovalNotAddressee = errors.New("not an addressee of this approval")
	// ErrApprovalResolved: the item is no longer pending.
	ErrApprovalResolved = errors.New("approval already resolved")
	// ErrStandingNotAllowed: a standing approval was asked for a class that does
	// not allow one, or for a never-self class by the beneficiary themself.
	ErrStandingNotAllowed = errors.New("a standing approval is not allowed here")
	// ErrApprovalBadChoice: a choose_machine answer named no candidate.
	ErrApprovalBadChoice = errors.New("the choice is not one of the candidates")
)

// HITLResolvedEvent reports that a raised approval left the pending set, with
// why (ADR-0142 D11; T-20261106-withdrawn-or-timed-out-tool-approvals-publish).
// Keyed by the same id as HITLRaisedEvent, ResolveHITL and AnswerApproval.
type HITLResolvedEvent struct {
	InterventionID string
	SessionID      string
	AgentID        string
	Kind           string
	// Outcome is the terminal ApprovalState.
	Outcome    string
	ResolvedBy string
	Detail     string
}

// EventTypeHITLResolved is HITLResolvedEvent's bus type.
const EventTypeHITLResolved = "hitl.resolved"

func (HITLResolvedEvent) domainEvent()      {}
func (HITLResolvedEvent) EventType() string { return EventTypeHITLResolved }

// NewAddressedHub builds the hub.
func NewAddressedHub(cfg AddressedHubConfig) *AddressedHub {
	h := &AddressedHub{
		store:         cfg.Store,
		policy:        cfg.Policy,
		relationships: cfg.Relationships,
		toolTimeout:   cfg.DefaultTimeout,
		consent:       cfg.ConsentTimeout,
		sweepEvery:    cfg.SweepInterval,
		bus:           cfg.Bus,
		now:           cfg.Now,
		logger:        cfg.Logger,
		waiters:       map[string][]chan ApprovalItem{},
		subs:          map[int]*approvalSub{},
		handlers:      map[string]ApprovalHandler{},
		dupNotices:    map[string]string{},
		stopCh:        make(chan struct{}),
		done:          make(chan struct{}),
	}
	if h.store == nil {
		h.store = NewMemoryApprovalStore()
	}
	if h.policy == nil {
		p, err := NewConfigApprovalPolicy(DefaultApprovalPolicyConfig())
		if err != nil {
			panic("domain: default approval policy is invalid: " + err.Error()) // shipped data; a test guards it
		}
		h.policy = p
	}
	if h.toolTimeout <= 0 {
		h.toolTimeout = 60 * time.Second
	}
	if h.consent <= 0 {
		h.consent = defaultConsentGrace
	}
	if h.sweepEvery <= 0 {
		h.sweepEvery = 30 * time.Second
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	return h
}

// SetBus sets the operator-feed bus (composition roots that build the bus later).
func (h *AddressedHub) SetBus(b EventBus) { h.mu.Lock(); h.bus = b; h.mu.Unlock() }

// Policy returns the installed approval policy.
func (h *AddressedHub) Policy() ApprovalPolicy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.policy
}

// SetPolicy replaces the approval policy (ADR-0142 stage 2: the premium
// hierarchy policy decorates the config policy with approval limits and
// relationship fallbacks). Tier-1 replace-one, called at plugin Build before
// anything raises. The hub is never unsafe without it — it is constructed with
// the config policy — so this is a refinement, not an attached control
// (AGENTS.md §2.3). nil is ignored. Items already raised keep the rules they
// were routed with.
func (h *AddressedHub) SetPolicy(p ApprovalPolicy) {
	if p == nil {
		return
	}
	h.mu.Lock()
	h.policy = p
	h.mu.Unlock()
}

// WithdrawForBeneficiary resolves every PENDING approval raised for person
// (whatever kind it ran as) as withdrawn, with reason as its detail, and
// returns how many it withdrew. Deprovisioning calls it (ADR-0142 D5: a
// suspended person's pending approvals are cancelled), so nothing approved
// later acts for someone who has left.
func (h *AddressedHub) WithdrawForBeneficiary(ctx context.Context, person string, reason string) (int, error) {
	if strings.TrimSpace(person) == "" {
		return 0, nil
	}
	items, err := h.store.ListApprovals(ctx, ApprovalQuery{States: []ApprovalState{ApprovalPending}})
	if err != nil {
		return 0, fmt.Errorf("approvals: withdraw for %s: %w", person, err)
	}
	n := 0
	for _, it := range items {
		if it.Beneficiary.ID != person || !personish(it.Beneficiary.Kind) {
			continue
		}
		if _, ok := h.resolve(ctx, it, ApprovalWithdrawn, "system:deprovision", reason); ok {
			n++
		}
	}
	return n, nil
}

// SetRelationships installs the relationship resolver (stage 2, premium).
func (h *AddressedHub) SetRelationships(r ApprovalRelationships) {
	h.mu.Lock()
	h.relationships = r
	h.mu.Unlock()
}

// HandleKind registers the effect of an ASYNC kind. Later registrations replace.
func (h *AddressedHub) HandleKind(kind string, fn ApprovalHandler) {
	h.mu.Lock()
	h.handlers[kind] = fn
	h.mu.Unlock()
}

// ── lifecycle ────────────────────────────────────────────────────────────────

// Start runs a recovery sweep at once (items left pending by a previous process
// whose window has passed are expired and published) and then a periodic sweep.
// One owner; drained by Stop.
func (h *AddressedHub) Start(ctx context.Context) {
	h.startOnce.Do(func() {
		h.mu.Lock()
		h.started = true
		h.mu.Unlock()
		h.sweep(ctx)
		go func() {
			defer close(h.done)
			t := time.NewTicker(h.sweepEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-h.stopCh:
					return
				case <-t.C:
					h.sweep(ctx)
				}
			}
		}()
	})
}

// Stop ends the sweeper. Idempotent; safe without Start.
func (h *AddressedHub) Stop() {
	h.stopOnce.Do(func() {
		close(h.stopCh)
		h.mu.Lock()
		started := h.started
		h.mu.Unlock()
		if started {
			<-h.done
		}
	})
}

// sweep expires pending items whose window passed and that no live requester is
// waiting on (a live requester expires its own item on its timer).
func (h *AddressedHub) sweep(ctx context.Context) {
	items, err := h.store.ListApprovals(ctx, ApprovalQuery{States: []ApprovalState{ApprovalPending}})
	if err != nil {
		h.logger.Warn("approvals: sweep could not list pending items", "err", err)
		return
	}
	now := h.now()
	for _, it := range items {
		if it.ExpiresAt.IsZero() || now.Before(it.ExpiresAt) {
			continue
		}
		h.mu.Lock()
		live := len(h.waiters[it.ID]) > 0
		h.mu.Unlock()
		if live {
			continue
		}
		h.resolve(ctx, it, ApprovalExpired, "", "the approval window passed with no answer")
	}
}

// ── raising ─────────────────────────────────────────────────────────────────

// Request implements ApprovalController: route, persist, deliver, and block for
// an answer. Never returns Approved without an affirmative answer or an active
// standing approval.
func (h *AddressedHub) Request(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error) {
	it, dec, done, err := h.raise(ctx, req, false)
	if done || err != nil {
		return dec, err
	}
	res, err := h.await(ctx, it, h.windowFor(req))
	return decisionOf(res), err
}

// Raise persists an ASYNC request and returns at once. Its kind's handler runs
// when it is approved. Delivery does not require a connected approver: the item
// waits durably until answered or expired.
func (h *AddressedHub) Raise(ctx context.Context, req ApprovalRequest) (ApprovalItem, error) {
	it, _, _, err := h.raise(ctx, req, true)
	return it, err
}

func (h *AddressedHub) windowFor(req ApprovalRequest) time.Duration {
	if req.Timeout > 0 {
		return req.Timeout
	}
	switch req.Kind {
	case ApprovalKindMachineConsent, ApprovalKindChooseMachine:
		return h.consent
	}
	return h.toolTimeout
}

// raise does everything up to blocking. done=true means dec is final.
func (h *AddressedHub) raise(ctx context.Context, req ApprovalRequest, async bool) (ApprovalItem, ApprovalDecision, bool, error) {
	it := itemFromRequest(req)
	it.Async = async
	route, err := h.Policy().Route(ctx, it)
	if err != nil {
		return it, ApprovalDecision{}, true, fmt.Errorf("approvals: route: %w", err)
	}
	it.ActionClass = route.ActionClass
	it.Rules = route.Rules
	if len(req.PersonalIdentifiers) > 0 {
		// ENG-917: a call carrying personal identifiers outside is shown to a
		// person every time — no standing approval covers it, and its answer
		// cannot grant one.
		route.AllowStanding = false
	}
	it.AllowStanding = route.AllowStanding
	it.NeverSelf = route.NeverSelf
	if it.Resource == "" {
		it.Resource = defaultResource(it)
	}
	if it.Scope == "" {
		it.Scope = defaultScope(it)
	}
	it.NaturalKey = naturalKey(it)
	now := h.now()
	it.RaisedAt = now
	window := h.windowFor(req)
	if async && req.Timeout <= 0 {
		window = 7 * 24 * time.Hour // an async item waits for a person, not a call
	}
	it.ExpiresAt = now.Add(window)

	// 1. Standing approval.
	if route.AllowStanding {
		if s, ok := h.matchStanding(ctx, it); ok {
			it.State = ApprovalApproved
			it.StandingID = s.ID
			it.ResolvedBy = "standing:" + s.ID
			it.ResolvedAt = now
			it.Consumed = !async
			it.Detail = "pre-approved by standing approval granted by " + s.GrantedBy.Principal.String()
			it.ID = h.nextID(ctx, it)
			if err := h.store.PutApproval(ctx, it); err != nil {
				h.logger.Warn("approvals: standing-approved item not recorded", "err", err)
			}
			s.LastUsedAt, s.UseCount = now, s.UseCount+1
			if err := h.store.PutStanding(ctx, s); err != nil {
				h.logger.Warn("approvals: standing approval use not recorded", "standing", s.ID, "err", err)
			}
			h.logger.Info("approvals: pre-approved by standing approval", "event", "approval_standing_used",
				"approval", it.ID, "standing", s.ID, "class", it.ActionClass, "resource", it.Resource, "scope", it.Scope)
			h.publishResolved(ctx, it)
			if async {
				h.runHandler(ctx, it)
			}
			return it, decisionOf(it), true, nil
		}
	}

	// 2. A decision recorded for this same request that nobody took yet (it was
	// answered while no requester waited — e.g. across a restart), or a pending
	// item an identical request can join.
	if latest, found, err := h.store.LatestByKey(ctx, it.NaturalKey); err == nil && found {
		switch {
		case latest.State == ApprovalPending && now.Before(latest.ExpiresAt):
			return latest, ApprovalDecision{}, false, nil // join
		case (latest.State == ApprovalApproved || latest.State == ApprovalDenied) &&
			!latest.Consumed && now.Before(latest.ExpiresAt) && !async:
			_ = h.store.MarkConsumed(ctx, latest.ID)
			latest.Consumed = true
			return latest, decisionOf(latest), true, nil
		}
		it.Round = latest.Round + 1
	}
	it.ID = ApprovalID(it.NaturalKey, it.Round)

	// 3. Unresolvable rules fail closed, visibly.
	if len(it.Rules) == 0 {
		return h.failClosed(ctx, it, ApprovalUnroutable, "no approval rule covers action class "+it.ActionClass)
	}
	if h.onlyUnresolvableRelationships(it) {
		return h.failClosed(ctx, it, ApprovalNeedsHierarchy,
			"the approver rule ("+it.RuleSummary()+") needs the organisation hierarchy, which is not configured (ADR-0142 stage 2)")
	}
	// A request that names no beneficiary cannot be addressed by a rule that is
	// defined relative to one (self, relationship:*): the kernel could not tell
	// whom the action is for. Fail closed at once and say so, rather than as a
	// generic "unroutable" (T-20261107-tool-approvals-raised-by-chat-delegated-plan:
	// 39 such denials in one run, none logged, each retried by its agent).
	if it.Beneficiary.IsZero() && onlyBeneficiaryRelativeRules(it.Rules) {
		return h.failClosed(ctx, it, ApprovalUnroutable,
			"the request names no beneficiary (the person the action is for), so its approver rule ("+
				it.RuleSummary()+") addresses nobody")
	}
	// A hierarchy IS installed but every rule is a relationship that resolves to
	// nobody (no manager recorded, no unit head): nobody could ever answer, so
	// the request fails closed at once and says which edge is missing — rather
	// than waiting out its window as "unroutable" (ADR-0142 stage 2).
	if missing, ok := h.relationshipsResolveToNobody(ctx, it); ok {
		return h.failClosed(ctx, it, ApprovalNeedsHierarchy,
			"no "+missing+" is recorded for "+it.Beneficiary.String()+" in the organisation hierarchy, and no fallback approver is configured")
	}
	// 4. A blocking request with nobody able to answer fails closed at once
	// (the historical "no subscriber ⇒ deny"). It is LOGGED and the reason rides
	// on the decision: this branch used to deny with neither, so a misrouted
	// request looked, to the agent and to the operator, like a person saying no.
	// A rule that opted in (approvals.rules[].wait_offline, ENG-653) parks the
	// request instead: it is persisted pending and waits its window for the
	// person to connect and answer it from their inbox.
	unreachable := !async && !h.anyEligible(ctx, it)
	if unreachable && route.WaitOffline {
		h.logger.Info("approvals: no eligible approver connected; parked for its window (wait_offline)",
			"event", "approval_waiting_offline", "approval", it.ID, "class", it.ActionClass,
			"rules", it.RuleSummary(), "expires_at", it.ExpiresAt)
	} else if unreachable {
		it.State = ApprovalUnroutable
		it.ResolvedAt = now
		it.Detail = "no surface that can reach an eligible approver (" + it.RuleSummary() + ") is connected"
		if err := h.store.PutApproval(ctx, it); err != nil {
			h.logger.Warn("approvals: unroutable item not recorded", "err", err)
		}
		h.logger.Warn("approvals: request unroutable, denied", "event", "approval_unroutable",
			"approval", it.ID, "class", it.ActionClass, "rules", it.RuleSummary(), "tool", it.ToolName,
			"agent", it.AgentID, "beneficiary", it.Beneficiary.String(), "conversation", it.ConversationID,
			"detail", it.Detail)
		return it, decisionOf(it), true, nil
	}

	it.State = ApprovalPending
	if err := h.store.PutApproval(ctx, it); err != nil {
		// Durability is the point; a store failure must not silently degrade to
		// an un-persisted approval, nor approve anything.
		return it, ApprovalDecision{}, true, fmt.Errorf("approvals: persist: %w", err)
	}
	if it.DuplicateNotice != "" {
		h.mu.Lock()
		h.dupNotices[it.ID] = it.DuplicateNotice
		h.mu.Unlock()
		h.logger.Info("approvals: raised with a possible-duplicate notice", "event", "approval_duplicate_notice",
			"approval", it.ID, "tool", it.ToolName, "conversation", it.ConversationID, "notice", it.DuplicateNotice)
	}
	h.publishRaised(ctx, it)
	return it, ApprovalDecision{}, false, nil
}

func (h *AddressedHub) failClosed(ctx context.Context, it ApprovalItem, state ApprovalState, detail string) (ApprovalItem, ApprovalDecision, bool, error) {
	it.State = state
	it.ResolvedAt = h.now()
	it.Detail = detail
	if err := h.store.PutApproval(ctx, it); err != nil {
		h.logger.Warn("approvals: fail-closed item not recorded", "err", err)
	}
	h.logger.Warn("approvals: request failed closed", "event", "approval_fail_closed",
		"approval", it.ID, "state", string(state), "class", it.ActionClass, "detail", detail)
	h.publishResolved(ctx, it)
	return it, decisionOf(it), true, nil
}

// onlyBeneficiaryRelativeRules reports whether every rule is defined relative to
// the beneficiary (self, relationship:*) — i.e. none can be satisfied when the
// request names no beneficiary.
func onlyBeneficiaryRelativeRules(rules []ApprovalRule) bool {
	for _, r := range rules {
		if r.Kind != ApprovalRuleSelf && r.Kind != ApprovalRuleRelationship {
			return false
		}
	}
	return len(rules) > 0
}

func (h *AddressedHub) nextID(ctx context.Context, it ApprovalItem) string {
	if latest, found, err := h.store.LatestByKey(ctx, it.NaturalKey); err == nil && found {
		return ApprovalID(it.NaturalKey, latest.Round+1)
	}
	return ApprovalID(it.NaturalKey, 0)
}

// await blocks the requester on its item.
func (h *AddressedHub) await(ctx context.Context, it ApprovalItem, window time.Duration) (ApprovalItem, error) {
	ch := make(chan ApprovalItem, 1)
	h.mu.Lock()
	h.waiters[it.ID] = append(h.waiters[it.ID], ch)
	h.mu.Unlock()
	defer h.dropWaiter(it.ID, ch)

	// An answer can land BEFORE the waiter above was registered: raise persists
	// the item pending and publishes it to the approvers' surfaces, and only then
	// does Request call await — and a joining request (raise step 2) reaches here
	// for an item that was published long ago. finish wakes only the waiters
	// registered at resolve time, so such an answer reached nobody: the requester
	// then blocked for its whole window and returned the decision late (an
	// approved signal plan was dropped by its caller first; CI flake
	// T-20261115-addressedhub-request-loses-an-answer-that-lands). The store is
	// the truth, so re-read it now that the waiter is registered: any answer
	// that lands from here on reaches ch through finish, and any answer that
	// landed earlier is visible here. Registering first, then reading, closes
	// the gap without holding a lock across the publish.
	if cur, err := h.store.GetApproval(ctx, it.ID); err == nil && cur.State != ApprovalPending {
		if (cur.State == ApprovalApproved || cur.State == ApprovalDenied) && !cur.Consumed {
			// This requester takes the decision, exactly as finish marks it when a
			// waiter was registered, so an identical later request does not reuse it.
			if err := h.store.MarkConsumed(ctx, cur.ID); err == nil {
				cur.Consumed = true
			}
		}
		return cur, nil
	} else if err != nil {
		// Not fatal: wait as before; the timer path resolves from the store.
		h.logger.Warn("approvals: could not re-read item after registering its waiter", "approval", it.ID, "err", err)
	}

	wait := window
	if !it.ExpiresAt.IsZero() {
		if d := it.ExpiresAt.Sub(h.now()); d < wait {
			wait = d
		}
	}
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	// From here until an answer, the window or the requester's end, the requester
	// is waiting on a person — not silent. Whoever put a hook on ctx (the chat
	// turn admission) counts the span (approval_wait.go;
	// T-20261118-a-chat-turn-waiting-on-a-human-approval-is).
	defer BeginApprovalWait(ctx)()
	select {
	case res := <-ch:
		return res, nil
	case <-timer.C:
		res, _ := h.resolve(context.WithoutCancel(ctx), it, ApprovalExpired, "", "nobody answered within the approval window")
		return h.takeAfterResolve(ch, res), nil
	case <-ctx.Done():
		res, _ := h.resolve(context.WithoutCancel(ctx), it, ApprovalWithdrawn, "", withdrawnDetail(ctx))
		res = h.takeAfterResolve(ch, res)
		return ApprovalItem{State: res.State, ID: res.ID, ResolvedBy: res.ResolvedBy, Choice: res.Choice}, ctx.Err()
	}
}

// withdrawnDetail says why a requester stopped waiting. The context's cause
// names it when the canceller gave one — a chat turn that ended cancels its tool
// calls with "the chat turn that ordered this work has ended" — so the approver
// who answers late is told that, not a bare "context canceled". Only the cause's
// first line: a joined cause carries the turn's own failure after it, which is
// internal text.
func withdrawnDetail(ctx context.Context) string {
	why := ctx.Err().Error()
	if c := context.Cause(ctx); c != nil && c != ctx.Err() {
		why, _, _ = strings.Cut(c.Error(), "\n")
	}
	return "the requester stopped waiting: " + why
}

// resolvedRefusal is Answer's refusal for an item that is no longer pending. It
// wraps ErrApprovalResolved with the item's state and reason, so an approver who
// answers after the request was withdrawn or expired reads that nothing was done
// and why (AnswerApproval returns it as the refusal) — a late approve never runs
// the action silently, and never fails silently either.
func resolvedRefusal(it ApprovalItem) error {
	switch it.State {
	case ApprovalWithdrawn, ApprovalExpired:
		msg := string(it.State) + "; nothing was done"
		if it.Detail != "" {
			msg += " (" + it.Detail + ")"
		}
		return fmt.Errorf("%w: %s", ErrApprovalResolved, msg)
	}
	return ErrApprovalResolved
}

// takeAfterResolve prefers an answer that raced the timer/cancel.
func (h *AddressedHub) takeAfterResolve(ch chan ApprovalItem, fallback ApprovalItem) ApprovalItem {
	select {
	case res := <-ch:
		return res
	default:
		return fallback
	}
}

func (h *AddressedHub) dropWaiter(id string, ch chan ApprovalItem) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ws := h.waiters[id]
	for i, w := range ws {
		if w == ch {
			ws = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	if len(ws) == 0 {
		delete(h.waiters, id)
	} else {
		h.waiters[id] = ws
	}
}

// resolve CASes a pending item to a terminal state, publishes it and wakes its
// waiters. When the item was already resolved, the stored item is returned.
func (h *AddressedHub) resolve(ctx context.Context, it ApprovalItem, state ApprovalState, by, detail string) (ApprovalItem, bool) {
	h.answerMu.Lock()
	cur, err := h.store.GetApproval(ctx, it.ID)
	if err != nil {
		h.answerMu.Unlock()
		return it, false
	}
	if cur.State != ApprovalPending {
		h.answerMu.Unlock()
		return cur, false
	}
	cur.State = state
	cur.ResolvedAt = h.now()
	cur.ResolvedBy = by
	cur.Detail = detail
	ok, err := h.store.ResolveApproval(ctx, cur)
	h.answerMu.Unlock()
	if err != nil || !ok {
		if latest, gerr := h.store.GetApproval(ctx, it.ID); gerr == nil {
			return latest, false
		}
		return cur, false
	}
	h.finish(ctx, cur)
	return cur, true
}

// finish wakes waiters (marking the decision consumed when one takes it) and
// publishes the resolution.
func (h *AddressedHub) finish(ctx context.Context, it ApprovalItem) {
	h.mu.Lock()
	ws := h.waiters[it.ID]
	delete(h.waiters, it.ID)
	if it.DuplicateNotice == "" {
		it.DuplicateNotice = h.dupNotices[it.ID]
	}
	delete(h.dupNotices, it.ID) // resolved: the in-memory copy has done its job
	h.mu.Unlock()
	if len(ws) > 0 && (it.State == ApprovalApproved || it.State == ApprovalDenied) {
		if err := h.store.MarkConsumed(ctx, it.ID); err == nil {
			it.Consumed = true
		}
	}
	for _, w := range ws {
		select {
		case w <- it:
		default:
		}
	}
	h.publishResolved(ctx, it)
}

// ── answering ───────────────────────────────────────────────────────────────

// Answer records one answer. The answerer must satisfy one of the item's rules
// AT ANSWER TIME; otherwise ErrApprovalNotAddressee and nothing changes.
func (h *AddressedHub) Answer(ctx context.Context, id string, ans ApprovalAnswer) (ApprovalItem, error) {
	h.answerMu.Lock()
	it, err := h.store.GetApproval(ctx, id)
	if err != nil {
		h.answerMu.Unlock()
		return ApprovalItem{}, err
	}
	if it.State != ApprovalPending {
		h.answerMu.Unlock()
		return it, resolvedRefusal(it)
	}
	if !it.ExpiresAt.IsZero() && !h.now().Before(it.ExpiresAt) {
		h.answerMu.Unlock()
		res, _ := h.resolve(ctx, it, ApprovalExpired, "", "answered after the approval window")
		return res, resolvedRefusal(res)
	}
	if ok, why := h.answerable(ctx, it, ans.By); !ok {
		h.answerMu.Unlock()
		h.logger.Warn("approvals: answer refused", "event", "approval_answer_refused",
			"approval", id, "answerer", ans.By.Principal.String(), "rules", it.RuleSummary(), "why", why)
		return it, fmt.Errorf("%w: %s", ErrApprovalNotAddressee, why)
	}
	if it.Kind == ApprovalKindChooseMachine && ans.Approved {
		valid := false
		for _, c := range it.Candidates {
			if c == ans.Choice {
				valid = true
				break
			}
		}
		if !valid {
			h.answerMu.Unlock()
			return it, ErrApprovalBadChoice
		}
	}
	var standing *StandingApproval
	if ans.Standing {
		if !ans.Approved || !it.AllowStanding || it.Kind == ApprovalKindChooseMachine {
			h.answerMu.Unlock()
			return it, fmt.Errorf("%w: class %s does not allow standing approvals", ErrStandingNotAllowed, it.ActionClass)
		}
		if it.NeverSelf && SamePerson(ans.By.Principal, it.Beneficiary) {
			h.answerMu.Unlock()
			return it, fmt.Errorf("%w: %s is never self-approvable, so its standing approval needs a non-self approver", ErrStandingNotAllowed, it.ActionClass)
		}
		standing = h.standingFor(ctx, it, ans)
	}
	it.State = ApprovalDenied
	if ans.Approved {
		it.State = ApprovalApproved
	}
	it.ResolvedAt = h.now()
	it.ResolvedBy = ApproverLabel(ans.By.Principal)
	it.Choice = ans.Choice
	it.Detail = ans.Reason
	if standing != nil {
		it.StandingID = standing.ID
	}
	ok, err := h.store.ResolveApproval(ctx, it)
	h.answerMu.Unlock()
	if err != nil {
		return it, fmt.Errorf("approvals: record answer: %w", err)
	}
	if !ok {
		cur, _ := h.store.GetApproval(ctx, id)
		return cur, resolvedRefusal(cur)
	}
	if standing != nil {
		if err := h.store.PutStanding(ctx, *standing); err != nil {
			h.logger.Warn("approvals: standing approval not recorded", "standing", standing.ID, "err", err)
		} else {
			h.logger.Info("approvals: standing approval granted", "event", "approval_standing_granted",
				"standing", standing.ID, "class", standing.ActionClass, "resource", standing.Resource,
				"scope", standing.Scope, "granted_by", standing.GrantedBy.Principal.String(), "expires_at", standing.ExpiresAt)
		}
	}
	h.logger.Info("approvals: answered", "event", "approval_answered", "approval", id,
		"state", string(it.State), "by", it.ResolvedBy, "class", it.ActionClass)
	h.finish(ctx, it)
	if it.Async && it.State == ApprovalApproved {
		h.runHandler(ctx, it)
	}
	return it, nil
}

func (h *AddressedHub) runHandler(ctx context.Context, it ApprovalItem) {
	h.mu.Lock()
	fn := h.handlers[it.Kind]
	h.mu.Unlock()
	if fn == nil {
		h.logger.Warn("approvals: approved async item has no handler for its kind", "approval", it.ID, "kind", it.Kind)
		return
	}
	if err := fn(context.WithoutCancel(ctx), it); err != nil {
		h.logger.Warn("approvals: approved item's effect failed", "approval", it.ID, "kind", it.Kind, "err", err)
		it.Detail = "approved, but applying it failed: " + err.Error()
		// The Detail is the operator's only sign that the approved effect did
		// not apply; losing it silently would leave the item reading "approved"
		// (AGENTS.md §3.2), so a failed write is logged with the item id.
		if perr := h.store.PutApproval(context.WithoutCancel(ctx), it); perr != nil {
			h.logger.Warn("approvals: failed-effect detail not recorded", "event", "approval_detail_lost",
				"approval", it.ID, "kind", it.Kind, "err", perr)
		}
	}
}

func (h *AddressedHub) standingFor(ctx context.Context, it ApprovalItem, ans ApprovalAnswer) *StandingApproval {
	now := h.now()
	// The answerer's ctx (AGENTS.md §2.7): a policy that decorates the base
	// (premium hierarchy) sees the request's values and deadline. A routing
	// error leaves the zero route, i.e. no policy cap beyond the answer's TTL —
	// the same outcome as before; Raise already routed this item successfully.
	route, _ := h.Policy().Route(ctx, it)
	ttl := ans.StandingTTL
	if route.StandingMaxTTL > 0 && (ttl <= 0 || ttl > route.StandingMaxTTL) {
		ttl = route.StandingMaxTTL
	}
	s := &StandingApproval{
		ID:           StandingApprovalID(it.ActionClass, it.Resource, it.Scope),
		ActionClass:  it.ActionClass,
		Resource:     it.Resource,
		Scope:        it.Scope,
		Beneficiary:  it.Beneficiary,
		GrantedBy:    Approver{Principal: ans.By.Principal, Roles: append([]string(nil), ans.By.Roles...)},
		GrantedAt:    now,
		Reason:       ans.Reason,
		FromApproval: it.ID,
	}
	if ttl > 0 {
		s.ExpiresAt = now.Add(ttl)
	}
	return s
}

// matchStanding finds an active standing approval for the item whose granter
// still satisfies the item's CURRENT rules, honouring the never-self bound.
func (h *AddressedHub) matchStanding(ctx context.Context, it ApprovalItem) (StandingApproval, bool) {
	s, err := h.store.GetStanding(ctx, StandingApprovalID(it.ActionClass, it.Resource, it.Scope))
	if err != nil || !s.Active(h.now()) {
		return StandingApproval{}, false
	}
	if it.NeverSelf && SamePerson(s.GrantedBy.Principal, it.Beneficiary) {
		return StandingApproval{}, false
	}
	if ok, _ := h.answerable(ctx, it, s.GrantedBy); !ok {
		return StandingApproval{}, false
	}
	return s, true
}

// RevokeStanding ends a standing approval. The beneficiary, the granter, or an
// operator may revoke it. Idempotent: revoking a revoked grant returns it.
func (h *AddressedHub) RevokeStanding(ctx context.Context, id string, by Approver, reason string) (StandingApproval, error) {
	s, err := h.store.GetStanding(ctx, id)
	if err != nil {
		return StandingApproval{}, err
	}
	if !SamePerson(by.Principal, s.Beneficiary) && !SamePerson(by.Principal, s.GrantedBy.Principal) &&
		by.Principal != s.GrantedBy.Principal && !by.HasRole(ApprovalRoleOperator) {
		return s, fmt.Errorf("%w: only the beneficiary, the granter or an operator may revoke it", ErrApprovalNotAddressee)
	}
	if !s.RevokedAt.IsZero() {
		return s, nil
	}
	s.RevokedAt = h.now()
	s.RevokedBy = by.Principal.String()
	if reason != "" {
		s.Reason = reason
	}
	if err := h.store.PutStanding(ctx, s); err != nil {
		return s, fmt.Errorf("approvals: revoke standing: %w", err)
	}
	h.logger.Info("approvals: standing approval revoked", "event", "approval_standing_revoked",
		"standing", s.ID, "by", s.RevokedBy, "reason", reason)
	return s, nil
}

// ListStanding lists standing approvals. Expiry is judged on the hub's clock,
// the same one Request uses to decide whether a grant still pre-approves.
func (h *AddressedHub) ListStanding(ctx context.Context, q StandingQuery) ([]StandingApproval, error) {
	if q.At.IsZero() {
		q.At = h.now()
	}
	return h.store.ListStanding(ctx, q)
}

// Get returns one item.
func (h *AddressedHub) Get(ctx context.Context, id string) (ApprovalItem, error) {
	it, err := h.store.GetApproval(ctx, id)
	if err != nil {
		return it, err
	}
	return h.withNotice(it), nil
}

// List lists items.
func (h *AddressedHub) List(ctx context.Context, q ApprovalQuery) ([]ApprovalItem, error) {
	items, err := h.store.ListApprovals(ctx, q)
	for i := range items {
		items[i] = h.withNotice(items[i])
	}
	return items, err
}

// withNotice restores a pending item's DuplicateNotice when the store did not
// keep it.
func (h *AddressedHub) withNotice(it ApprovalItem) ApprovalItem {
	if it.DuplicateNotice != "" {
		return it
	}
	h.mu.Lock()
	it.DuplicateNotice = h.dupNotices[it.ID]
	h.mu.Unlock()
	return it
}

// Answerable reports whether by may answer it, and if not, why.
func (h *AddressedHub) Answerable(ctx context.Context, it ApprovalItem, by Approver) (bool, string) {
	return h.answerable(ctx, it, by)
}

func (h *AddressedHub) answerable(ctx context.Context, it ApprovalItem, by Approver) (bool, string) {
	if by.IsZero() {
		return false, "the answerer is not identified"
	}
	needsHierarchy := false
	for _, r := range it.Rules {
		switch r.Kind {
		case ApprovalRuleSelf:
			if !it.NeverSelf && SamePerson(by.Principal, it.Beneficiary) {
				return true, ""
			}
		case ApprovalRulePerson:
			if personish(by.Principal.Kind) && by.Principal.ID == r.Value {
				return true, ""
			}
		case ApprovalRuleRole:
			if by.HasRole(r.Value) {
				return true, ""
			}
		case ApprovalRuleOperator:
			if by.HasRole(ApprovalRoleOperator) {
				return true, ""
			}
		case ApprovalRuleRelationship:
			h.mu.Lock()
			rel := h.relationships
			h.mu.Unlock()
			if rel == nil {
				needsHierarchy = true
				continue
			}
			ids, err := rel.Approvers(ctx, it.Beneficiary, r.Value)
			if err != nil {
				continue // fail closed
			}
			for _, id := range ids {
				if personish(by.Principal.Kind) && by.Principal.ID == id {
					return true, ""
				}
			}
		}
	}
	why := "this approval may be answered by " + it.RuleSummary()
	if needsHierarchy {
		why += " (relationship rules need the organisation hierarchy, not configured)"
	}
	return false, why
}

// relationshipsResolveToNobody reports whether an installed resolver leaves
// every one of the item's rules unanswerable: all rules are relationships and
// each resolves to no person. A resolver error counts as nobody (fail closed).
// missing names the relations, for the visible detail.
func (h *AddressedHub) relationshipsResolveToNobody(ctx context.Context, it ApprovalItem) (missing string, nobody bool) {
	h.mu.Lock()
	rel := h.relationships
	h.mu.Unlock()
	if rel == nil || len(it.Rules) == 0 {
		return "", false
	}
	var names []string
	for _, r := range it.Rules {
		if r.Kind != ApprovalRuleRelationship {
			return "", false
		}
		ids, err := rel.Approvers(ctx, it.Beneficiary, r.Value)
		if err == nil {
			for _, id := range ids {
				if id != "" && id != it.Beneficiary.ID {
					return "", false
				}
			}
		}
		names = append(names, r.Value)
	}
	return strings.Join(names, " or "), true
}

func (h *AddressedHub) onlyUnresolvableRelationships(it ApprovalItem) bool {
	h.mu.Lock()
	rel := h.relationships
	h.mu.Unlock()
	if rel != nil {
		return false
	}
	for _, r := range it.Rules {
		if r.Kind != ApprovalRuleRelationship {
			return false
		}
	}
	return len(it.Rules) > 0
}

// ── delivery ────────────────────────────────────────────────────────────────

// Watch subscribes a surface. The returned cancel removes the subscription.
func (h *AddressedHub) Watch(spec ApprovalWatch) (<-chan ApprovalEvent, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.nextSub
	h.nextSub++
	sub := &approvalSub{spec: spec, ch: make(chan ApprovalEvent, 32)}
	h.subs[id] = sub
	return sub.ch, func() {
		h.mu.Lock()
		delete(h.subs, id)
		h.mu.Unlock()
	}
}

func (h *AddressedHub) snapshotSubs() []*approvalSub {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*approvalSub, 0, len(h.subs))
	for _, s := range h.subs {
		out = append(out, s)
	}
	return out
}

func kindWanted(spec ApprovalWatch, kind string) bool {
	if spec.Kinds == nil {
		return true
	}
	for _, k := range spec.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// eligible reports whether sub can reach someone who may answer it.
func (h *AddressedHub) eligible(ctx context.Context, sub *approvalSub, it ApprovalItem) bool {
	if !kindWanted(sub.spec, it.Kind) {
		return false
	}
	// The bridge reaches the beneficiary through the ordering conversation. An
	// item with no conversation still counts it as a subscriber (the bridge
	// leaves it to the fail-closed timeout, as the CL-2 seam always did) —
	// what it never does is let anyone but the beneficiary answer.
	if sub.spec.Bridge && !it.NeverSelf && !it.Beneficiary.IsZero() {
		for _, r := range it.Rules {
			if r.Kind == ApprovalRuleSelf {
				return true
			}
		}
	}
	if sub.spec.For.IsZero() {
		return false
	}
	ok, _ := h.answerable(ctx, it, sub.spec.For)
	return ok
}

func (h *AddressedHub) anyEligible(ctx context.Context, it ApprovalItem) bool {
	for _, s := range h.snapshotSubs() {
		if h.eligible(ctx, s, it) {
			return true
		}
	}
	return false
}

func (h *AddressedHub) publishRaised(ctx context.Context, it ApprovalItem) {
	h.mu.Lock()
	bus := h.bus
	h.mu.Unlock()
	if bus != nil {
		_ = bus.Publish(ctx, HITLRaisedEvent{
			InterventionID: it.ID,
			SessionID:      it.SessionID,
			AgentID:        it.AgentID,
			Description:    it.ToolName,
			// A skill access request widens what an agent may read, not what it
			// may do; a choice of machine changes nothing by itself.
			IsDestructive: it.Kind != ApprovalKindSkillAccess && it.Kind != ApprovalKindChooseMachine &&
				it.Kind != ApprovalKindAccountConfirm && it.Kind != ApprovalKindKnowledgeCommit,
		})
	}
	for _, s := range h.snapshotSubs() {
		elig := h.eligible(ctx, s, it)
		if !elig && !(s.spec.All && kindWanted(s.spec, it.Kind)) {
			continue
		}
		select {
		case s.ch <- ApprovalEvent{Type: ApprovalEventRaised, Item: it, Answerable: elig}:
		default: // a saturated subscriber is skipped, never waited on
		}
	}
}

func (h *AddressedHub) publishResolved(ctx context.Context, it ApprovalItem) {
	// Delivery of the resolution — to the bus's subscribers (receipts) and to
	// the hub's — must not stop because the resolving request was cancelled,
	// but it keeps that request's values (AGENTS.md §2.7).
	ctx = context.WithoutCancel(ctx)
	h.mu.Lock()
	bus := h.bus
	h.mu.Unlock()
	if bus != nil {
		_ = bus.Publish(ctx, HITLResolvedEvent{
			InterventionID: it.ID,
			SessionID:      it.SessionID,
			AgentID:        it.AgentID,
			Kind:           it.Kind,
			Outcome:        string(it.State),
			ResolvedBy:     it.ResolvedBy,
			Detail:         it.Detail,
		})
	}
	for _, s := range h.snapshotSubs() {
		if !s.spec.Resolutions || !kindWanted(s.spec, it.Kind) {
			continue
		}
		elig := h.eligible(ctx, s, it)
		if !elig && !s.spec.All {
			continue
		}
		select {
		case s.ch <- ApprovalEvent{Type: ApprovalEventResolved, Item: it, Answerable: false}:
		default:
		}
	}
}

// notify delivers a non-blocking notice (never persisted, never answered).
func (h *AddressedHub) notify(it ApprovalItem, text string) {
	h.mu.Lock()
	h.notices++
	it.ID = fmt.Sprintf("notice-%s-%d", approvalHash(it.ConversationID, it.Machine, text), h.notices)
	h.mu.Unlock()
	for _, s := range h.snapshotSubs() {
		if !s.spec.Notices && !s.spec.Bridge {
			continue
		}
		select {
		case s.ch <- ApprovalEvent{Type: ApprovalEventNotice, Item: it, Notice: text}:
		default:
		}
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// ApproverLabel is how a decision names its approver: a person (kind user) by
// bare id — the form operator approvals, schedule approvals and skill grants
// always recorded — and any other kind as kind:id.
func ApproverLabel(p PrincipalRef) string {
	if p.Kind == PrincipalUser {
		return p.ID
	}
	return p.String()
}

func itemFromRequest(req ApprovalRequest) ApprovalItem {
	return ApprovalItem{
		Kind:            req.Kind,
		ActionClass:     req.ActionClass,
		Resource:        req.Resource,
		AgentID:         req.AgentID,
		ToolName:        req.ToolName,
		ArgsPreview:     req.ArgsPreview,
		Object:          req.Object,
		Machine:         req.Machine,
		Candidates:      append([]string(nil), req.Candidates...),
		Args:            req.Args,
		DuplicateNotice: req.DuplicateNotice,
		Beneficiary:     req.Beneficiary,
		ConversationID:  req.ConversationID,
		TaskID:          req.TaskID,
		SessionID:       req.SessionID,
	}
}

func defaultResource(it ApprovalItem) string {
	switch {
	case it.Machine != "" && it.ToolName != "":
		return it.Machine + "/" + it.ToolName
	case it.ToolName != "":
		return it.ToolName
	}
	return it.Kind
}

func defaultScope(it ApprovalItem) string {
	if !it.Beneficiary.IsZero() {
		return "person:" + it.Beneficiary.ID
	}
	if it.AgentID != "" {
		return "agent:" + it.AgentID
	}
	return "kernel"
}

// naturalKey identifies "the same request" for joining a pending item and for
// reusing a decision nobody took. It covers the ROUTE (the approver rules the
// policy resolved) and the FULL arguments, not only their 200-char preview:
// with approval limits (ADR-0142 stage 2) two calls whose previews agree can
// route differently — a 4 999 payment to self, a 7 500 one to the supervisor —
// and the above-limit call must never join, or reuse the answer of, the
// self-approvable one.
func naturalKey(it ApprovalItem) string {
	return strings.Join([]string{
		it.Kind, it.ActionClass, it.Scope, it.AgentID, it.ToolName, it.Machine,
		approvalHash(it.ArgsPreview, it.Object, strings.Join(it.Candidates, ","), string(it.Args), RulesString(it.Rules)),
		it.ConversationID, it.TaskID, it.SessionID,
	}, "|")
}

func decisionOf(it ApprovalItem) ApprovalDecision {
	d := ApprovalDecision{State: it.State, ApproverID: it.ResolvedBy, Choice: it.Choice, ApprovalID: it.ID, Detail: it.Detail}
	switch it.State {
	case ApprovalApproved:
		d.Approved, d.Decided = true, true
	case ApprovalDenied:
		d.Decided = true
	}
	return d
}

// ── the consent view (ADR-0127 CL-2 seam over the one hub) ───────────────────

// Consent returns the WorkerConsentHub view the ToolExecutor and the
// conversation bridge drive. It is a VIEW: the prompts are hub items, answered
// under the hub's answer-time check (default rule: self — the beneficiary).
func (h *AddressedHub) Consent() WorkerConsentHub { return consentView{h: h} }

type consentView struct{ h *AddressedHub }

var _ WorkerConsentHub = consentView{}

func consentKind(k ConsentPromptKind) string {
	if k == ConsentPromptChooseMachine {
		return ApprovalKindChooseMachine
	}
	return ApprovalKindMachineConsent
}

func (v consentView) Request(ctx context.Context, p ConsentPrompt) (ConsentAnswer, ConsentOutcome, error) {
	dec, err := v.h.Request(ctx, ApprovalRequest{
		Kind:           consentKind(p.Kind),
		AgentID:        p.AgentID,
		ToolName:       p.Tool,
		ArgsPreview:    p.ArgsJSON,
		Object:         p.Object,
		Machine:        p.Machine,
		Candidates:     p.Candidates,
		Beneficiary:    p.Beneficiary,
		ConversationID: p.ConversationID,
		TaskID:         p.TaskID,
	})
	if err != nil {
		return ConsentAnswer{}, ConsentTimedOut, err
	}
	switch dec.State {
	case ApprovalUnroutable:
		return ConsentAnswer{}, ConsentNoSubscriber, nil
	case ApprovalApproved, ApprovalDenied:
		return ConsentAnswer{Approved: dec.Approved, Machine: dec.Choice, AnsweredBy: dec.ApproverID}, ConsentAnswered, nil
	}
	return ConsentAnswer{}, ConsentTimedOut, nil
}

func (v consentView) Notify(_ context.Context, p ConsentPrompt) {
	it := itemFromRequest(ApprovalRequest{
		Kind: ApprovalKindMachineConsent, AgentID: p.AgentID, ToolName: p.Tool, Machine: p.Machine,
		Beneficiary: p.Beneficiary, ConversationID: p.ConversationID, TaskID: p.TaskID,
	})
	v.h.notify(it, p.Notice)
}

// Watch subscribes a consent surface: it is the conversation bridge shape
// (eligible for self-ruled prompts carrying a conversation) and receives
// machine-consent prompts and notices only.
func (v consentView) Watch() (<-chan ConsentPrompt, func()) {
	evs, cancel := v.h.Watch(ApprovalWatch{
		Bridge:  true,
		Notices: true,
		Kinds:   []string{ApprovalKindMachineConsent, ApprovalKindChooseMachine},
	})
	out := make(chan ConsentPrompt, 32)
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(out)
		for {
			select {
			case <-stop:
				return
			case ev, ok := <-evs:
				if !ok {
					return
				}
				p := ev.Item.ConsentPrompt()
				if ev.Type == ApprovalEventNotice {
					p.Kind, p.Notice = ConsentPromptNotice, ev.Notice
				} else if ev.Type != ApprovalEventRaised {
					continue
				}
				select {
				case out <- p:
				case <-stop:
					return
				}
			}
		}
	}()
	return out, func() {
		once.Do(func() {
			cancel()
			close(stop)
		})
	}
}

// Submit answers a prompt as ans.By. An unidentified answerer, or one who is not
// an addressee, is refused (false).
func (v consentView) Submit(ctx context.Context, id string, ans ConsentAnswer) bool {
	_, err := v.h.Answer(ctx, id, ApprovalAnswer{
		Approved: ans.Approved, Choice: ans.Machine, By: ans.By,
	})
	return err == nil
}

// ConsentPrompt renders a consent-kind item in the CL-2 prompt shape.
func (i ApprovalItem) ConsentPrompt() ConsentPrompt {
	k := ConsentPromptApprove
	if i.Kind == ApprovalKindChooseMachine {
		k = ConsentPromptChooseMachine
	}
	return ConsentPrompt{
		ID: i.ID, Kind: k, Machine: i.Machine, Candidates: append([]string(nil), i.Candidates...),
		Tool: i.ToolName, Object: i.Object, ArgsJSON: i.ArgsPreview, TaskID: i.TaskID,
		AgentID: i.AgentID, Beneficiary: i.Beneficiary, ConversationID: i.ConversationID,
	}
}
