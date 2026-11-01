package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// SpendMeter is the one LLM spend ledger of the kernel (ADR-0131 D1).
//
// Every LLM call reaches it through exactly one of two doors, both of which
// exist anyway and neither of which can be skipped:
//
//   - Provider.Acquire, for every system organ (planner, router, replan,
//     checkpoint, thought steps, tier-2 scoring, verification, interview, the
//     Ingress Studio's drafting model). The meter is the OUTERMOST decorator on
//     every acquired generator, so a call refused at the ceiling never takes a
//     concurrency slot, and a new organ is metered by construction.
//   - SubstrateLLMGateway, for every agent stream and agent tool turn (the
//     ADR-0018 managed path, which includes the retrieval_agent that runs the
//     agentic retrieval chain).
//
// There is no third ledger: the gateway's per-lease counters are lease
// accounting (Pass-1 cut-off, BudgetOverrun), and they are fed from the same
// reconciled usage the meter records.
//
// The meter keys spend by the session id carried in ctx (domain.WithSessionID).
// Properties of the call's ctx decide where it is booked and whether the
// ceiling applies (owner decision 2026-11-01,
// T-20261101-router-classification-and-async-organs):
//
//   - a session in ctx: booked to it, refused over the ceiling (the request
//     path — router, planner, agent steps, replans, checkpoints, thoughts);
//   - a session plus domain.WithBackgroundSpend: booked to the ORIGINATING session but
//     never refused (verification, tier-2 scoring — work that outlives the
//     request, where a refusal would silently drop the organ's job);
//   - a SpendTab (WithSpendTab) and no session: the session is not known yet
//     (router classification before Execute opens one); the tab is enforced
//     against its own total and SettleTab later moves it into the session;
//   - none of these: the explicit kernel no-session bucket (NoSessionBucket —
//     registration-triggered interviews, maintenance), reported by
//     ReportNoSession and never refused: there is no session whose ceiling it
//     could exceed.
//
// These are properties of a call on the one meter, not a second ledger.
//
// The ceiling (execution.session.max_session_tokens) is enforced at ADMISSION:
// a call is refused with ErrSpendCeiling when the session's recorded total plus
// the call's own prompt estimate would exceed it. Output size is unknown before
// the call, so the last admitted call can overshoot by its own output, and
// calls admitted concurrently can each overshoot by theirs — the ceiling bounds
// what is STARTED, not to the token what is billed. Once a session has been
// refused it stays refused (Exhausted), so a plan cannot creep forward on
// calls whose prompts happen to be small.
type SpendMeter struct {
	ceiling int
	prices  PriceReader
	log     *slog.Logger

	mu        sync.Mutex
	sessions  map[domain.SessionID]*sessionSpend
	noSession SessionSpend
	// noSessionReported is the noSession total at the last ReportNoSession.
	noSessionReported SessionSpend
}

// NoSessionBucket names the kernel no-session spend bucket in logs and
// reports: LLM calls with no originating session (ADR-0131; owner decision
// 2026-11-01). Metered and reported, never enforced.
const NoSessionBucket = "kernel:no-session"

type sessionSpend struct {
	SessionSpend
	lastActivity time.Time
}

// SessionSpend is the running total for one session.
type SessionSpend struct {
	Calls           int
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	CostUSD         float64
	// EstimatedCalls counts the calls whose usage came from the len/4 fallback
	// (the provider reported none). Non-zero means the totals are partly an
	// estimate (ADR-0131 D2).
	EstimatedCalls int
	// Exhausted is set the first time a call was refused at the ceiling.
	Exhausted bool
}

// TotalTokens is input + output.
func (s SessionSpend) TotalTokens() int { return s.InputTokens + s.OutputTokens }

// Sub returns s minus base, field-wise — the spend between two snapshots.
// Exhausted is s's own flag.
func (s SessionSpend) Sub(base SessionSpend) SessionSpend {
	return SessionSpend{
		Calls:           s.Calls - base.Calls,
		InputTokens:     s.InputTokens - base.InputTokens,
		OutputTokens:    s.OutputTokens - base.OutputTokens,
		CacheReadTokens: s.CacheReadTokens - base.CacheReadTokens,
		CostUSD:         s.CostUSD - base.CostUSD,
		EstimatedCalls:  s.EstimatedCalls - base.EstimatedCalls,
		Exhausted:       s.Exhausted,
	}
}

// ErrSpendCeiling is the sentinel every ceiling refusal matches with errors.Is.
var ErrSpendCeiling = errors.New("llm spend ceiling reached")

// SpendCeilingError is the typed refusal (ADR-0131 D3). It names the session,
// the purpose that was refused, and the numbers, so the plan's visible failure
// says why without a log dive.
type SpendCeilingError struct {
	SessionID domain.SessionID
	Purpose   string
	Used      int // tokens already recorded for the session
	Requested int // the refused call's prompt estimate
	Ceiling   int
}

func (e *SpendCeilingError) Error() string {
	return fmt.Sprintf("%s: session %s has used %d of %d tokens (execution.session.max_session_tokens); %s call of ~%d prompt tokens refused",
		ErrSpendCeiling.Error(), e.SessionID, e.Used, e.Ceiling, e.Purpose, e.Requested)
}

// Is makes errors.Is(err, ErrSpendCeiling) hold for the typed error.
func (e *SpendCeilingError) Is(target error) bool { return target == ErrSpendCeiling }

// NewSpendMeter builds a meter. ceiling <= 0 disables enforcement (metering
// still runs); prices may be nil, in which case cost is recorded as 0.
func NewSpendMeter(ceiling int, prices PriceReader, log *slog.Logger) *SpendMeter {
	if ceiling < 0 {
		ceiling = 0
	}
	if log == nil {
		log = slog.Default()
	}
	return &SpendMeter{
		ceiling:  ceiling,
		prices:   prices,
		log:      log,
		sessions: make(map[domain.SessionID]*sessionSpend),
	}
}

// Ceiling returns the configured per-session token ceiling (0 = off).
func (m *SpendMeter) Ceiling() int {
	if m == nil {
		return 0
	}
	return m.ceiling
}

// Admit decides whether a call with the given prompt may start for the session
// in ctx. It returns a *SpendCeilingError (matching ErrSpendCeiling) when the
// session is over its ceiling. Nil-safe: a nil meter admits everything.
func (m *SpendMeter) Admit(ctx context.Context, purpose string, promptTokens int) error {
	if m == nil || domain.IsBackgroundSpend(ctx) {
		// Background organs are booked, never refused (owner decision 2026-11-01).
		return nil
	}
	if sid, ok := domain.SessionIDFromContext(ctx); ok && sid != "" {
		return m.AdmitSession(sid, purpose, promptTokens)
	}
	if tab := spendTabFrom(ctx); tab != nil {
		return m.admitTab(tab, purpose, promptTokens)
	}
	return nil
}

// admitTab enforces the ceiling for calls made before the session exists: a
// session that does not exist yet starts at zero, so the tab is checked against
// its own total. Once settled, the tab's calls are the session's.
func (m *SpendMeter) admitTab(tab *SpendTab, purpose string, promptTokens int) error {
	if m.ceiling <= 0 {
		return nil
	}
	tab.mu.Lock()
	if tab.settled {
		sid := tab.settledTo
		tab.mu.Unlock()
		return m.AdmitSession(sid, purpose, promptTokens)
	}
	defer tab.mu.Unlock()
	used := tab.spend.TotalTokens()
	if !tab.spend.Exhausted && used+promptTokens <= m.ceiling {
		return nil
	}
	tab.spend.Exhausted = true
	return &SpendCeilingError{Purpose: purpose, Used: used, Requested: promptTokens, Ceiling: m.ceiling}
}

// AdmitSession is Admit for a caller that resolved the session itself (the
// gateway reads it from the lease binding, which is authoritative there).
func (m *SpendMeter) AdmitSession(sid domain.SessionID, purpose string, promptTokens int) error {
	if m == nil || m.ceiling <= 0 || sid == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessionLocked(sid)
	used := s.TotalTokens()
	if !s.Exhausted && used+promptTokens <= m.ceiling {
		return nil
	}
	if !s.Exhausted {
		m.log.Warn("llm spend ceiling reached; refusing further calls for this session",
			"session", sid, "purpose", purpose, "used_tokens", used, "ceiling", m.ceiling,
			"adr", "ADR-0131 D3")
	}
	s.Exhausted = true
	return &SpendCeilingError{SessionID: sid, Purpose: purpose, Used: used, Requested: promptTokens, Ceiling: m.ceiling}
}

// Record books one finished call against the session in ctx.
func (m *SpendMeter) Record(ctx context.Context, purpose, generatorID string, u TokenUsage) {
	if m == nil {
		return
	}
	sid, _ := domain.SessionIDFromContext(ctx)
	if sid == "" {
		if tab := spendTabFrom(ctx); tab != nil {
			tab.mu.Lock()
			if !tab.settled {
				tab.spend.add(u, m.cost(generatorID, u))
				tab.mu.Unlock()
				return
			}
			sid = tab.settledTo
			tab.mu.Unlock()
		}
	}
	m.RecordSession(sid, purpose, generatorID, u)
}

// RecordSession books one finished call against sid ("" = the kernel
// no-session bucket, NoSessionBucket).
func (m *SpendMeter) RecordSession(sid domain.SessionID, purpose, generatorID string, u TokenUsage) {
	if m == nil {
		return
	}
	cost := m.cost(generatorID, u)
	m.mu.Lock()
	var s *SessionSpend
	if sid == "" {
		s = &m.noSession
	} else {
		s = &m.sessionLocked(sid).SessionSpend
	}
	s.add(u, cost)
	m.mu.Unlock()
	bucket := string(sid)
	if sid == "" {
		bucket = NoSessionBucket
	}
	m.log.Debug("llm spend recorded", "session", bucket, "purpose", purpose, "generator", generatorID,
		"input", u.PromptTokens, "output", u.CompletionTokens, "cache_read", u.CacheReadTokens,
		"cost_usd", cost, "estimated", u.Estimated)
}

// Session returns a copy of sid's running total (zero for an unknown session).
func (m *SpendMeter) Session(sid domain.SessionID) SessionSpend {
	if m == nil || sid == "" {
		return SessionSpend{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[sid]; ok {
		return s.SessionSpend
	}
	return SessionSpend{}
}

// NoSession returns the kernel no-session bucket (NoSessionBucket): the total
// of calls that had no originating session.
func (m *SpendMeter) NoSession() SessionSpend {
	if m == nil {
		return SessionSpend{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.noSession
}

// ReportNoSession logs what the kernel no-session bucket accrued since the last
// report and returns that delta. The bucket is reported, never enforced; the
// gateway's periodic eviction sweep calls this so the figure reaches the log
// without a per-call line. Nothing accrued: zero delta, no log.
func (m *SpendMeter) ReportNoSession() SessionSpend {
	if m == nil {
		return SessionSpend{}
	}
	m.mu.Lock()
	d := m.noSession.Sub(m.noSessionReported)
	m.noSessionReported = m.noSession
	m.mu.Unlock()
	if d.Calls == 0 {
		return d
	}
	m.log.Info("llm spend outside any session", "bucket", NoSessionBucket,
		"calls", d.Calls, "input", d.InputTokens, "output", d.OutputTokens,
		"cache_read", d.CacheReadTokens, "cost_usd", d.CostUSD,
		"estimated_calls", d.EstimatedCalls, "adr", "ADR-0131")
	return d
}

// SettleTab moves a pending tab's spend into sid — the session Execute resolved
// or opened after the calls were made — or into the no-session bucket when sid
// is "" (a CHAT or clarification turn that never gets a session). Calls made
// under the tab's ctx after settlement are booked to sid directly. Idempotent;
// nil-safe.
func (m *SpendMeter) SettleTab(tab *SpendTab, sid domain.SessionID) {
	if m == nil || tab == nil {
		return
	}
	tab.mu.Lock()
	if tab.settled {
		tab.mu.Unlock()
		return
	}
	tab.settled, tab.settledTo = true, sid
	pending := tab.spend
	tab.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if sid == "" {
		pending.Exhausted = false // the bucket is never enforced
		m.noSession.merge(pending)
		return
	}
	m.sessionLocked(sid).merge(pending)
}

func (s *SessionSpend) add(u TokenUsage, cost float64) {
	s.Calls++
	s.InputTokens += u.PromptTokens
	s.OutputTokens += u.CompletionTokens
	s.CacheReadTokens += u.CacheReadTokens
	s.CostUSD += cost
	if u.Estimated {
		s.EstimatedCalls++
	}
}

func (s *SessionSpend) merge(o SessionSpend) {
	s.Calls += o.Calls
	s.InputTokens += o.InputTokens
	s.OutputTokens += o.OutputTokens
	s.CacheReadTokens += o.CacheReadTokens
	s.CostUSD += o.CostUSD
	s.EstimatedCalls += o.EstimatedCalls
	s.Exhausted = s.Exhausted || o.Exhausted
}

// ── Attribution properties carried in ctx ───────────────────────────────────

// The background-organ marker (book to the originating session, never refuse)
// is domain.WithBackgroundSpend, beside domain.WithSessionID, so the organs
// that set it need no import of this package.

type spendTabKey struct{}

// SpendTab collects request-path spend made before the session is known:
// router classification runs before Execute resolves or opens the session, and
// opening one first would create sessions for CHAT turns. Settle it with
// SpendMeter.SettleTab once the session is known.
type SpendTab struct {
	mu        sync.Mutex
	spend     SessionSpend
	settled   bool
	settledTo domain.SessionID
}

// Spend returns what the tab holds while unsettled (zero once settled).
func (t *SpendTab) Spend() SessionSpend {
	if t == nil {
		return SessionSpend{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.settled {
		return SessionSpend{}
	}
	return t.spend
}

// WithSpendTab returns a child ctx whose session-less calls are booked to a
// fresh tab. A session id in ctx still wins over the tab.
func WithSpendTab(ctx context.Context) (context.Context, *SpendTab) {
	t := &SpendTab{}
	return context.WithValue(ctx, spendTabKey{}, t), t
}

func spendTabFrom(ctx context.Context) *SpendTab {
	t, _ := ctx.Value(spendTabKey{}).(*SpendTab)
	return t
}

// EvictIdle forgets sessions with no recorded or refused call for longer than
// idle. idle <= 0 is a no-op. The ledger is in-memory and per session, so
// without this it grows by one small entry per session for the life of the
// kernel; callers pass the session retention window, so a session is never
// forgotten while the session store would still accept work for it.
func (m *SpendMeter) EvictIdle(now time.Time, idle time.Duration) {
	if m == nil || idle <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if now.Sub(s.lastActivity) > idle {
			delete(m.sessions, id)
		}
	}
}

func (m *SpendMeter) sessionLocked(sid domain.SessionID) *sessionSpend {
	s, ok := m.sessions[sid]
	if !ok {
		s = &sessionSpend{}
		m.sessions[sid] = s
	}
	s.lastActivity = time.Now()
	return s
}

// cost prices a call from the ledger. Cache reads are charged at the full input
// price: the ledger has no cached-input price yet (that is ADR-0131 D4), so the
// figure is an upper bound rather than a guess at a discount.
func (m *SpendMeter) cost(generatorID string, u TokenUsage) float64 {
	if m.prices == nil || generatorID == "" {
		return 0
	}
	in, out, ok := m.prices.Cost(strings.TrimPrefix(generatorID, "llm:"))
	if !ok {
		return 0
	}
	return CalculateCost(u, in, out)
}

// ── The Acquire-side door ────────────────────────────────────────────────────

// meteredGenerator is the SpendMeter's decorator on every generator the
// Provider hands out. It forwards every surface the inner stack has (text,
// native tools, streaming, the tool-capability report) because a surface it
// failed to forward would be a call path around the ledger.
type meteredGenerator struct {
	inner       domain.Generator
	meter       *SpendMeter
	purpose     string
	generatorID string
}

func (g *meteredGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	if err := g.meter.Admit(ctx, g.purpose, EstimateTokens(prompt)); err != nil {
		return "", err
	}
	cctx, capture := WithUsageCapture(ctx)
	out, err := g.inner.Generate(cctx, prompt)
	if err != nil {
		// A failed call is not booked: providers bill only what they served, and
		// the failure path has no usage to read.
		return out, err
	}
	g.meter.Record(ctx, g.purpose, g.generatorID, usageOrEstimate(capture, prompt, out))
	return out, nil
}

// NativeToolsEnabled forwards the inner generator's capability report.
func (g *meteredGenerator) NativeToolsEnabled() bool {
	r, ok := g.inner.(domain.ToolCallingReporter)
	return !ok || r.NativeToolsEnabled()
}

func (g *meteredGenerator) GenerateWithTools(
	ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition,
) (domain.ModelTurn, error) {
	tg, ok := g.inner.(domain.ToolCallingGenerator)
	if !ok {
		return domain.ModelTurn{}, fmt.Errorf("llm provider: %T does not implement native tool-calling", g.inner)
	}
	prompt := MessagesText(messages)
	if err := g.meter.Admit(ctx, g.purpose, EstimateTokens(prompt)); err != nil {
		return domain.ModelTurn{}, err
	}
	cctx, capture := WithUsageCapture(ctx)
	turn, err := tg.GenerateWithTools(cctx, messages, tools)
	if err != nil {
		return turn, err
	}
	g.meter.Record(ctx, g.purpose, g.generatorID, usageOrEstimate(capture, prompt, TurnText(turn)))
	return turn, nil
}

func (g *meteredGenerator) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	sg, ok := g.inner.(streamingInner)
	if !ok {
		return nil, fmt.Errorf("llm provider: metered %T does not implement streaming", g.inner)
	}
	if err := g.meter.Admit(ctx, g.purpose, EstimateTokens(prompt)); err != nil {
		return nil, err
	}
	in, err := sg.GenerateStream(ctx, prompt)
	if err != nil {
		return nil, err
	}
	out := make(chan domain.StreamChunk, 64)
	go func() {
		defer close(out)
		var text strings.Builder
		var usage StreamUsage
		for chunk := range in {
			text.WriteString(chunk.Text)
			usage.Observe(chunk)
			out <- chunk
		}
		// Booked when the stream is fully drained — the only point at which the
		// provider's usage chunk (often trailing the final text) has arrived.
		g.meter.Record(ctx, g.purpose, g.generatorID, ReconcileTokens(prompt, usage.Chunk(), text.String()))
	}()
	return out, nil
}

// usageOrEstimate returns the provider-reported usage, or the flagged len/4
// estimate when the provider reported none (ADR-0131 D2).
func usageOrEstimate(c *UsageCapture, prompt, response string) TokenUsage {
	if u, ok := c.Usage(); ok {
		return u
	}
	return EstimateUsage(prompt, response)
}

// MessagesText flattens a tool-calling conversation for the prompt estimate.
func MessagesText(messages []domain.ModelMessage) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString(m.Content)
		for _, tc := range m.ToolCalls {
			b.WriteString(tc.Name)
			b.Write(tc.Arguments)
		}
	}
	return b.String()
}

// TurnText is the billable text of a tool-calling turn: its prose plus the
// serialized arguments of every call, since a turn that is ALL tool calls has
// no prose and would otherwise estimate as free.
func TurnText(turn domain.ModelTurn) string {
	var b strings.Builder
	b.WriteString(turn.Text)
	for _, tc := range turn.ToolCalls {
		b.Write(tc.Arguments)
	}
	return b.String()
}

var (
	_ domain.Generator            = (*meteredGenerator)(nil)
	_ domain.ToolCallingGenerator = (*meteredGenerator)(nil)
	_ streamingInner              = (*meteredGenerator)(nil)
)
