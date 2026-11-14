package executer_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/awareness"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/dispatch"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/supervision/gatekeeper"
)

// The E2E shape of use-case journey o3x ("which customers are more than 60 days
// overdue? draft a reminder to each", R-20261106T210820Z / R-20261106T211946Z) with a
// fake LLM: the employee's work systems are an MCP server granted to the assistant,
// the profile's filesystem globs name a server that is NOT connected, and the
// planner must plan from the tools the agents can actually call.
//
// In the live runs the planner was shown the declared allowlist globs — named
// filesystem tools that did not exist and "vantis-emp-904 (all tools)" — and planned
// "search the working directory for invoice files" every time
// (T-20261107-world-system-asks-are-planned-as-local-file).

// worldFleet declares allowlists (unlike fleet), so the planner renders tool lines.
type worldFleet struct {
	agents []domain.AgentDefinition
	allow  map[string][]string
}

func (f *worldFleet) GetAllAgents(context.Context) ([]domain.AgentDefinition, error) {
	return f.agents, nil
}
func (f *worldFleet) GetManifest(_ context.Context, id string) (*domain.AgentManifest, error) {
	return &domain.AgentManifest{Capabilities: []string{"general_purpose"}, ToolAllowlist: f.allow[id]}, nil
}

// worldTool builds an MCP tool the way the registry holds one: its operator policy
// (effects) and the server's own description.
func worldTool(name, desc string, effs ...domain.ToolEffect) domain.SystemTool {
	return domain.SystemTool{Name: "mcp:acme-erp/" + name, Description: desc, Effects: effs}
}

var (
	agingReport = worldTool("finance_aging_report",
		"Accounts-receivable or payable aging by customer, in 0-30/31-60/61-90/90+ day buckets. Pass ledger=ar or ap.",
		domain.EffectRead)
	listInvoices = worldTool("finance_list_invoices", "List invoices with status, amount and due date.", domain.EffectRead)
	mailDraft    = worldTool("mail_draft", "Save an email draft in the user's mailbox.", domain.EffectWrite, domain.EffectEgress)
	mailSend     = worldTool("mail_send", "Send an email from the user's mailbox.", domain.EffectWrite, domain.EffectEgress)
	webFetch     = domain.SystemTool{Name: "mcp:fetch/fetch", Description: "Fetch a URL.", Effects: []domain.ToolEffect{domain.EffectEgress}}
)

// planFromMenuLLM is a recorded-shape planner: it answers like the live model did.
// Shown a menu that names the finance tool, it plans from the tool; shown only file
// tools, it plans the file search the live runs produced.
type planFromMenuLLM struct {
	mu      sync.Mutex
	prompts []string
}

const (
	o3xToolPlan = `{"reasoning":"the aging report has the overdue customers; drafts go to the mailbox","subject":"AR reminders","cache_policy":"tool","steps":[
 {"query":"Use finance_aging_report (ledger ar) to list the customers more than 60 days overdue with their amounts and billing contacts","depends_on":[],"required_capabilities":[],"required_effects":["read"]},
 {"query":"Use mail_draft to save one polite payment reminder per customer from step 0, addressed to its billing contact; do not send","depends_on":[0],"required_capabilities":[],"required_effects":["write","egress"]}]}`
	o3xFilePlan = `{"reasoning":"find the invoice files","subject":"AR reminders","cache_policy":"tool","steps":[
 {"query":"Search the working directory and subdirectories for files containing invoice data","depends_on":[],"required_capabilities":[],"required_effects":["read"]}]}`
)

func (l *planFromMenuLLM) Generate(_ context.Context, prompt string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prompts = append(l.prompts, prompt)
	if strings.Contains(prompt, "finance_aging_report") {
		return o3xToolPlan, nil
	}
	return o3xFilePlan, nil
}

func buildO3x(t *testing.T, liveMenu bool) (*awareness.Planner, *planFromMenuLLM, executer.StepFunc, *recordingCaller) {
	t.Helper()
	cfg := config.DefaultConfig().Execution
	cfg.Gatekeeper.MeritMode = config.MeritModeLegacy
	vocab, err := cfg.Capability.StepEffectVocabulary()
	if err != nil {
		t.Fatal(err)
	}
	reg := &worldFleet{
		agents: []domain.AgentDefinition{{ID: "assistant"}, {ID: "researcher"}},
		allow: map[string][]string{
			// The world server plus filesystem globs whose server is not connected.
			"assistant":  {"mcp:acme-erp/*", "mcp:filesystem/read_*", "mcp:filesystem/search_files"},
			"researcher": {"mcp:fetch/*"},
		},
	}
	// What the executor would let each agent call: grants × ceiling × live registry.
	// No filesystem tool exists, exactly as in the live run.
	tools := toolsByAgent{
		"assistant":  {agingReport, listInvoices, mailDraft, mailSend},
		"researcher": {webFetch},
	}
	effects := gatekeeper.NewToolEffectSource(vocab, tools)
	gk := gatekeeper.NewGatekeeper(reg, cfg, gatekeeper.WithEffects(effects))
	caller := &recordingCaller{ran: map[string][]string{}}
	disp := &dispatch.Dispatcher{Gatekeeper: gk, Caller: caller, ExecCfg: cfg}

	llm := &planFromMenuLLM{}
	planner := awareness.NewPlanner(llm, reg)
	planner.SetCapabilityContract(true)
	planner.SetEffectSource(effects)
	if liveMenu {
		planner.SetToolMenuSource(tools) // app wires the executor here (wireEffectCheck)
	}
	stepFn := func(ctx context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		step, _ := domain.PlanStepFrom(ctx)
		res, err := disp.Execute(ctx, &domain.DispatchTask{
			ID: fmt.Sprintf("task-%d", i), Description: step.Query,
			RequiredCapabilities: step.RequiredCapabilities, RequiredEffects: step.RequiredEffects,
		}, h)
		if err != nil {
			return nil, err
		}
		return res.Handoff, nil
	}
	return planner, llm, stepFn, caller
}

const o3xAsk = "Which customers are more than 60 days overdue? Draft a polite payment reminder to each of them, addressed to their accounts payable contact — don't send anything yet."

func TestO3x_FinanceAskIsPlannedFromTheWorldToolsAndReachesTheirHolder(t *testing.T) {
	planner, llm, stepFn, caller := buildO3x(t, true)
	plan, err := planner.GetExecutionPlan(context.Background(), o3xAsk)
	if err != nil {
		t.Fatal(err)
	}
	prompt := llm.prompts[0]
	for _, want := range []string{
		"- assistant can call: acme-erp (all 4 tools below)",
		"- researcher can call: fetch (fetch)",
		"What the tools do:",
		"- finance_aging_report: Accounts-receivable or payable aging by customer, in 0-30/31-60/61-90/90+ day buckets.",
		"- mail_draft: Save an email draft in the user's mailbox.",
		"- assistant: egress, read, send, write",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("planner prompt lacks %q", want)
		}
	}
	if strings.Contains(prompt, "search_files") || strings.Contains(prompt, "filesystem (") {
		t.Errorf("the planner was shown filesystem tools no connected server provides")
	}

	exec := &executer.DAGExecutor{MaxReplanAttempts: 0, MaxFanOutWidth: 10}
	if _, err := exec.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	ran := caller.ran["assistant"]
	if len(ran) != 2 || !strings.Contains(ran[0], "finance_aging_report") || !strings.Contains(ran[1], "mail_draft") {
		t.Fatalf("the finance and mail steps must reach the agent holding those tools; assistant ran %q, all %v", ran, caller.ran)
	}
	if len(caller.ran["researcher"]) != 0 {
		t.Errorf("researcher (fetch only) was dispatched a finance/mail step: %v", caller.ran["researcher"])
	}
}

// The pre-fix rendering, kept as the fallback when no menu source is wired, is what
// produced the file-search plan: it names the unconnected filesystem tools and
// shows the world server only as "all tools". This pins the diagnosis.
func TestO3x_DeclaredGlobMenuHidesTheWorldTools(t *testing.T) {
	planner, llm, _, _ := buildO3x(t, false)
	plan, err := planner.GetExecutionPlan(context.Background(), o3xAsk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(llm.prompts[0], "- assistant can call: acme-erp (all tools); filesystem (read_*, search_files)") {
		t.Fatalf("fallback rendering changed:\n%s", llm.prompts[0])
	}
	if !strings.Contains(plan.Steps[0].Query, "Search the working directory") {
		t.Errorf("recorded-shape fake: the glob menu should yield the file-search plan, got %q", plan.Steps[0].Query)
	}
}

// Effects of a typical world MCP server, through the shipped vocabulary: a finance
// read tool is read; a mail tool whose policy declares write+egress is write and
// egress, and send only when its name says it sends; an unlisted tool (the
// fail-closed default policy) contributes nothing.
func TestO3x_WorldToolPoliciesDeriveTheirEffects(t *testing.T) {
	vocab, err := config.DefaultConfig().Execution.Capability.StepEffectVocabulary()
	if err != nil {
		t.Fatal(err)
	}
	unlisted := worldTool("finance_record_payment", "Record a payment.", domain.EffectWrite)
	unlisted.EffectsDefaulted = true
	for _, tc := range []struct {
		tool domain.SystemTool
		want string
	}{
		{agingReport, "read"},
		{listInvoices, "read"},
		{mailDraft, "write,egress"},
		{mailSend, "write,egress,send"},
		{webFetch, "egress"},
		{unlisted, ""},
	} {
		if got := strings.Join(vocab.EffectsOfTool(tc.tool), ","); got != tc.want {
			t.Errorf("%s: effects %q, want %q", tc.tool.Name, got, tc.want)
		}
	}
	src := gatekeeper.NewToolEffectSource(vocab, toolsByAgent{"assistant": {agingReport, mailDraft, mailSend, unlisted}})
	if got := strings.Join(src.AgentEffects(context.Background(), "assistant"), ","); got != "egress,read,send,write" {
		t.Errorf("assistant effects %q", got)
	}
}
