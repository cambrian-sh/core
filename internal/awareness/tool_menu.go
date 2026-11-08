package awareness

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
)

// ── The planner's live tool menu ─────────────────────────────────────────────
//
// T-20261107-world-system-asks-are-planned-as-local-file: a finance analyst asked
// "which customers are more than 60 days overdue? draft a reminder to each". Her
// work systems were an MCP server (finance_aging_report, mail_draft, … 45 tools)
// granted to every profile, and effect-checked dispatch was correct — yet the
// planner wrote "search the working directory for invoice files", 6 of 6 times.
//
// The planner was shown each profile's DECLARED allowlist globs (agentToolLine):
//
//	assistant can call: vantis-emp-904 (all tools); filesystem (read_*, list_*,
//	  directory_tree, search_files, …); pdf-reader (all tools); …
//
// The filesystem, pdf-reader, markitdown, time and calculator entries matched no
// registered tool (those stdio servers were refused by their trust tier), so the
// only file tools the planner could name did not exist, and the one server that
// did exist was an opaque id with "all tools". Nothing told the planner that an
// agent could read the aging report, so it planned the only data source it could
// see. The executing agent then asked find_tools for a file-search tool, which
// ranked by that need and never surfaced finance_*.
//
// The fix is the menu's SOURCE: each agent's callable tools, from the same lister
// that the effect check and the executor use (domain.ToolExecutor.AvailableTools —
// grants inside every ceiling), so the planner, dispatch and enforcement read one
// set. A glob that matches nothing contributes nothing; each tool's purpose is
// shown once, as a one-line summary of its own description.

// ToolMenuSource lists the tools an agent can actually call. It is the
// planner-side narrowing of the executor's menu; *domain.ToolExecutor implements
// it (wired in app/effect_check_wiring.go together with the effect source).
type ToolMenuSource interface {
	AvailableTools(ctx context.Context, agentID string) []domain.SystemTool
}

// SetToolMenuSource wires the live tool menu. nil keeps the declared-glob lines.
func (p *Planner) SetToolMenuSource(src ToolMenuSource) { p.toolMenu = src }

// maxToolSummaries bounds how many tools get a summary line, so a fleet with a
// very large tool catalog cannot grow the planner prompt without limit. Tools past
// it are still named on their agent's line. A prompt-size guard, not a tuner: it is
// far above any one deployment's tool count measured so far (55 in the o3x run).
const maxToolSummaries = 200

// maxToolSummaryRunes caps one summary line.
const maxToolSummaryRunes = 140

// nativeServer is the group name for tools that are not MCP tools.
const nativeServer = "native"

// liveToolMenu collects each agent's callable tools and renders the tool section
// of <capability_clusters>.
type liveToolMenu struct {
	agents  []string                       // insertion order; sorted at render
	byAgent map[string]map[string][]string // agent → server → tool base names
	catalog map[string]map[string]string   // server → base name → summary
}

func newLiveToolMenu() *liveToolMenu {
	return &liveToolMenu{byAgent: map[string]map[string][]string{}, catalog: map[string]map[string]string{}}
}

// splitToolName splits "mcp:<server>/<tool>" into its server and tool; any other
// name is a native tool.
func splitToolName(name string) (server, tool string) {
	if rest, ok := strings.CutPrefix(name, "mcp:"); ok {
		if s, t, found := strings.Cut(rest, "/"); found {
			return s, t
		}
	}
	return nativeServer, name
}

// add records one agent's callable tools. An agent with an allowlist and no
// callable tool is still recorded, so the planner sees that it cannot act.
func (m *liveToolMenu) add(agentID string, tools []domain.SystemTool) {
	if _, seen := m.byAgent[agentID]; !seen {
		m.agents = append(m.agents, agentID)
	}
	servers := map[string][]string{}
	for _, t := range tools {
		server, base := splitToolName(t.Name)
		servers[server] = append(servers[server], base)
		if m.catalog[server] == nil {
			m.catalog[server] = map[string]string{}
		}
		if _, ok := m.catalog[server][base]; !ok || m.catalog[server][base] == "" {
			m.catalog[server][base] = toolSummary(t.Description)
		}
	}
	m.byAgent[agentID] = servers
}

// render returns the tool section, or "" when no agent was recorded.
//
//	Tools each agent can call (agents not listed declare no tool allowlist):
//	- assistant can call: vantis-emp-904 (all 45 tools below); native (calculator)
//	- researcher can call: vantis-emp-904 (drive_search, mail_list)
//
//	What the tools do:
//	vantis-emp-904:
//	- finance_aging_report: Accounts-receivable aging by customer and bucket.
//
// A server's tools are listed by name on an agent's line unless the agent holds
// every tool of that server shown below, which reads "all N tools below".
func (m *liveToolMenu) render() string {
	if len(m.agents) == 0 {
		return ""
	}
	agents := append([]string(nil), m.agents...)
	sort.Strings(agents)
	var sb strings.Builder
	sb.WriteString("Tools each agent can call (agents not listed declare no tool allowlist):\n")
	for _, id := range agents {
		servers := m.byAgent[id]
		if len(servers) == 0 {
			fmt.Fprintf(&sb, "- %s can call: no tool (none of its allowed tools is connected)\n", id)
			continue
		}
		names := menuKeys(servers)
		parts := make([]string, 0, len(names))
		for _, s := range names {
			tools := dedupSorted(servers[s])
			if all := len(m.catalog[s]); all > 1 && len(tools) == all {
				parts = append(parts, fmt.Sprintf("%s (all %d tools below)", s, all))
				continue
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", s, strings.Join(tools, ", ")))
		}
		fmt.Fprintf(&sb, "- %s can call: %s\n", id, strings.Join(parts, "; "))
	}

	servers := menuKeys(m.catalog)
	if len(servers) == 0 {
		return strings.TrimRight(sb.String(), "\n")
	}
	sb.WriteString("\nWhat the tools do:\n")
	shown := 0
	for _, s := range servers {
		fmt.Fprintf(&sb, "%s:\n", s)
		for _, base := range menuKeys(m.catalog[s]) {
			summary := m.catalog[s][base]
			if shown >= maxToolSummaries || summary == "" {
				fmt.Fprintf(&sb, "- %s\n", base)
			} else {
				fmt.Fprintf(&sb, "- %s: %s\n", base, summary)
			}
			shown++
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// toolSummary is the first sentence (or line) of a tool description, whitespace
// collapsed and capped at maxToolSummaryRunes. It is the tool author's own text,
// rendered as DATA inside <capability_clusters> (the region's closing tag is
// neutralised by the prompt renderer), never as instructions.
func toolSummary(desc string) string {
	d := strings.Join(strings.Fields(desc), " ")
	if d == "" {
		return ""
	}
	if i := strings.Index(d, ". "); i >= 0 {
		d = d[:i+1]
	}
	if utf8.RuneCountInString(d) > maxToolSummaryRunes {
		r := []rune(d)
		d = strings.TrimRight(string(r[:maxToolSummaryRunes-1]), " ") + "…"
	}
	return d
}

func menuKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupSorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	j := 0
	for i, s := range out {
		if i > 0 && s == out[j-1] {
			continue
		}
		out[j] = s
		j++
	}
	return out[:j]
}

// ── The chat front desk's view of the same menu ──────────────────────────────
//
// T-20261107-chat-agent-tells-the-user-it-has-no-tool-for: the chat front desk
// (agents/chat_agent.py) holds no tools; the planner's agents do. Its only view
// of capability was its own skills menu (find_skills), which has no entry for an
// MCP work system, so "what's in my CRM?" was answered "I don't have a CRM tool"
// instead of being delegated, although every worker profile held crm_*.
//
// WorkSystems renders, for the front desk, the MCP systems the planner's agents
// can call — from the planner's own provider and tool menu source, through the
// same liveToolMenu, so the chat lane and the planner read one set and cannot
// disagree (no second pathway). Native tools are left out: they are generic
// helpers, not a system the user names.

// maxWorkSystemTools bounds the tool names listed per server for the chat lane.
// A prompt-size guard like maxToolSummaries, far above one world's 45 tools.
const maxWorkSystemTools = 120

// WorkSystems returns one line per MCP server any planner agent can call, with
// the tools reachable on it:
//
//   - work-mcp: crm_search, finance_aging_report, mail_list, …
//
// "" when no tool menu source is wired (the declared-glob fallback names tools
// that may not exist, so the front desk is told nothing rather than something
// false), when the agents cannot be listed, or when no agent reaches an MCP tool.
func (p *Planner) WorkSystems(ctx context.Context) string {
	if p == nil || p.toolMenu == nil || p.provider == nil {
		return ""
	}
	agents, err := p.provider.GetAllAgents(ctx)
	if err != nil {
		return ""
	}
	live := newLiveToolMenu()
	for _, a := range agents {
		if a.Trait == domain.TraitModel {
			continue
		}
		// Same admission as buildCapabilityClusterFromManifests: an agent with no
		// declared allowlist contributes no tool line there, and none here.
		m, merr := p.provider.GetManifest(ctx, a.ID)
		if merr != nil || m == nil || len(m.ToolAllowlist) == 0 {
			continue
		}
		live.add(a.ID, p.toolMenu.AvailableTools(ctx, a.ID))
	}
	return live.renderSystems()
}

// renderSystems is the chat lane's rendering of the menu: servers and the union of
// their reachable tools, with no per-agent attribution (the front desk routes
// nothing to an agent; it only delegates to the planner).
func (m *liveToolMenu) renderSystems() string {
	reach := map[string]map[string]struct{}{}
	for _, servers := range m.byAgent {
		for s, tools := range servers {
			if s == nativeServer {
				continue
			}
			if reach[s] == nil {
				reach[s] = map[string]struct{}{}
			}
			for _, t := range tools {
				reach[s][t] = struct{}{}
			}
		}
	}
	if len(reach) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, s := range menuKeys(reach) {
		tools := menuKeys(reach[s])
		if len(tools) > maxWorkSystemTools {
			tools = append(tools[:maxWorkSystemTools:maxWorkSystemTools], fmt.Sprintf("… %d more", len(reach[s])-maxWorkSystemTools))
		}
		fmt.Fprintf(&sb, "- %s: %s\n", s, strings.Join(tools, ", "))
	}
	return strings.TrimRight(sb.String(), "\n")
}
