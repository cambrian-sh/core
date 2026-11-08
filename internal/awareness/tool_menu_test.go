package awareness

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type menuByAgent map[string][]domain.SystemTool

func (m menuByAgent) AvailableTools(_ context.Context, id string) []domain.SystemTool { return m[id] }

// T-20261107-world-system-asks-are-planned-as-local-file: with a menu source the
// tool lines are the agents' CALLABLE tools, MCP tools carry a one-line summary of
// their own description, and an allowlist glob that matches nothing shows nothing.
func TestCapabilityClusterShowsLiveToolsWithSummaries(t *testing.T) {
	agents := []domain.AgentDefinition{{ID: "assistant"}, {ID: "analyst"}, {ID: "offline"}, {ID: "legacy"}}
	prov := &vocabAgents{
		agents: agents,
		caps:   map[string][]string{"assistant": {"general_purpose"}, "analyst": {"analysis"}, "offline": {"file_read"}, "legacy": {"analysis"}},
		tools: map[string][]string{
			"assistant": {"mcp:erp/*", "mcp:filesystem/read_*"},
			"analyst":   {"mcp:erp/finance_*", "calculator"},
			"offline":   {"mcp:filesystem/*"},
		},
	}
	aging := domain.SystemTool{Name: "mcp:erp/finance_aging_report", Description: "Accounts-receivable aging\n by customer and bucket. Pass ledger=ar or ap."}
	send := domain.SystemTool{Name: "mcp:erp/mail_send", Description: "Send an email."}
	calc := domain.SystemTool{Name: "calculator", Description: ""}
	menu := menuByAgent{
		"assistant": {aging, send},
		"analyst":   {aging, calc},
		// "offline": its only server is not connected — no callable tool.
	}
	block, vocab := buildCapabilityClusterFromManifests(context.Background(), agents, prov, false, menu)
	for _, want := range []string{
		"Tools each agent can call (agents not listed declare no tool allowlist):\n",
		"- analyst can call: erp (finance_aging_report); native (calculator)",
		"- assistant can call: erp (all 2 tools below)",
		"- offline can call: no tool (none of its allowed tools is connected)",
		"\n\nWhat the tools do:\nerp:\n- finance_aging_report: Accounts-receivable aging by customer and bucket.\n- mail_send: Send an email.\nnative:\n- calculator",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block lacks %q:\n%s", want, block)
		}
	}
	for _, unwanted := range []string{"filesystem", "read_*", "legacy can call", "all tools)"} {
		if strings.Contains(block, unwanted) {
			t.Errorf("block contains %q:\n%s", unwanted, block)
		}
	}
	if len(vocab) != 3 {
		t.Errorf("tool lines leaked into the vocabulary: %v", vocab)
	}
}

// The planner wires the source through to the rendered request, and the replan
// path (CapabilityContract) shows the same menu.
func TestPlannerToolMenuSourceReachesThePromptAndTheReplan(t *testing.T) {
	agents := []domain.AgentDefinition{{ID: "assistant"}}
	prov := &vocabAgents{agents: agents, caps: map[string][]string{"assistant": {"general_purpose"}},
		tools: map[string][]string{"assistant": {"mcp:erp/*"}}}
	gen := &requestRecorder{reply: `{"reasoning":"r","subject":"s","cache_policy":"tool","steps":[{"query":"q","depends_on":[],"required_capabilities":[]}]}`}
	p := NewPlanner(gen, prov, nil)
	p.SetCapabilityContract(true)
	p.SetToolMenuSource(menuByAgent{"assistant": {{Name: "mcp:erp/finance_aging_report", Description: "AR aging."}}})
	if _, err := p.GetExecutionPlan(context.Background(), "who owes us"); err != nil {
		t.Fatal(err)
	}
	if got := renderRequest(gen.only(t)); !strings.Contains(got, "- finance_aging_report: AR aging.") {
		t.Errorf("planner request lacks the tool summary:\n%s", got)
	}
	block, _, _, on := p.CapabilityContract(context.Background())
	if !on || !strings.Contains(block, "- finance_aging_report: AR aging.") {
		t.Errorf("replan block lacks the tool summary:\n%s", block)
	}
}

func TestToolSummary(t *testing.T) {
	long := strings.Repeat("word ", 60)
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"  List   invoices.  ", "List invoices."},
		{"First sentence. Second sentence.", "First sentence."},
		{"Version 2.1 of the report", "Version 2.1 of the report"},
	} {
		if got := toolSummary(tc.in); got != tc.want {
			t.Errorf("toolSummary(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := toolSummary(long); len([]rune(got)) != maxToolSummaryRunes || !strings.HasSuffix(got, "…") {
		t.Errorf("long summary not capped: %d runes %q", len([]rune(got)), got)
	}
}

// The plan-from-the-tools rule is in the bytes both capability-arm producers send.
func TestCapabilityPromptsCarryThePlanFromToolsRule(t *testing.T) {
	for name, text := range map[string]string{"planner_cap": plannerCapPrompts.System.Body, "replan_cap": replanCapPrompts.System.Body} {
		if !strings.Contains(text, "Plan from the tools the agents have") || !strings.Contains(text, `"What the tools do"`) {
			t.Errorf("%s lacks the plan-from-the-tools rule", name)
		}
	}
}

// T-20261107-chat-agent-tells-the-user-it-has-no-tool-for: the chat lane's
// capability summary lists the MCP systems the planner's agents can call, from the
// SAME tool menu source the planner reads — servers with the union of their
// reachable tools, no native tools, nothing for an unconnected allowlist glob.
func TestWorkSystemsListsTheLiveMCPSystemsFromTheToolMenu(t *testing.T) {
	agents := []domain.AgentDefinition{{ID: "assistant"}, {ID: "analyst"}, {ID: "offline"}, {ID: "chatty"}}
	prov := &vocabAgents{
		agents: agents,
		caps:   map[string][]string{"assistant": {"general_purpose"}, "analyst": {"analysis"}},
		tools: map[string][]string{
			"assistant": {"mcp:emp/*"},
			"analyst":   {"mcp:emp/finance_*", "mcp:wiki/*", "calculator"},
			"offline":   {"mcp:filesystem/*"},
			// chatty declares no allowlist: it contributes nothing, as on the planner side.
		},
	}
	menu := menuByAgent{
		"assistant": {{Name: "mcp:emp/crm_search"}, {Name: "mcp:emp/mail_list"}, {Name: "mcp:emp/finance_aging_report"}},
		"analyst":   {{Name: "mcp:emp/finance_aging_report"}, {Name: "mcp:wiki/page_get"}, {Name: "calculator"}},
		"chatty":    {{Name: "mcp:secret/should_not_show"}},
	}
	p := NewPlanner(&requestRecorder{}, prov, nil)
	if got := p.WorkSystems(context.Background()); got != "" {
		t.Errorf("no tool menu source wired: want \"\", got %q", got)
	}
	p.SetToolMenuSource(menu)
	got := p.WorkSystems(context.Background())
	want := "- emp: crm_search, finance_aging_report, mail_list\n- wiki: page_get"
	if got != want {
		t.Errorf("WorkSystems:\n got %q\nwant %q", got, want)
	}
	for _, unwanted := range []string{"calculator", "filesystem", "secret"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("WorkSystems contains %q: %q", unwanted, got)
		}
	}
	var nilPlanner *Planner
	if nilPlanner.WorkSystems(context.Background()) != "" {
		t.Error("nil planner must render nothing")
	}
}
