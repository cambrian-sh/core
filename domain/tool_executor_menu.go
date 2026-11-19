package domain

// Tool menus and grant resolution: what an agent may see and call, under its
// grants, ceilings, skill-conferred run grants and scope (split from
// tool_executor.go, ENG-1531).

import (
	"context"
)

// WithinLeaseCeiling reports whether tool is inside the D12 ceiling bound to
// sessionToken. A lease the ceiling does not bind (every non-child lease), an
// empty token and a nil ceiling are all inside.
func (e *ToolExecutor) WithinLeaseCeiling(sessionToken, tool string) bool {
	if e == nil || e.leaseCeiling == nil || sessionToken == "" {
		return true
	}
	inside, bound := e.leaseCeiling.AllowsLease(sessionToken, tool)
	return !bound || inside
}

// AvailableTools returns the system tools an agent may invoke, for building the
// agent's ReAct prompt menu (ADR-0039 / SDK ReAct routing). It mirrors grantFor's
// authority rules so the menu matches what Execute will actually allow: an
// anonymous principal sees nothing; under the unrestricted bypass every named
// agent sees every registered tool; otherwise the agent sees exactly the tools
// named by its grants. This is an advisory menu only — Execute still authorizes
// (grant + resource policy + scope + approval) every call (A1.4).
// AllTools returns the whole registered tool catalog, independent of any agent's
// grants. The operator plane governs the catalog at ScopeSystem (ADR-0047
// Amendment A2.3) — distinct from AvailableTools, which is a per-agent
// grant-filtered advisory menu. nil registry ⇒ empty catalog.
func (e *ToolExecutor) AllTools() []SystemTool {
	if e.Registry == nil {
		return nil
	}
	return e.Registry.All()
}

func (e *ToolExecutor) AvailableTools(ctx context.Context, agentID string) []SystemTool {
	if agentID == "" {
		return nil // anonymous principal: no menu, same as fail-closed grantFor
	}
	var menu []SystemTool
	if e.unrestricted {
		// ENG-2232: the bypass does not reach a guarded free-query tool, so the
		// menu offers one only when the agent holds an exact grant for it.
		for _, t := range e.Registry.All() {
			if e.freeQueryGuarded(t) {
				if _, ok := e.explicitGrant(ctx, agentID, t.Name); !ok {
					continue
				}
			}
			menu = append(menu, t)
		}
	} else {
		grants, err := e.Grants.GrantsFor(ctx, agentID)
		if err != nil {
			return nil
		}
		menu = e.Registry.SchemasFor(grants)
	}
	// ADR-0127 D4: the task beneficiary's contributed tools join the kernel
	// menu — resolved per task, appended here so the restricted-principal
	// ceiling below applies to them exactly as it does to kernel tools.
	menu = append(menu, e.contributedTools(ctx, agentID)...)
	// ADR-0051 D6 + the profile ceiling: a restricted principal sees only its
	// allowlisted tools, so the advisory menu matches what grantFor will allow.
	if !e.ceilingBinds(agentID) {
		return menu
	}
	filtered := menu[:0:0]
	for _, t := range menu {
		if e.withinCeiling(agentID, t.Name) {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// ceilingBinds reports whether any tool ceiling (RestrictedTools, Ceiling) binds
// agentID, so an unbound principal's menu is not re-filtered.
func (e *ToolExecutor) ceilingBinds(agentID string) bool {
	if _, restricted := e.RestrictedTools[agentID]; restricted {
		return true
	}
	if e.ceiling != nil {
		if _, bound := e.ceiling.Allows(agentID, ""); bound {
			return true
		}
	}
	return false
}

// withinCeiling reports whether tool is inside every ceiling that binds agentID:
// the ADR-0051 D6 name set (RestrictedTools) and the profile glob allowlist
// (Ceiling). An unbound principal is inside. One helper for every call site, so
// the menu, describe, grant and skill-confer paths cannot disagree.
func (e *ToolExecutor) withinCeiling(agentID, tool string) bool {
	if allow, restricted := e.RestrictedTools[agentID]; restricted && !allow[tool] {
		return false
	}
	if e.ceiling != nil {
		if inside, bound := e.ceiling.Allows(agentID, tool); bound && !inside {
			return false
		}
	}
	return true
}

// AvailableToolsRanked returns the agent's granted tools narrowed to the top-k
// most relevant to query (ADR-0044). Authorization is unchanged — it grant-filters
// via AvailableTools first, then ranks within that authorized set, so an ungranted
// tool can never appear. Degrades to the full menu when there is no query, no
// retriever, the set already fits in k, or ranking errors. An empty ranked result
// (the retriever's relevance floor cleared nothing) is honored — the menu is empty.
func (e *ToolExecutor) AvailableToolsRanked(ctx context.Context, agentID, query string, k int) []SystemTool {
	full := e.AvailableTools(ctx, agentID)
	if query == "" || e.Retriever == nil || k <= 0 || len(full) <= k {
		return full
	}
	names := make([]string, len(full))
	byName := make(map[string]SystemTool, len(full))
	for i, t := range full {
		names[i] = t.Name
		byName[t.Name] = t
	}
	ranked, err := e.Retriever.Rank(ctx, query, names, k)
	if err != nil {
		return full // degrade to the full menu on a ranking failure, never crash
	}
	out := make([]SystemTool, 0, len(ranked))
	for _, n := range ranked {
		if t, ok := byName[n]; ok {
			out = append(out, t)
		}
	}
	return out
}

// AvailableToolsNamed returns the named tools the agent may invoke — the
// describe_tool Tier-2 fetch (ADR-0045 D4/D6). It grant-filters via
// AvailableTools first, then keeps only the requested names, so an
// ungranted/unknown name is simply absent (fail-closed, no existence leak). An
// anonymous principal gets nothing.
func (e *ToolExecutor) AvailableToolsNamed(ctx context.Context, agentID string, names []string) []SystemTool {
	return e.AvailableToolsNamedFor(ctx, agentID, "", names)
}

// AvailableToolsNamedFor is AvailableToolsNamed plus the run-scoped skill overlay
// (ADR-0046 D6): a tool that a system skill loaded under sessionToken conferred is
// describable as well as callable. Without this the menu and the executor disagreed:
// ExecuteTool honoured the overlay through grantFor while the describe path served
// standing grants only, so a skill-granted tool reached the model with no argument
// schema (T-20261104-kernel-listtools-names-ignores-run-scoped-skill).
//
// The overlay half applies the same ceilings grantFor does: an anonymous principal
// gets nothing, and a restricted principal only its allowlisted tools. An empty
// sessionToken reproduces the standing-grants-only behaviour exactly.
func (e *ToolExecutor) AvailableToolsNamedFor(ctx context.Context, agentID, sessionToken string, names []string) []SystemTool {
	if len(names) == 0 || agentID == "" {
		return nil
	}
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	var out []SystemTool // nil when nothing matches (anonymous / all ungranted)
	for _, t := range e.AvailableTools(ctx, agentID) {
		if !e.WithinLeaseCeiling(sessionToken, t.Name) {
			continue // ADR-0139 D12: outside a child lease's toolset
		}
		if _, ok := want[t.Name]; ok {
			out = append(out, t)
			delete(want, t.Name)
		}
	}
	if sessionToken == "" || e.Overlay == nil || e.Registry == nil {
		return out
	}
	for _, n := range names { // input order, so the result is deterministic
		if _, pending := want[n]; !pending {
			continue
		}
		if !e.withinCeiling(agentID, n) || !e.WithinLeaseCeiling(sessionToken, n) {
			continue
		}
		if !e.Overlay.Granted(sessionToken, n) {
			continue
		}
		if t, ok := e.Registry.Get(n); ok {
			out = append(out, t)
			delete(want, n)
		}
	}
	return out
}

func (e *ToolExecutor) grantFor(ctx context.Context, agentID, tool, sessionToken string) (ToolGrant, bool) {
	if agentID == "" {
		return ToolGrant{}, false // anonymous principal denied even when unrestricted
	}
	// ADR-0051 D6: a restricted principal may use ONLY its allowlisted (discovery-safe)
	// tools — a hard ceiling enforced BEFORE the Unrestricted bypass, so it holds even in
	// dev/unrestricted mode. Everything else is denied fail-closed.
	//
	// RestrictedTools is empty in a current kernel: its only principal was the Scout,
	// retired 2026-08-07 with the config key that populated it. An absent entry means
	// "not a restricted principal", so an empty map is a no-op here rather than a
	// deny-all — the ceiling stays available for the next read-only organ.
	//
	// The profile ceiling (Ceiling) is checked here too, for the same reason: a
	// profile's tool allowlist must hold under the unrestricted bypass.
	if !e.withinCeiling(agentID, tool) {
		return ToolGrant{}, false
	}
	// ADR-0139 D12: a subagent's lease carries its toolset as a hard ceiling,
	// held under the Unrestricted bypass and over the skill overlay below.
	if !e.WithinLeaseCeiling(sessionToken, tool) {
		return ToolGrant{}, false
	}
	// ENG-2232: a guarded free-query tool (raw SQL and the like; the LLM never
	// writes SQL, ADR-0149 D6) is granted only by a grant naming it exactly.
	// Neither the Unrestricted bypass, nor a fleet wildcard, nor a skill's
	// run-scoped overlay below reaches it; a store error denies (fail closed).
	if e.Registry != nil {
		if t, ok := e.Registry.Get(tool); ok && e.freeQueryGuarded(t) {
			return e.explicitGrant(ctx, agentID, tool)
		}
	}
	// Unrestricted: any named agent gets an allow-all grant for any registered
	// tool (operator-chosen bypass). Approval for dangerous tools still applies.
	if e.unrestricted {
		return ToolGrant{Tool: tool, Policy: ToolResourcePolicy{AllowAll: true}}, true
	}
	grants, err := e.Grants.GrantsFor(ctx, agentID)
	if err != nil {
		return ToolGrant{}, false
	}
	for _, g := range grants {
		if grantCovers(g, tool) {
			// A fleet grant is reported against the concrete tool it covered, so
			// receipts and audits name local:<machine>/<tool>, never "local:*".
			g.Tool = tool
			return g, true
		}
	}
	// ADR-0046 D6: a system skill loaded this run may have conferred this tool
	// (run-scoped overlay, keyed by the session token). The conferred grant carries
	// an allow-all resource policy — the operator vouched for the skill — but
	// dangerous tools still hit ApprovalController in Execute.
	if e.Overlay.Granted(sessionToken, tool) {
		return ToolGrant{Tool: tool, Policy: ToolResourcePolicy{AllowAll: true}}, true
	}
	return ToolGrant{}, false
}

// freeQueryGuarded reports whether tool is a free-query tool the guard holds
// back from agents (ENG-2232).
func (e *ToolExecutor) freeQueryGuarded(tool SystemTool) bool {
	return tool.FreeQuery && !e.freeQueryOpen
}

// explicitGrant returns the agent's grant that names tool exactly. A profile
// grant counts: the profile grant sync derives one for a free-query tool only
// from an allowlist entry naming it literally (app/profile_agents.go).
func (e *ToolExecutor) explicitGrant(ctx context.Context, agentID, tool string) (ToolGrant, bool) {
	grants, err := e.Grants.GrantsFor(ctx, agentID)
	if err != nil {
		return ToolGrant{}, false
	}
	for _, g := range grants {
		if g.Tool == tool {
			return g, true
		}
	}
	return ToolGrant{}, false
}

// FreeQueryDenyReason is the reason a guarded free-query call without an exact
// grant is refused with, for the agent and the operator.
const FreeQueryDenyReason = "free-query tool: it takes raw SQL or another free query language, and agents use typed tools instead (ADR-0149 D6); an operator can grant this tool by its exact name (execution.tools.free_query)"

// EndRun drops every grant a system skill conferred for the run identified by
// session — the ADR-0046 D6 ephemerality backstop ("the overlay falls away when
// the task completes"). The kernel calls it when the step lease the agent
// presented as its session token is completed (server.go), so a conferred
// capability never outlives the run and the overlay cannot grow unbounded
// (T-20261031-skill-conferred-run-grants-are-never-cleared). Nil-safe.
func (e *ToolExecutor) EndRun(session string) {
	if e == nil {
		return
	}
	e.Overlay.Clear(session)
	// The step's repeated-failure record goes with its lease.
	e.StepFailures.End(LeaseID(session))
}

// ConferSkillGrants activates a loaded system skill's tool grants run-scoped
// (ADR-0046 D6), INTERSECTED with what policy permits the principal (ADR-0085 D4).
//
// This is the security-critical half of the skill model. A skill is gated on
// retrieval by its tags, and loading it activates tool grants — so without an
// intersection, "if you can see the skill you get its tools" makes skill
// visibility a privilege-granting operation, and the skill tag vocabulary
// silently becomes a tool-permission vocabulary.
//
// The intersection is against POLICY, not against the agent's static grants: a
// system skill is operator-authored and may still confer a tool the agent has no
// standing grant for (that is D6, and it is the point of skills). What it can
// never do is confer a tool the decision point refuses — loading a skill only
// ever narrows or maintains privilege, exactly as a restricted Windows token
// cannot be widened by the thing it loads.
//
// A clipped grant does NOT fail the load: the rest of the skill activates and a
// ReasonSkillGrantClipped decision is returned per dropped tool. Denying the whole
// skill makes the system feel broken; granting the tool is a security hole.
// Returns the granted set and one decision per clip.
func (e *ToolExecutor) ConferSkillGrants(ctx context.Context, session, agentID string, tools []string) (granted []string, clipped []AccessDecision) {
	if len(tools) == 0 {
		return nil, nil
	}
	a := e.authz
	if a == nil {
		a = AllowAllAuthorizer{}
	}
	for _, name := range tools {
		tool, known := e.Registry.Get(name)
		if !known {
			clipped = append(clipped, AccessDecision{
				Resource:  ResourceRef{Kind: KindTool, ID: name},
				Principal: AgentPrincipal(agentID),
				Reason:    ReasonSkillGrantClipped,
				Detail:    "skill grants an unknown tool",
			})
			continue
		}
		// ADR-0051 D6: a restricted principal's hard ceiling outranks a skill —
		// otherwise a skill would be the way around a restricted principal's confinement.
		if !e.withinCeiling(agentID, name) || !e.WithinLeaseCeiling(session, name) {
			clipped = append(clipped, AccessDecision{
				Resource:  tool.AuthzRef(),
				Principal: AgentPrincipal(agentID),
				Reason:    ReasonSkillGrantClipped,
				Detail:    "outside the principal's restricted tool ceiling",
			})
			continue
		}
		dec := a.Authorize(ctx, AccessRequest{
			Principal: AgentPrincipal(agentID),
			Surface:   SurfaceFromContext(ctx),
			Resource:  tool.AuthzRef(),
			Tags:      tool.AuthzTags(),
			Effects:   tool.Effects,
			Session:   SessionID(session),
		})
		if !dec.Allowed {
			dec.Reason = ReasonSkillGrantClipped
			if dec.Detail == "" {
				dec.Detail = "not permitted to this principal"
			}
			clipped = append(clipped, dec)
			continue
		}
		granted = append(granted, name)
	}
	e.Overlay.Activate(session, granted)
	return granted, clipped
}

// effectDecision asks the decision point whether every effect this tool declares
// is permitted to this principal on this surface. With no decision point (OSS) the
// answer is always yes — the check runs, the policy is simply empty.
func (e *ToolExecutor) effectDecision(ctx context.Context, req ToolCallRequest, tool SystemTool) AccessDecision {
	if len(tool.Effects) == 0 {
		// Unclassified only reaches here in non-strict mode, where registration
		// inferred a set. An empty set at this point means the tool bypassed
		// validation entirely, which is a wiring bug — deny and say so.
		return AccessDecision{
			Allowed: false, Reason: ReasonEffectNotPermitted,
			Detail: "tool declares no effect classes (unvalidated registration)",
		}
	}
	a := e.authz
	if a == nil {
		a = AllowAllAuthorizer{}
	}
	return a.Authorize(ctx, AccessRequest{
		Principal: AgentPrincipal(req.AgentID),
		Surface:   SurfaceFromContext(ctx),
		Resource:  tool.AuthzRef(),
		Tags:      tool.AuthzTags(),
		Effects:   tool.Effects,
		Session:   SessionID(req.SessionTokenID),
	})
}

// ScopeAllows is scopeAllows for the ADR-0145 D12 composer: whether the principal,
// under the person on ctx, may use a tool's data stores at all. Advisory at
// composition time; Execute still authorizes every call.
func (e *ToolExecutor) ScopeAllows(ctx context.Context, agentID string, tool SystemTool) bool {
	if e == nil {
		return false
	}
	return e.scopeAllows(ctx, agentID, tool)
}

// scopeAllows applies the data-store regime (ADR-0039 D8 Regime 1): a tool that
// touches tagged stores may run only if the principal's predicate admits the tag
// classes the tool declares. The decision point resolves the predicate; a nil
// predicate (the plugin could not resolve the principal) denies.
func (e *ToolExecutor) scopeAllows(ctx context.Context, agentID string, tool SystemTool) bool {
	a := e.authz
	if a == nil {
		a = AllowAllAuthorizer{}
	}
	eff, _ := a.ReadFilter(ctx, AgentPrincipal(agentID), SurfaceFromContext(ctx))
	if eff == nil {
		return false
	}
	if !eff.Allows(tool.DataReadKinds) {
		return false
	}
	if len(tool.DataWriteKinds) > 0 && !eff.Allows(tool.DataWriteKinds) {
		return false
	}
	return true
}
