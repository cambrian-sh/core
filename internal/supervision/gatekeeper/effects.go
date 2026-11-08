package gatekeeper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// Effect-checked dispatch (T-20261106-kernel-does-not-check-that-a-declared, owner
// option B): the planner declares the effects an action step needs, and L1 admits
// only agents whose TOOLS produce them. See domain/step_effects.go for the model.

// AgentToolLister lists the tools an agent can actually call: its grants (a profile
// agent's allowlist expanded over the live registry, re-expanded when MCP servers
// change), inside every ceiling, plus the task beneficiary's contributed tools.
// *domain.ToolExecutor.AvailableTools is the production implementation — the same
// menu the executor authorizes against, so routing and enforcement cannot disagree.
type AgentToolLister interface {
	AvailableTools(ctx context.Context, agentID string) []domain.SystemTool
}

// ToolEffectSource derives an agent's effects from its callable tools at dispatch
// time. It is computed per call, not at boot, because grants and integrations
// change live (an integration installed from the console adds tools, a profile
// re-sync drops them); the derivation is cached by the tool-set SIGNATURE (each
// tool's name, effect classes and policy flags), so a changed grant set or a
// re-registered tool with a new policy is a different key and is never served a
// stale answer.
type ToolEffectSource struct {
	vocab *domain.StepEffectVocabulary
	tools AgentToolLister

	mu    sync.Mutex
	cache map[string][]string // signature → derived effects
}

// maxEffectCacheEntries bounds the signature cache; on overflow it is reset. Tool
// sets are few (one per distinct grant set), so this is a leak guard, not a tuner.
const maxEffectCacheEntries = 1024

// NewToolEffectSource builds the source. A nil vocabulary or lister is allowed and
// fails closed: every agent then has no effects, so every effect-declaring step is
// refused rather than routed unchecked.
func NewToolEffectSource(vocab *domain.StepEffectVocabulary, tools AgentToolLister) *ToolEffectSource {
	return &ToolEffectSource{vocab: vocab, tools: tools, cache: map[string][]string{}}
}

// Vocabulary implements domain.AgentEffectSource.
func (s *ToolEffectSource) Vocabulary() *domain.StepEffectVocabulary {
	if s == nil {
		return nil
	}
	return s.vocab
}

// AgentEffects implements domain.AgentEffectSource.
func (s *ToolEffectSource) AgentEffects(ctx context.Context, agentID string) []string {
	if s == nil || s.vocab == nil || s.tools == nil || agentID == "" {
		return nil
	}
	tools := s.tools.AvailableTools(ctx, agentID)
	sig := toolSetSignature(tools)
	s.mu.Lock()
	if got, ok := s.cache[sig]; ok {
		s.mu.Unlock()
		return append([]string(nil), got...)
	}
	s.mu.Unlock()
	derived := s.vocab.DeriveEffects(tools)
	s.mu.Lock()
	if len(s.cache) >= maxEffectCacheEntries {
		s.cache = map[string][]string{}
	}
	s.cache[sig] = derived
	s.mu.Unlock()
	return append([]string(nil), derived...)
}

// toolSetSignature identifies a tool set by everything the derivation reads.
func toolSetSignature(tools []domain.SystemTool) string {
	lines := make([]string, 0, len(tools))
	for _, t := range tools {
		effs := make([]string, len(t.Effects))
		for i, e := range t.Effects {
			effs[i] = string(e)
		}
		sort.Strings(effs)
		flags := ""
		if t.EffectsInferred {
			flags += "i"
		}
		if t.EffectsDefaulted {
			flags += "d"
		}
		lines = append(lines, t.Name+"\x1f"+strings.Join(effs, ",")+"\x1f"+flags)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\x1e")))
	return hex.EncodeToString(sum[:])
}

// effectGate runs the effect check for one dispatch. It is created per
// FindCandidates call and memoises each agent's effects for that call.
type effectGate struct {
	task   *domain.DispatchTask
	src    domain.AgentEffectSource
	vocab  *domain.StepEffectVocabulary
	result *domain.EffectCheckResult
}

// newEffectGate returns nil when the step declares no required effects (nil or
// empty), after recording the unverified / none outcome; otherwise a gate that
// every candidate must pass.
func newEffectGate(ctx context.Context, task *domain.DispatchTask, src domain.AgentEffectSource) *effectGate {
	if task == nil {
		return nil
	}
	if len(task.RequiredEffects) == 0 {
		outcome := domain.EffectCheckNone
		if task.RequiredEffects == nil {
			outcome = domain.EffectCheckUnverified
		}
		task.EffectCheck = &domain.EffectCheckResult{Outcome: outcome}
		if outcome == domain.EffectCheckUnverified {
			// Back-compat: a legacy plan (old cache entry, non-contract planner) carries
			// no effect contract. It dispatches as before, and says so.
			slog.InfoContext(ctx, "Gatekeeper: effect check unverified: the step declares no required_effects (legacy plan); dispatching without an effect check",
				"task_id", task.ID)
		}
		return nil
	}
	required := make([]string, 0, len(task.RequiredEffects))
	seen := map[string]bool{}
	for _, e := range task.RequiredEffects {
		n := domain.NormalizeStepEffect(e)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		required = append(required, n)
	}
	var vocab *domain.StepEffectVocabulary
	if src != nil {
		vocab = src.Vocabulary()
	}
	g := &effectGate{
		task:  task,
		src:   src,
		vocab: vocab,
		result: &domain.EffectCheckResult{
			Required:     required,
			AgentEffects: map[string][]string{},
			Unknown:      vocab.UnknownEffects(required),
		},
	}
	if len(required) == 0 { // only blanks: nothing real was declared
		g.result.Outcome = domain.EffectCheckNone
		task.EffectCheck = g.result
		return nil
	}
	task.EffectCheck = g.result
	return g
}

// admits reports whether agentID holds every required effect, recording its set.
// No source wired, an unknown effect, or a missing effect all refuse (fail closed).
func (g *effectGate) admits(ctx context.Context, agentID string) (bool, []string) {
	var have []string
	if g.src != nil {
		have = g.src.AgentEffects(ctx, agentID)
	}
	g.result.AgentEffects[agentID] = have
	missing := g.vocab.MissingEffects(g.result.Required, have)
	if len(missing) == 0 {
		g.result.Eligible = append(g.result.Eligible, agentID)
		return true, nil
	}
	return false, missing
}

// refusal builds the NoEffectMatchError for a step no candidate passed.
func (g *effectGate) refusal() *domain.NoEffectMatchError {
	g.result.Outcome = domain.EffectCheckRefused
	// Missing = the required effects no considered agent covers; when every effect
	// is covered by SOME agent but no single agent covers all, the whole set is missing.
	covered := map[string]bool{}
	for _, effs := range g.result.AgentEffects {
		for _, e := range effs {
			covered[e] = true
		}
	}
	var missing []string
	for _, r := range g.result.Required {
		if !g.vocab.Known(r) || !covered[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		missing = append(missing, g.result.Required...)
	}
	return &domain.NoEffectMatchError{TaskID: g.task.ID, Missing: missing, Check: g.result}
}

// log records the check for the step (observability, item 6 of the owner design):
// required, each agent's derived effects, the eligible set and the outcome.
func (g *effectGate) log(ctx context.Context) {
	if g == nil {
		return
	}
	if g.result.Outcome == "" {
		if len(g.result.Eligible) == 0 {
			// FindCandidates returned before the check finished (an earlier refusal,
			// e.g. no capability match): nothing was admitted on effects.
			g.result.Outcome = "not_reached"
		} else {
			g.result.Outcome = domain.EffectCheckEligible
		}
	}
	level := slog.LevelInfo
	if g.result.Outcome == domain.EffectCheckRefused {
		level = slog.LevelWarn
	}
	slog.Log(ctx, level, "Gatekeeper: effect check",
		"task_id", g.task.ID, "outcome", g.result.Outcome,
		"required", g.result.Required, "eligible", g.result.Eligible,
		"unknown", g.result.Unknown, "pin_refused", g.result.PinRefused,
		"agents", g.result.Summary())
}
