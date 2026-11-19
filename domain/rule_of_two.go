package domain

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// The Rule-of-Two gate (ENG-531, owner decision 2026-11-19: build directly, ADR
// after; T-20261104-no-content-provenance-reaches-tool).
//
// Meta's "Agents Rule of Two": a session may hold at most two of
//
//	[A] it has consumed untrusted content (a recalled ingested chunk, a web page,
//	    a tool's answer — text an attacker can write),
//	[B] it holds sensitive access (private data, sensitive systems),
//	[C] it is about to change state or communicate outside (write / egress).
//
// When a tool call would complete all three, the reference monitor
// (ToolExecutor.Execute) narrows the call to ask: it goes to the person through
// the ONE existing approval path (ApprovalController, its intent and recovery),
// with a notice naming what tainted the session and why it counts as sensitive.
// With no approval controller the call is denied (fail closed).
//
// What counts as untrusted (source classes, source_class.go), as sensitive
// (classification tags) and as a gated call (effect classes) is configuration —
// execution.tools.rule_of_two — never a Go branch on a tool, agent or source name.
//
// Where the taint lives: a ProvenanceTracker keyed by the identity of the lease
// binding (TaintScope: the task session, the conversation, the run; the lease
// itself only when it is bound to none of them). Not a field mutated on one
// lease: BindLease replaces a binding whole at dispatch, and one session spans
// many step and turn leases — a flag on one lease would not reach the next
// step's lease, while content consumed in step 1 reaches step 2 through its
// output. Every key of the scope is marked, and a check reads every key, so a
// chat-delegated plan step (session + the ordering conversation) and the
// conversation's next turn see each other's taint.

// TaintScope is the set of identities a session's provenance is tracked under,
// read from the kernel-held lease binding (never from a header a caller sets).
// The zero scope means "the kernel cannot say whose session this is"; the gate
// treats it as tainted (fail closed).
type TaintScope struct {
	keys []string
}

// NewTaintScope builds the scope of a call from its lease and the kernel-resolved
// facts about it: the binding (when the lease is known), the task session and the
// conversation the kernel resolved onto ctx (a plan step's conversation is the
// second hop through its session, ADR-0084 D2). The lease itself is a key only
// when nothing else identifies the session — a bare lease is still one session.
func NewTaintScope(lease LeaseID, b LeaseBinding, session SessionID, conversation string) TaintScope {
	seen := map[string]bool{}
	var keys []string
	add := func(prefix, v string) {
		if v = strings.TrimSpace(v); v == "" {
			return
		}
		k := prefix + v
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	add("session:", string(b.SessionID))
	add("session:", string(session))
	add("conversation:", b.ConversationID)
	add("conversation:", conversation)
	add("run:", string(b.RunID))
	if len(keys) == 0 {
		add("lease:", string(lease))
	}
	sort.Strings(keys)
	return TaintScope{keys: keys}
}

// IsZero reports whether the scope names no session at all.
func (s TaintScope) IsZero() bool { return len(s.keys) == 0 }

// Keys returns the scope's identities (sorted; a copy).
func (s TaintScope) Keys() []string { return append([]string(nil), s.keys...) }

type taintScopeKey struct{}

// WithTaintScope carries a call's taint scope on ctx. Set by the transport
// (ExecuteTool, QueryMemory) from the lease binding.
func WithTaintScope(ctx context.Context, s TaintScope) context.Context {
	return context.WithValue(ctx, taintScopeKey{}, s)
}

// TaintScopeFromContext returns the scope WithTaintScope set (zero if none).
func TaintScopeFromContext(ctx context.Context) TaintScope {
	s, _ := ctx.Value(taintScopeKey{}).(TaintScope)
	return s
}

// TaintState is what a scope has consumed: every source class (trusted or not —
// the policy decides which count) and the sensitive tags of what it read.
type TaintState struct {
	Sources       []SourceClass
	SensitiveTags []string
}

type taintEntry struct {
	sources   map[SourceClass]bool
	sensitive map[string]bool
	seen      time.Time
}

// ProvenanceTracker records, per taint-scope key, what content a session has
// consumed. In memory: a kernel restart forgets it (the residual the ADR names;
// a resumed session starts untainted until it consumes content again). An entry
// lives ttl after its last mark.
type ProvenanceTracker struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]*taintEntry
	marks   int
}

// NewProvenanceTracker builds a tracker. ttl <= 0 ⇒ 24h; now nil ⇒ time.Now.
func NewProvenanceTracker(ttl time.Duration, now func() time.Time) *ProvenanceTracker {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if now == nil {
		now = time.Now
	}
	return &ProvenanceTracker{ttl: ttl, now: now, entries: map[string]*taintEntry{}}
}

// sweepEvery is how many marks pass between expiry sweeps of the whole map.
const sweepEvery = 256

// Mark records that the scope consumed content of class (and, when non-empty,
// read records carrying sensitiveTags). A zero scope or an empty class with no
// tags records nothing.
func (t *ProvenanceTracker) Mark(scope TaintScope, class SourceClass, sensitiveTags []string) {
	if t == nil || scope.IsZero() || (class == "" && len(sensitiveTags) == 0) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, k := range scope.keys {
		e := t.entries[k]
		if e == nil || now.Sub(e.seen) > t.ttl {
			e = &taintEntry{sources: map[SourceClass]bool{}, sensitive: map[string]bool{}}
			t.entries[k] = e
		}
		if class != "" {
			e.sources[class] = true
		}
		for _, tag := range sensitiveTags {
			e.sensitive[tag] = true
		}
		e.seen = now
	}
	t.marks++
	if t.marks%sweepEvery == 0 {
		for k, e := range t.entries {
			if now.Sub(e.seen) > t.ttl {
				delete(t.entries, k)
			}
		}
	}
}

// State returns the union of what every key of the scope consumed.
func (t *ProvenanceTracker) State(scope TaintScope) TaintState {
	if t == nil || scope.IsZero() {
		return TaintState{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	sources := map[SourceClass]bool{}
	sensitive := map[string]bool{}
	for _, k := range scope.keys {
		e := t.entries[k]
		if e == nil || now.Sub(e.seen) > t.ttl {
			continue
		}
		for c := range e.sources {
			sources[c] = true
		}
		for tag := range e.sensitive {
			sensitive[tag] = true
		}
	}
	var st TaintState
	for _, c := range AllSourceClasses {
		if sources[c] {
			st.Sources = append(st.Sources, c)
		}
	}
	for tag := range sensitive {
		st.SensitiveTags = append(st.SensitiveTags, tag)
	}
	sort.Strings(st.SensitiveTags)
	return st
}

// RuleOfTwoPolicy is the gate's configuration (execution.tools.rule_of_two).
type RuleOfTwoPolicy struct {
	// UntrustedSources are the source classes whose consumption taints a session.
	// Required: an empty set would make the gate inert while it reads as on.
	UntrustedSources []SourceClass
	// SensitiveTags are the classification tags that make access sensitive: a
	// session that read a record carrying one, or whose agent holds a grant on a
	// tool classified with one. EMPTY ⇒ every session holds sensitive access
	// (fail closed: with no sensitivity vocabulary the kernel cannot show that a
	// session holds none).
	SensitiveTags []string
	// GatedEffects are the effect classes that make a call state-changing or
	// outbound. A call whose ceiling effects (CeilingEffects: inferred egress
	// counts as send) include one is gated; a tool with no effects is gated.
	// Required.
	GatedEffects []ToolEffect
}

// ErrRuleOfTwoPolicy reports an invalid gate configuration.
var ErrRuleOfTwoPolicy = errors.New("rule of two")

// RuleOfTwoGate is the executor control (ToolExecutorControls.RuleOfTwo; nil ⇒
// off). It owns the tracker the transport reports consumed content to.
type RuleOfTwoGate struct {
	tracker   *ProvenanceTracker
	untrusted map[SourceClass]bool
	sensitive map[string]bool
	gated     []ToolEffect
}

// NewRuleOfTwoGate validates the policy and builds the gate over tracker.
func NewRuleOfTwoGate(p RuleOfTwoPolicy, tracker *ProvenanceTracker) (*RuleOfTwoGate, error) {
	if tracker == nil {
		return nil, fmt.Errorf("%w: a provenance tracker is required", ErrRuleOfTwoPolicy)
	}
	if len(p.UntrustedSources) == 0 {
		return nil, fmt.Errorf("%w: untrusted_sources must name at least one source class", ErrRuleOfTwoPolicy)
	}
	if len(p.GatedEffects) == 0 {
		return nil, fmt.Errorf("%w: gated_effects must name at least one effect class", ErrRuleOfTwoPolicy)
	}
	g := &RuleOfTwoGate{tracker: tracker, untrusted: map[SourceClass]bool{}, sensitive: map[string]bool{}}
	for _, c := range p.UntrustedSources {
		if !ValidSourceClass(c) {
			return nil, fmt.Errorf("%w: unknown source class %q", ErrRuleOfTwoPolicy, c)
		}
		g.untrusted[c] = true
	}
	for _, tag := range p.SensitiveTags {
		if tag = strings.TrimSpace(tag); tag != "" {
			g.sensitive[tag] = true
		}
	}
	for _, e := range p.GatedEffects {
		if !ValidToolEffect(e) {
			return nil, fmt.Errorf("%w: unknown effect class %q", ErrRuleOfTwoPolicy, e)
		}
	}
	g.gated = ExpandEffects(p.GatedEffects)
	return g, nil
}

// Observe records that the scope consumed content of class, from records or a
// tool carrying tags (only the sensitive ones are kept).
func (g *RuleOfTwoGate) Observe(scope TaintScope, class SourceClass, tags []string) {
	if g == nil {
		return
	}
	g.tracker.Mark(scope, class, g.sensitiveOf(tags))
}

// State is what the scope has consumed (diagnostics and tests).
func (g *RuleOfTwoGate) State(scope TaintScope) TaintState {
	if g == nil {
		return TaintState{}
	}
	return g.tracker.State(scope)
}

func (g *RuleOfTwoGate) sensitiveOf(tags []string) []string {
	if len(g.sensitive) == 0 || len(tags) == 0 {
		return nil
	}
	var out []string
	for _, t := range tags {
		if g.sensitive[t] {
			out = append(out, t)
		}
	}
	return out
}

// gatedEffects returns the gated effects a call of t exercises (nil ⇒ not gated).
func (g *RuleOfTwoGate) gatedEffects(t SystemTool) []ToolEffect {
	effects := CeilingEffects(t)
	if len(effects) == 0 {
		return g.gated // bypassed registration: treat as everything (fail closed)
	}
	var out []ToolEffect
	for _, e := range effects {
		for _, x := range g.gated {
			if EffectCovers(x, e) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// RuleOfTwoHit is why a call completed all three legs.
type RuleOfTwoHit struct {
	// Sources are the untrusted source classes the session consumed.
	Sources []SourceClass
	// UnknownProvenance: the call carried no taint scope, so the kernel cannot
	// show the session consumed nothing untrusted (fail closed).
	UnknownProvenance bool
	// SensitiveTags are the sensitive tags of what the session read or of the
	// tools its agent holds grants on.
	SensitiveTags []string
	// AllSensitive: no sensitivity vocabulary is configured, so every session
	// counts as holding sensitive access.
	AllSensitive bool
	// SensitiveUnknown: the agent's grants could not be read (fail closed).
	SensitiveUnknown bool
	// Effects are the gated effect classes the call exercises.
	Effects []ToolEffect
}

// Check decides whether a call of tool in scope completes the Rule of Two.
// grantTags returns the classification tags of the tools the calling agent holds
// grants on (and an error when they cannot be read); it is asked only when the
// other two legs already hold and a sensitivity vocabulary is configured.
func (g *RuleOfTwoGate) Check(scope TaintScope, tool SystemTool, grantTags func() ([]string, error)) (RuleOfTwoHit, bool) {
	if g == nil {
		return RuleOfTwoHit{}, false
	}
	var hit RuleOfTwoHit
	// [C] state change or outbound.
	hit.Effects = g.gatedEffects(tool)
	if len(hit.Effects) == 0 {
		return RuleOfTwoHit{}, false
	}
	// [A] untrusted content consumed.
	st := g.tracker.State(scope)
	if scope.IsZero() {
		hit.UnknownProvenance = true
	}
	for _, c := range st.Sources {
		if g.untrusted[c] {
			hit.Sources = append(hit.Sources, c)
		}
	}
	if !hit.UnknownProvenance && len(hit.Sources) == 0 {
		return RuleOfTwoHit{}, false
	}
	// [B] sensitive access.
	switch {
	case len(g.sensitive) == 0:
		hit.AllSensitive = true
	default:
		set := map[string]bool{}
		for _, t := range st.SensitiveTags {
			set[t] = true
		}
		for _, t := range g.sensitiveOf(tool.AuthzTags()) {
			set[t] = true
		}
		if len(set) == 0 && grantTags != nil {
			tags, err := grantTags()
			if err != nil {
				hit.SensitiveUnknown = true
			}
			for _, t := range g.sensitiveOf(tags) {
				set[t] = true
			}
		}
		for t := range set {
			hit.SensitiveTags = append(hit.SensitiveTags, t)
		}
		sort.Strings(hit.SensitiveTags)
		if len(hit.SensitiveTags) == 0 && !hit.SensitiveUnknown {
			return RuleOfTwoHit{}, false
		}
	}
	return hit, true
}

// RenderRuleOfTwoNotice is the approver's notice for a gated call: what tainted
// the session, why its access counts as sensitive, and what the call would do.
func RenderRuleOfTwoNotice(hit RuleOfTwoHit) string {
	var src string
	switch {
	case len(hit.Sources) > 0:
		names := make([]string, len(hit.Sources))
		for i, c := range hit.Sources {
			names[i] = string(c)
		}
		src = "this session has read untrusted content (" + strings.Join(names, ", ") + ")"
	default:
		src = "the kernel cannot tell what content this session has read"
	}
	var sens string
	switch {
	case hit.AllSensitive:
		sens = "it can reach private data"
	case hit.SensitiveUnknown && len(hit.SensitiveTags) == 0:
		sens = "its access could not be checked"
	default:
		sens = "it can reach sensitive data (" + strings.Join(hit.SensitiveTags, ", ") + ")"
	}
	effects := make([]string, len(hit.Effects))
	for i, e := range hit.Effects {
		effects[i] = string(e)
	}
	return "Rule of Two: " + src + ", " + sens + ", and this call would " +
		strings.Join(effects, "/") + ". Text the agent read may be steering it — check the call is what was asked for."
}

// RuleOfTwoDenyReason is the refusal when a gated call cannot be approved
// because no approval controller is wired.
func RuleOfTwoDenyReason(hit RuleOfTwoHit) string {
	return "approval required but unavailable: " + RenderRuleOfTwoNotice(hit)
}

// KernelToolSource is implemented by a kernel tool whose result's provenance is
// not plain tool content (ToolExecutor reports a kernel tool's result to the
// Rule-of-Two gate under this class). "" means the result carries no content
// beyond what the caller's own scope already consumed (a calculation over its
// own ledger rows). A kernel tool that does not implement it reports SourceTool
// (fail closed).
type KernelToolSource interface {
	ResultSource() SourceClass
}
