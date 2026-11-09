package executer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"

	"github.com/cambrian-sh/core/domain"
)

// Step cache guards (ADR-0145 D9, T-20261108-step-cache-key-omits-principal-scope-and).
//
// The ADR-0026 key (step_cache_key.go) names WHAT a step computed: subject, query,
// upstream outputs, prior-plan results and the date. It did not name WHOSE result
// it is or WHAT the step does, and a reproduction (step_cache_isolation_test.go)
// showed both holes: a dependent step of principal A was served to principal B when
// the upstream output was identical (shared data), and a dependent write/move/send
// step was replayed from cache so its tools never ran.
//
// Two guards close them:
//
//  1. Identity. The key is salted with every kernel-held fact that decides what a
//     step may read: the session the run belongs to, the principal, the run
//     principal, the conversation owner, the act-for actor, and hashes of the read
//     predicate and the session-isolation predicate on ctx. The session is included
//     because a step agent's reads are narrowed per lease (the conversation owner and
//     caller_scope are resolved from the session, not from the executor's ctx), so a
//     context principal alone does not pin what the step saw. Reuse is therefore
//     within one session: retries, replans and resumed runs. A run that carries none
//     of these facts caches nothing (fail closed).
//
//  2. Effects. Only steps that observe are served or stored: a thought step (an
//     in-kernel model call with no tools), or an action step whose declared
//     required_effects are all read-only vocabulary terms (an empty list is the
//     planner saying "no effect"). A step that declares nothing (nil, a legacy or
//     resumed plan), an unknown effect, or any write/move/delete/execute/egress/
//     send/spend/admin effect always runs.

// stepCacheIdentity returns the identity salt for the step cache key, and false
// when nothing identifies whose result this is (then nothing is cached).
func stepCacheIdentity(ctx context.Context, session domain.SessionID) (string, bool) {
	p := domain.PrincipalFromContext(ctx)
	run := domain.RunPrincipalFromContext(ctx)
	owner := domain.ConversationOwnerFromContext(ctx)
	if session == "" && p.ID == "" && run == "" && owner == "" {
		return "", false
	}
	h := sha256.New()
	field := func(name, v string) {
		io.WriteString(h, name+"\x00"+v+"\x00") //nolint:errcheck
	}
	field("session", string(session))
	field("principal", string(p.Kind)+":"+p.ID)
	field("run_principal", run)
	field("conversation_owner", owner)
	field("act_for", domain.ActForActorFromContext(ctx))
	if scope, ok := domain.ScopeFromContext(ctx); ok {
		field("scope", jsonDigest(scope))
	} else {
		field("scope", "absent")
	}
	if iso, ok := domain.IsolationFromContext(ctx); ok {
		field("isolation", jsonDigest(iso))
	} else {
		field("isolation", "absent")
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// jsonDigest hashes v's JSON form. A value that cannot be marshalled hashes to a
// constant that never equals a real digest, so it still separates from "absent".
func jsonDigest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "unmarshalable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// stepCacheable reports whether a step's result may be served from or stored in
// the step cache: thought steps, and action steps whose declared effects are all
// read-only in vocab. vocab nil ⇒ no action step is cacheable.
func stepCacheable(step domain.Step, vocab *domain.StepEffectVocabulary) bool {
	if step.IsThought {
		return true
	}
	if step.RequiredEffects == nil {
		return false
	}
	for _, e := range step.RequiredEffects {
		if !vocab.ReadOnly(e) {
			return false
		}
	}
	return true
}

// guardedStepCacheKey is the full step cache key, or "" when the step must not be
// served from or stored in the cache.
func (d *DAGExecutor) guardedStepCacheKey(ctx context.Context, plan *domain.ExecutionPlan, planID string, stepIndex int, dispatched domain.Step, depResults map[string]string, today string) string {
	if !stepCacheable(plan.Steps[stepIndex], d.EffectVocabulary) {
		return ""
	}
	identity, ok := stepCacheIdentity(ctx, d.CurrentSessionID)
	if !ok {
		return ""
	}
	key := dayScopedCacheKey(stepCacheKey(plan.Subject, planID, dispatched, depResults), today)
	h := sha256.New()
	io.WriteString(h, key)          //nolint:errcheck
	io.WriteString(h, "|identity|") //nolint:errcheck
	io.WriteString(h, identity)     //nolint:errcheck
	return hex.EncodeToString(h.Sum(nil))
}
