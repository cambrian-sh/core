package domain

import (
	"context"
	"encoding/json"
)

// The ToolExecutor side of ADR-0139 D11: the authorization pass a hook-rewritten
// call must clear again, and the event a tool hook sees.

// authorize runs the arg-dependent half of the reference monitor for one call:
// grant, CID hydration, Regime 2 resource policy, Regime 1 data scope and Regime 3
// effects. Execute calls it once for the call as made and AGAIN for a call a
// pre_tool hook rewrote — one function, so the rewritten call cannot skip a check
// the original passed. req.ArgsJSON is hydrated in place.
func (e *ToolExecutor) authorize(ctx context.Context, req *ToolCallRequest, tool SystemTool) (ToolGrant, string, bool) {
	// Grant (fail-closed on unknown principal / no grant). A2.2: an operator
	// ScopeSystem execution (req.System) bypasses the per-agent grant with an
	// allow-all policy — the operator is above the scope plane (D13).
	var grant ToolGrant
	if req.System {
		grant = ToolGrant{Tool: req.ToolName, Policy: ToolResourcePolicy{AllowAll: true}}
	} else {
		g, granted := e.grantFor(ctx, req.AgentID, req.ToolName, string(req.SessionTokenID))
		if !granted {
			return ToolGrant{}, "tool not granted to agent", false
		}
		grant = g
	}

	// ADR-0048 #3: resolve {"$cid":"…"} reference args to their stored content before
	// policy + dispatch, so the tool receives the real bytes. Done after the grant
	// check (no CAS reads for ungranted calls); the caller's argHash stays keyed on
	// the logical reference action.
	req.ArgsJSON = e.hydrateCIDArgs(ctx, req.ArgsJSON)

	// Regime 2 — system-resource policy on the tool's declared resource args.
	if reason, ok := checkResourcePolicy(tool, grant.Policy, req.ArgsJSON); !ok {
		return ToolGrant{}, "resource policy: " + reason, false
	}

	// Regime 1 — data-store scope, when the tool touches tagged stores. A2.2: an
	// operator ScopeSystem execution reads/writes at ScopeSystem (D13), so it is
	// not scope-gated here.
	if !req.System && (len(tool.DataReadKinds) > 0 || len(tool.DataWriteKinds) > 0) {
		if !e.scopeAllows(ctx, req.AgentID, tool) {
			return ToolGrant{}, "scope", false
		}
	}

	// Regime 3 — EFFECT classes (ADR-0086). The tag check above asks what the tool
	// is ABOUT; this asks what the invocation DOES. Both must pass, and this one
	// applies to every tool, not only the tagged-store ones — "no tool may
	// transmit outside this network" has to hold for a tool that touches no store.
	//
	// An operator ScopeSystem execution carries its own authority (A2.2) and is not
	// effect-gated; the resource-arg policy and process confinement still apply.
	if !req.System {
		if dec := e.effectDecision(ctx, *req, tool); !dec.Allowed {
			return ToolGrant{}, "effect not permitted: " + dec.Detail, false
		}
	}
	return grant, "", true
}

// toolHookEvent builds the immutable event a tool hook sees.
func (e *ToolExecutor) toolHookEvent(ctx context.Context, req ToolCallRequest, tool SystemTool, p HookPoint, verdict HookVerdict, result []byte) HookEvent {
	effects := make([]string, 0, len(tool.Effects))
	for _, ef := range tool.Effects {
		effects = append(effects, string(ef))
	}
	ev := HookEvent{
		Point:      p,
		RunID:      taskIDOf(ctx, req),
		LeaseID:    string(req.SessionTokenID),
		TaskID:     req.TaskID,
		AgentID:    req.AgentID,
		Principals: HookPrincipalChain(ctx, req.AgentID),
		Tool:       req.ToolName,
		Args:       json.RawMessage(req.ArgsJSON),
		Effects:    effects,
		Dangerous:  tool.Dangerous,
		Verdict:    verdict,
	}
	if result != nil {
		ev.Result = json.RawMessage(result)
	}
	return ev
}

// HookPrincipalChain renders the principal chain a hook event carries: the
// authenticated caller (sub), the acting agent (act) when it differs, then the
// task beneficiary when one is set. Duplicates are dropped, order is kept.
func HookPrincipalChain(ctx context.Context, agentID string) []string {
	var chain []string
	add := func(p PrincipalRef) {
		if p.IsZero() {
			return
		}
		s := p.String()
		for _, c := range chain {
			if c == s {
				return
			}
		}
		chain = append(chain, s)
	}
	add(PrincipalFromContext(ctx))
	if agentID != "" {
		add(AgentPrincipal(agentID))
	}
	add(TaskBeneficiaryFromContext(ctx))
	return chain
}
