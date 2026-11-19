package domain

import (
	"sort"
	"sync"
)

// ─────────────────────────────────────────────────────────────────────────────
// Tool ceilings: per-principal hard limits on which tool NAMES a principal may
// touch, independent of how it would otherwise be authorised (agent stack v2,
// decisions/agent-stack-v2-mcp-tools-and-profiles.md D3; agents/profiles/README.md).
//
// A declarative agent profile carries a tool allowlist (`tools:` globs such as
// `mcp:filesystem/read_*`). The kernel enforces it twice:
//
//   - as GRANTS: the allowlist is expanded against the tool registry into the
//     agent's ToolGrants, and re-expanded when MCP servers (re)connect, so a
//     profile agent is granted exactly the registered tools its globs match;
//   - as a CEILING: the same globs are consulted wherever the executor consults
//     RestrictedTools, BEFORE the ExecutionConfig.Tools.ToolsUnrestricted bypass
//     and before a skill's run-scoped overlay. So a profile agent stays inside its
//     allowlist in unrestricted mode, and a skill cannot widen it.
//
// The ceiling is glob-based rather than a name set so it needs no re-expansion:
// a tool an MCP server registers later is inside or outside the ceiling by its
// name alone. RestrictedTools (ADR-0051 D6, name sets) stays as it was; the two
// ceilings compose (a tool must be inside both).
// ─────────────────────────────────────────────────────────────────────────────

// ToolCeiling bounds the tools a principal may use.
//
// Allows reports bound=false for a principal the ceiling knows nothing about (no
// limit), and for a bound principal whether tool is inside its ceiling.
type ToolCeiling interface {
	Allows(agentID, tool string) (inside, bound bool)
}

// ToolAllowlist is a list of tool-name globs (`*` any run, `?` one character,
// case-sensitive, no character classes) — the profile `tools:` field. An empty
// allowlist allows nothing. Pattern validity (no `[...]`, no `**`, a literal MCP
// server id) is checked where profiles are parsed (internal/agentprofile); here a
// pattern is only matched.
type ToolAllowlist []string

// Allows reports whether a tool name matches any pattern.
func (a ToolAllowlist) Allows(tool string) bool {
	if tool == "" {
		return false
	}
	for _, p := range a {
		if MatchToolGlob(p, tool) {
			return true
		}
	}
	return false
}

// Expand returns the registered tools the allowlist matches, sorted by name.
func (a ToolAllowlist) Expand(tools []SystemTool) []string {
	var out []string
	for _, t := range tools {
		if a.Allows(t.Name) {
			out = append(out, t.Name)
		}
	}
	sort.Strings(out)
	return out
}

// ExpandGrants is Expand for deriving grants. With freeQueryLiteral (the
// free-query guard, ENG-2232) a SystemTool.FreeQuery tool is derived only from
// a pattern that IS its name: a glob such as `mcp:postgres/*` is not an
// explicit grant of free SQL. The ceiling (Allows) is unchanged — it bounds,
// it never grants.
func (a ToolAllowlist) ExpandGrants(tools []SystemTool, freeQueryLiteral bool) []string {
	var out []string
	for _, t := range tools {
		if !a.Allows(t.Name) {
			continue
		}
		if freeQueryLiteral && t.FreeQuery && !a.names(t.Name) {
			continue
		}
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// names reports whether some pattern is exactly tool (no wildcard needed).
func (a ToolAllowlist) names(tool string) bool {
	for _, p := range a {
		if p == tool {
			return true
		}
	}
	return false
}

// Unmatched returns the patterns that match none of the given tools, in
// allowlist order (a boot warning: the profile names a tool no server lists).
func (a ToolAllowlist) Unmatched(tools []SystemTool) []string {
	var out []string
	for _, p := range a {
		hit := false
		for _, t := range tools {
			if MatchToolGlob(p, t.Name) {
				hit = true
				break
			}
		}
		if !hit {
			out = append(out, p)
		}
	}
	return out
}

// MatchToolGlob matches name against pattern with Python fnmatch.fnmatchcase
// semantics restricted to `*` and `?` — the SDK's ToolAllowlist
// (sdk/cambrian_agent_sdk/profiles.py), so the client-side menu filter and the
// kernel's enforcement agree on every name. Unlike path.Match, `*` crosses `/`
// and `\` is an ordinary character.
func MatchToolGlob(pattern, name string) bool {
	p, n := []rune(pattern), []rune(name)
	pi, ni := 0, 0
	star, mark := -1, 0
	for ni < len(n) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == n[ni]):
			pi++
			ni++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ni
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ni = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// ProfileToolCeiling is the ToolCeiling built from profile allowlists. Safe for
// concurrent use; Set replaces one principal's ceiling.
type ProfileToolCeiling struct {
	mu    sync.RWMutex
	allow map[string]ToolAllowlist
}

// NewProfileToolCeiling builds an empty ceiling (binds nobody).
func NewProfileToolCeiling() *ProfileToolCeiling {
	return &ProfileToolCeiling{allow: map[string]ToolAllowlist{}}
}

// Set binds agentID to allow. A nil allow still binds (to nothing): a profile
// with no tools may use no tools.
func (c *ProfileToolCeiling) Set(agentID string, allow ToolAllowlist) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.allow[agentID] = append(ToolAllowlist(nil), allow...)
}

// Allows implements ToolCeiling.
func (c *ProfileToolCeiling) Allows(agentID, tool string) (inside, bound bool) {
	if c == nil {
		return false, false
	}
	c.mu.RLock()
	allow, ok := c.allow[agentID]
	c.mu.RUnlock()
	if !ok {
		return false, false
	}
	return allow.Allows(tool), true
}

// Bound returns the principals the ceiling binds and their allowlists (a copy).
func (c *ProfileToolCeiling) Bound() map[string]ToolAllowlist {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]ToolAllowlist, len(c.allow))
	for id, a := range c.allow {
		out[id] = append(ToolAllowlist(nil), a...)
	}
	return out
}
