// Package stepoutput is the live step-output stream (ADR-0139 amendment
// 2026-11-06, which supersedes ADR-0047 D12's token-level tier): ONE stream per
// step, keyed by the step's lease, with several producers and several outward
// channels.
//
// Producers:
//   - the gateway (EmitGateway): text deltas of a managed model call on either
//     lane, and the live final-answer argument of a native tool turn
//     (network.gatewayForwarder, Extractor);
//   - the agent (EmitAgent): an external agent that calls its own model pushes
//     its output over the agent-plane EmitStepOutput RPC.
//
// Channels are sinks fixed at construction (the operator feed) and per-session
// subscribers (the MCP endpoint's notifications/progress).
//
// Policy lives here, constructor-injected (AGENTS.md §2.3), so no producer can
// forget it:
//   - an agent that declares streams_output gets no gateway deltas — one step never
//     streams twice;
//   - a D11 post_model hook that MATCHES the step suppresses its gateway deltas,
//     fail-closed: such a hook sees only the COMPLETE reply (it may rewrite,
//     redact or refuse it), so a delta forwarded before it ran would leak exactly
//     what it exists to withhold. Matching is the hook's static scope
//     (HookSpec.Matches) against the step's agent and the gateway's purpose
//     label, agent_step — decidable before the reply exists. A hook with no scope
//     matches every step, so it still suppresses everything;
//   - per-chunk and per-step byte caps;
//   - a kernel sequence per lease across producers, and agent duplicates dropped.
//
// Delivery is synchronous and ordered (under the stream's lock), so sinks and
// subscribers MUST be prompt and non-blocking (§3.3): the feed spool drops for a
// slow subscriber, the MCP subscriber drops into a bounded buffer. No goroutine is
// started per chunk, or at all.
package stepoutput

import (
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// Sink receives every delivered chunk. It must not block.
type Sink func(domain.StepOutputChunk)

// Options configures a Stream.
type Options struct {
	Config config.StepOutputConfig
	// Hooks is the D11 chain the Provider runs. A post_model hook whose scope
	// matches the step suppresses its gateway deltas (fail-closed). nil = no hooks.
	Hooks *domain.HookChain
	// Leases resolves a lease to its binding (session, step, agent). nil ⇒ chunks
	// carry the lease only.
	Leases domain.LeaseResolver
	// StreamsOutput reports whether an agent declared streams_output (it pushes
	// its own output). nil ⇒ none does.
	StreamsOutput func(agentID string) bool
	// Sinks receive every chunk (the operator feed).
	Sinks []Sink
	Log   *slog.Logger
	// Now is the clock (tests). nil ⇒ time.Now.
	Now func() time.Time
}

// Lease state is bounded: EndLease (the gateway's lease-complete hook) removes an
// entry, and past maxTrackedLeases entries idle longer than leaseIdleTTL are
// pruned, so leases that expire without completing cannot grow the map.
const (
	maxTrackedLeases = 4096
	leaseIdleTTL     = 15 * time.Minute
)

// Stream is the live step-output stream. Safe for concurrent use.
type Stream struct {
	cfg           config.StepOutputConfig
	hooks         *domain.HookChain
	leases        domain.LeaseResolver
	streamsOutput func(string) bool
	sinks         []Sink
	log           *slog.Logger
	now           func() time.Time

	mu      sync.Mutex
	state   map[domain.LeaseID]*leaseState
	subs    map[domain.SessionID]map[uint64]Sink
	nextSub uint64
}

type leaseState struct {
	seq      uint64 // kernel sequence, across producers
	agentSeq uint64 // highest agent seq accepted
	bytes    int
	capped   bool // the per-step cap was hit (and said once)
	closed   bool // the agent sent its final chunk
	touched  time.Time
	binding  domain.LeaseBinding
	resolved bool
}

// New builds a Stream.
func New(o Options) *Stream {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Stream{
		cfg:           o.Config,
		hooks:         o.Hooks,
		leases:        o.Leases,
		streamsOutput: o.StreamsOutput,
		sinks:         append([]Sink(nil), o.Sinks...),
		log:           log,
		now:           now,
		state:         map[domain.LeaseID]*leaseState{},
		subs:          map[domain.SessionID]map[uint64]Sink{},
	}
}

// FinalAnswerTool and FinalAnswerArg name the native tool lane's deliverable
// (config, not code: the SDK's control vocabulary).
func (s *Stream) FinalAnswerTool() string { return s.cfg.FinalAnswerToolName() }

// FinalAnswerArg is the argument of FinalAnswerTool holding the deliverable.
func (s *Stream) FinalAnswerArg() string { return s.cfg.FinalAnswerArgName() }

// SuppressReason says why gateway deltas are not forwarded for a step under lease,
// or "" when they are. agentID is the caller's header id; the lease's bound agent
// is checked too (a post_model hook matching either suppresses).
func (s *Stream) SuppressReason(lease domain.LeaseID, agentID string) string {
	if s == nil {
		return "no stream"
	}
	if s.cfg.GatewayDeltasDisabled {
		return "execution.step_output.gateway_deltas_disabled"
	}
	bound := ""
	if b, ok := s.binding(lease); ok {
		bound = b.AgentID
	}
	if s.hooks.Has(domain.HookPostModel) {
		// The hook event's agent is the caller's principal (the header) and the
		// lease is bound to the same agent (the gateway refuses a foreign lease);
		// matching EITHER suppresses, so a disagreement can only fail closed.
		purpose := string(domain.PurposeAgentStep)
		if s.hooks.HasFor(domain.HookPostModel, agentID, purpose) ||
			(bound != "" && bound != agentID && s.hooks.HasFor(domain.HookPostModel, bound, purpose)) {
			return "a post_model hook matches this step (it sees only the complete reply)"
		}
	}
	if s.streamsOutput != nil {
		if bound != "" {
			agentID = bound
		}
		if agentID != "" && s.streamsOutput(agentID) {
			return "the agent declares streams_output"
		}
	}
	return ""
}

// GatewayForwarding reports whether gateway deltas are forwarded for the step.
func (s *Stream) GatewayForwarding(lease domain.LeaseID, agentID string) bool {
	return s.SuppressReason(lease, agentID) == ""
}

// EmitGateway delivers one gateway delta. The caller checked GatewayForwarding
// once for the call; empty data is ignored.
func (s *Stream) EmitGateway(lease domain.LeaseID, kind domain.StepOutputKind, data string) {
	if s == nil || data == "" || lease == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.leaseLocked(lease)
	if st.closed {
		return
	}
	s.deliverLocked(lease, st, kind, data, false, domain.StepOutputFromGateway)
}

// AgentChunk is one chunk an agent pushed (EmitStepOutput).
type AgentChunk struct {
	Seq   uint64
	Kind  domain.StepOutputKind
	Data  string
	Final bool
}

// AgentOutcome is what happened to one pushed chunk.
type AgentOutcome int

const (
	// AgentAccepted: delivered (possibly cut to the chunk cap).
	AgentAccepted AgentOutcome = iota
	// AgentDuplicate: seq not above the highest accepted one — dropped.
	AgentDuplicate
	// AgentOverCap: the step's byte cap was already reached — dropped.
	AgentOverCap
	// AgentClosed: the agent already sent its final chunk — dropped.
	AgentClosed
)

// EmitAgent delivers one agent-pushed chunk. The caller has already checked that
// the lease is live and the caller's own (the RPC refuses otherwise). seq must be
// strictly increasing per lease; a chunk at or below the highest accepted seq is a
// duplicate (a retried stream) and is dropped.
func (s *Stream) EmitAgent(lease domain.LeaseID, c AgentChunk) AgentOutcome {
	if !c.Kind.Valid() {
		c.Kind = domain.StepOutputText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.leaseLocked(lease)
	switch {
	case st.closed:
		return AgentClosed
	case c.Seq <= st.agentSeq:
		return AgentDuplicate
	}
	st.agentSeq = c.Seq
	if c.Final {
		st.closed = true
	}
	if st.capped && !c.Final {
		return AgentOverCap
	}
	if !s.deliverLocked(lease, st, c.Kind, c.Data, c.Final, domain.StepOutputFromAgent) {
		return AgentOverCap
	}
	return AgentAccepted
}

// EndLease forgets a lease's state. Wired to the gateway's lease-complete hook.
func (s *Stream) EndLease(lease domain.LeaseID) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.state, lease)
	s.mu.Unlock()
}

// SubscribeSession delivers every chunk of every lease bound to the task session
// sid to fn until the returned cancel runs. fn must not block.
func (s *Stream) SubscribeSession(sid domain.SessionID, fn Sink) (cancel func()) {
	if s == nil || sid == "" || fn == nil {
		return func() {}
	}
	s.mu.Lock()
	id := s.nextSub
	s.nextSub++
	if s.subs[sid] == nil {
		s.subs[sid] = map[uint64]Sink{}
	}
	s.subs[sid][id] = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if m := s.subs[sid]; m != nil {
			delete(m, id)
			if len(m) == 0 {
				delete(s.subs, sid)
			}
		}
	}
}

// Tracked reports how many leases hold state (tests, the leak bound).
func (s *Stream) Tracked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.state)
}

func (s *Stream) binding(lease domain.LeaseID) (domain.LeaseBinding, bool) {
	if s.leases == nil || lease == "" {
		return domain.LeaseBinding{}, false
	}
	return s.leases.ResolveLease(lease)
}

// leaseLocked returns (creating) the lease's state. The binding is resolved once,
// on first use: a lease's binding is fixed at dispatch, before its first call.
func (s *Stream) leaseLocked(lease domain.LeaseID) *leaseState {
	st := s.state[lease]
	if st == nil {
		if len(s.state) >= maxTrackedLeases {
			s.pruneLocked()
		}
		st = &leaseState{binding: domain.LeaseBinding{StepIndex: -1}}
		if b, ok := s.binding(lease); ok {
			st.binding, st.resolved = b, true
		}
		s.state[lease] = st
	}
	st.touched = s.now()
	return st
}

// pruneLocked drops idle lease state, then — if every entry is fresh — the oldest.
func (s *Stream) pruneLocked() {
	cutoff := s.now().Add(-leaseIdleTTL)
	for id, st := range s.state {
		if st.touched.Before(cutoff) {
			delete(s.state, id)
		}
	}
	for len(s.state) >= maxTrackedLeases {
		var oldest domain.LeaseID
		var at time.Time
		for id, st := range s.state {
			if oldest == "" || st.touched.Before(at) {
				oldest, at = id, st.touched
			}
		}
		delete(s.state, oldest)
	}
}

// deliverLocked applies the caps, stamps the chunk and hands it to every sink and
// session subscriber. It reports false when the step cap left nothing to send.
func (s *Stream) deliverLocked(lease domain.LeaseID, st *leaseState, kind domain.StepOutputKind, data string, final bool, src domain.StepOutputSource) bool {
	if st.capped {
		if final {
			// The producer's final marker still goes out, without data, so a
			// consumer waiting for it is not left hanging.
			s.sendLocked(lease, st, kind, "", true, src)
		}
		return false
	}
	data = truncateUTF8(data, s.cfg.ChunkBytes())
	if room := s.cfg.StepBytes() - st.bytes; len(data) > room {
		data = truncateUTF8(data, room)
		st.capped = true
	}
	st.bytes += len(data)
	sent := false
	if data != "" || final {
		s.sendLocked(lease, st, kind, data, final, src)
		sent = true
	}
	if st.capped {
		s.log.Info("step output: per-step cap reached; further live output for this step is dropped "+
			"(the step's final output is unaffected)", "lease", lease, "max_step_bytes", s.cfg.StepBytes())
		s.sendLocked(lease, st, domain.StepOutputStatus,
			"live output truncated: execution.step_output.max_step_bytes reached", false, domain.StepOutputFromGateway)
	}
	return sent
}

func (s *Stream) sendLocked(lease domain.LeaseID, st *leaseState, kind domain.StepOutputKind, data string, final bool, src domain.StepOutputSource) {
	st.seq++
	c := domain.StepOutputChunk{
		LeaseID:   lease,
		SessionID: st.binding.SessionID,
		StepIndex: st.binding.StepIndex,
		AgentID:   st.binding.AgentID,
		Seq:       st.seq,
		Kind:      kind,
		Data:      data,
		Final:     final,
		Source:    src,
	}
	for _, sink := range s.sinks {
		sink(c)
	}
	if c.SessionID != "" {
		for _, fn := range s.subs[c.SessionID] {
			fn(c)
		}
	}
}

// truncateUTF8 cuts s to at most n bytes at a rune boundary.
func truncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
