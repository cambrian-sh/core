package executer_test

import (
	"context"
	"errors"
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

// The E2E shape of use-case journey p05 ("rename last month's receipts and move them
// to Documents/Receipts", R-20261106T033413Z) with a fake LLM: a real Planner
// (planner_cap v3) plans it, a real DAGExecutor runs it with fan-out, and every step
// is bound by the real Dispatcher + Gatekeeper with the effect check on. The move
// step and each of its fan-out children must reach only the agent whose tools can
// move — never the assistant that can only write (which, in the live run, offered to
// overwrite a receipt with placeholder text).
//
// T-20261106-kernel-does-not-check-that-a-declared (owner option B, fail-safe).

type fleet struct {
	agents []domain.AgentDefinition
}

func (f *fleet) GetAllAgents(context.Context) ([]domain.AgentDefinition, error) { return f.agents, nil }
func (f *fleet) GetManifest(_ context.Context, id string) (*domain.AgentManifest, error) {
	// Every agent declares file_organisation: the TAG does not distinguish them,
	// exactly as in the live run. Only the effect check can.
	return &domain.AgentManifest{Capabilities: []string{"file_organisation"}}, nil
}

type toolsByAgent map[string][]domain.SystemTool

func (t toolsByAgent) AvailableTools(_ context.Context, id string) []domain.SystemTool { return t[id] }

var (
	listDir   = domain.SystemTool{Name: "mcp:filesystem/list_directory", Effects: []domain.ToolEffect{domain.EffectRead}}
	mkdir     = domain.SystemTool{Name: "mcp:filesystem/create_directory", Effects: []domain.ToolEffect{domain.EffectWrite}}
	writeFile = domain.SystemTool{Name: "mcp:filesystem/write_file", Effects: []domain.ToolEffect{domain.EffectWrite}}
	moveFile  = domain.SystemTool{Name: "mcp:filesystem/move_file", Effects: []domain.ToolEffect{domain.EffectWrite}}
)

// scriptedLLM answers planning and replanning calls in order and records the prompts.
type scriptedLLM struct {
	mu      sync.Mutex
	replies []string
	prompts []string
}

func (s *scriptedLLM) Generate(_ context.Context, prompt string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, prompt)
	if len(s.replies) == 0 {
		return "", errors.New("scriptedLLM: no reply left")
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}

// recordingCaller is the agent plane: it records which agent ran which step.
type recordingCaller struct {
	mu  sync.Mutex
	ran map[string][]string // agent → step queries
}

func (c *recordingCaller) CallAgent(_ context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	q := plannerQuery(h)
	c.mu.Lock()
	c.ran[agentID] = append(c.ran[agentID], q)
	c.mu.Unlock()
	out := "done: " + q
	if strings.HasPrefix(q, "List") {
		// A structured list: a side-effecting fan-out (the move) expands only over one
		// (T-20261107-fan-out-over-a-prose-source-splits-markdown).
		out = `["receipt-apex.pdf","receipt-bolt.pdf"]`
	}
	return &domain.Handoff{FromAgent: agentID, Payload: &domain.Payload{Data: []byte(out)}}, nil
}

const p05Plan = `{"reasoning":"list, make the folder, then move each receipt","subject":"receipts","cache_policy":"tool","steps":[
 {"query":"List last month's receipt PDFs in Downloads","depends_on":[],"required_capabilities":[],"required_effects":["read"]},
 {"query":"Create a folder named Receipts in Documents","depends_on":[],"required_capabilities":[],"required_effects":["write"]},
 {"query":"Rename {item} by its date and title, then move it into the Receipts folder","depends_on":[0,1],"fan_out_over":0,"required_capabilities":[],"required_effects":["move"]}]}`

func buildP05(t *testing.T, tools toolsByAgent, llm *scriptedLLM) (*awareness.Planner, *executer.DAGExecutor, executer.StepFunc, *recordingCaller) {
	t.Helper()
	cfg := config.DefaultConfig().Execution
	cfg.Gatekeeper.MeritMode = config.MeritModeLegacy
	vocab, err := cfg.Capability.StepEffectVocabulary()
	if err != nil {
		t.Fatal(err)
	}
	reg := &fleet{agents: []domain.AgentDefinition{{ID: "assistant"}, {ID: "uc-personal-assistant"}, {ID: "researcher"}}}
	effects := gatekeeper.NewToolEffectSource(vocab, tools)
	gk := gatekeeper.NewGatekeeper(reg, cfg, gatekeeper.WithEffects(effects))
	caller := &recordingCaller{ran: map[string][]string{}}
	disp := &dispatch.Dispatcher{Gatekeeper: gk, Caller: caller, ExecCfg: cfg}

	planner := awareness.NewPlanner(llm, reg, nil)
	planner.SetCapabilityContract(true)
	planner.SetEffectSource(effects)

	// The server's stepFn, reduced to its routing contract: the executor's CURRENT
	// step (domain.PlanStepFrom) becomes the DispatchTask.
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
	exec := &executer.DAGExecutor{
		ReplanHandler:     awareness.NewPlannerReplanHandler(llm, awareness.WithCapabilityContract(planner)),
		MaxReplanAttempts: 2,
		MaxFanOutWidth:    10,
	}
	return planner, exec, stepFn, caller
}

func TestP05_MoveStepReachesOnlyAMoveCapableAgent(t *testing.T) {
	tools := toolsByAgent{
		"assistant":             {listDir, writeFile},       // can write, cannot move
		"uc-personal-assistant": {listDir, mkdir, moveFile}, // can move
		"researcher":            {listDir},                  // read only
	}
	llm := &scriptedLLM{replies: []string{p05Plan}}
	planner, exec, stepFn, caller := buildP05(t, tools, llm)

	plan, err := planner.GetExecutionPlan(context.Background(), "rename last month's receipts and move them to Documents/Receipts")
	if err != nil {
		t.Fatal(err)
	}
	// The planner was shown the vocabulary and each agent's tool-derived effects.
	for _, want := range []string{"Effects (the required_effects vocabulary):", "- move: ", "- uc-personal-assistant: move, read, write", "- assistant: read, write"} {
		if !strings.Contains(llm.prompts[0], want) {
			t.Errorf("planner prompt lacks %q", want)
		}
	}
	if _, err := exec.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	var moves []string
	for agent, qs := range caller.ran {
		for _, q := range qs {
			if strings.Contains(q, "move it into") {
				moves = append(moves, agent+": "+q)
				if agent != "uc-personal-assistant" {
					t.Errorf("a move step reached %s, whose tools cannot move: %q", agent, q)
				}
			}
		}
	}
	if len(moves) != 2 {
		t.Fatalf("want one move per receipt (fan-out of 2), got %v", moves)
	}
}

// No agent can move: the step is refused, the plan replans ONCE with the reason,
// the replan still needs move, and the run ends with the refusal (which the server
// turns into a question) — no agent is ever handed the move step.
func TestP05_NoMoveCapableAgent_ReplansOnceThenStops(t *testing.T) {
	tools := toolsByAgent{
		"assistant":             {listDir, writeFile},
		"uc-personal-assistant": {listDir, mkdir},
		"researcher":            {listDir},
	}
	replan := `{"reasoning":"keep the move; it is required","subject":"Replan: receipts","cache_policy":"tool","steps":[
 {"query":"Move each listed receipt into the Receipts folder, renamed by date and title","depends_on":[],"required_capabilities":[],"required_effects":["move"]}]}`
	llm := &scriptedLLM{replies: []string{p05Plan, replan, replan}}
	planner, exec, stepFn, caller := buildP05(t, tools, llm)

	plan, err := planner.GetExecutionPlan(context.Background(), "rename last month's receipts and move them to Documents/Receipts")
	if err != nil {
		t.Fatal(err)
	}
	_, err = exec.Execute(context.Background(), plan, nil, stepFn)
	var nem *domain.NoEffectMatchError
	if !errors.As(err, &nem) {
		t.Fatalf("want the run to end on the effect refusal, got %v", err)
	}
	if len(llm.prompts) != 2 {
		t.Fatalf("want exactly one replan after the refusal, got %d LLM calls", len(llm.prompts))
	}
	if !strings.Contains(llm.prompts[1], "no agent can move") {
		t.Errorf("the replan was not told why the step was refused:\n%s", llm.prompts[1])
	}
	for agent, qs := range caller.ran {
		for _, q := range qs {
			if strings.Contains(strings.ToLower(q), "move") {
				t.Errorf("%s was handed a move step it cannot do: %q", agent, q)
			}
		}
	}
}
