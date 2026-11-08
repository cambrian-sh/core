package awareness

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// Effect-checked dispatch, planner side (T-20261106-kernel-does-not-check-that-a-declared).

// fakeEffects is a fixed AgentEffectSource over the shipped default vocabulary.
type fakeEffects struct {
	vocab *domain.StepEffectVocabulary
	by    map[string][]string
}

func (f fakeEffects) Vocabulary() *domain.StepEffectVocabulary { return f.vocab }
func (f fakeEffects) AgentEffects(_ context.Context, id string) []string {
	return f.by[id]
}

func goldenEffects(t *testing.T) fakeEffects {
	t.Helper()
	v, err := config.CapabilityConfig{EffectVocabulary: config.DefaultEffectVocabulary()}.StepEffectVocabulary()
	if err != nil {
		t.Fatal(err)
	}
	return fakeEffects{vocab: v, by: map[string][]string{
		"worker_agent":   {"move", "read", "write"},
		"research_agent": {"egress", "read"},
	}}
}

const effectPlanReply = `{"reasoning":"r","subject":"s","cache_policy":"tool","steps":[
 {"query":"move the report","depends_on":[],"required_capabilities":["file_read"],"required_effects":["Move"," move ","teleport"]},
 {"query":"summarise it","depends_on":[0],"required_capabilities":[],"required_effects":[]},
 {"query":"legacy","depends_on":[]}]}`

func effectPlanner(t *testing.T, reply string) (*Planner, *requestRecorder) {
	t.Helper()
	gen := &requestRecorder{reply: reply}
	p := NewPlanner(gen, &vocabAgents{agents: goldenAgents(), caps: map[string][]string{"worker_agent": {"file_read"}},
		tools: map[string][]string{"worker_agent": {"mcp:filesystem/read_*", "mcp:filesystem/move_file"}}}, nil)
	p.SetCapabilityContract(true)
	p.SetEffectSource(goldenEffects(t))
	p.goos = "linux"
	return p, gen
}

// Golden: planner_cap v3 with the effect block in <capability_clusters> and the
// vocabulary as the enum of required_effects in the response schema.
func TestPlannerEffectRequestGolden(t *testing.T) {
	p, gen := effectPlanner(t, effectPlanReply)
	if _, err := p.GetExecutionPlan(context.Background(), "Move the report into Archive"); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "planner_cap_effects.golden", renderRequest(gen.only(t)))
}

func TestPlannerEffects_BlockSchemaAndDeclaredEffects(t *testing.T) {
	t.Parallel()
	p, gen := effectPlanner(t, effectPlanReply)
	plan, err := p.GetExecutionPlan(context.Background(), "Move the report into Archive")
	if err != nil {
		t.Fatal(err)
	}
	req := gen.only(t)
	msg := req.Messages[0].Content
	for _, want := range []string{"Effects (the required_effects vocabulary):", "- move: move or rename", "Agent effects (what each agent's tools can do):",
		"- worker_agent: move, read, write", "- research_agent: egress, read", "- bare_agent: (none)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(req.System[0].Text, "worker_agent: move") {
		t.Error("per-call effect data leaked into the cached system block")
	}
	if !strings.Contains(req.System[0].Text, "Effect requirements:") {
		t.Error("planner_cap v3 must carry the effect rules")
	}
	// The schema's required_effects items are constrained to the vocabulary.
	var sch map[string]any
	if err := json.Unmarshal(req.ResponseSchema.Schema, &sch); err != nil {
		t.Fatal(err)
	}
	items := sch["properties"].(map[string]any)["steps"].(map[string]any)["items"].(map[string]any)
	enum := items["properties"].(map[string]any)["required_effects"].(map[string]any)["items"].(map[string]any)["enum"]
	want := []any{"read", "write", "move", "delete", "execute", "egress", "send", "spend", "admin"}
	if !reflect.DeepEqual(enum, want) {
		t.Errorf("enum = %v, want %v", enum, want)
	}
	// Declared effects are normalised; an unknown one is KEPT (dispatch refuses it),
	// an empty list stays empty (declared none), an absent one stays nil (legacy).
	if got := plan.Steps[0].RequiredEffects; !reflect.DeepEqual(got, []string{"move", "teleport"}) {
		t.Errorf("step 0 effects = %v", got)
	}
	if got := plan.Steps[1].RequiredEffects; got == nil || len(got) != 0 {
		t.Errorf("step 1 effects = %#v, want an empty non-nil list", got)
	}
	if plan.Steps[2].RequiredEffects != nil {
		t.Errorf("step 2 declared nothing and must stay nil (unverified), got %#v", plan.Steps[2].RequiredEffects)
	}
}

// The vocabulary comes from config: an operator's narrower vocabulary is what the
// planner is shown and constrained to.
func TestPlannerEffects_VocabularyFromConfig(t *testing.T) {
	t.Parallel()
	v, err := config.CapabilityConfig{EffectVocabulary: []config.EffectTermConfig{
		{Name: "read", Classes: []string{"read"}, Description: "look"},
		{Name: "archive", Refines: "write", Tools: []string{"*archive*"}, Description: "archive a record"},
	}}.StepEffectVocabulary()
	if err != nil {
		t.Fatal(err)
	}
	gen := &requestRecorder{reply: effectPlanReply}
	p := NewPlanner(gen, &vocabAgents{agents: goldenAgents()}, nil)
	p.SetCapabilityContract(true)
	p.SetEffectSource(fakeEffects{vocab: v})
	if _, err := p.GetExecutionPlan(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	req := gen.only(t)
	if !strings.Contains(req.Messages[0].Content, "- archive: archive a record") || strings.Contains(req.Messages[0].Content, "- move:") {
		t.Errorf("the configured vocabulary was not the one shown:\n%s", req.Messages[0].Content)
	}
	if !strings.Contains(string(req.ResponseSchema.Schema), `"enum":["read","archive"]`) {
		t.Errorf("schema enum is not the configured vocabulary: %s", req.ResponseSchema.Schema)
	}
}

// replanEffectSource is a capability + effect contract source for the replan handler.
type replanEffectSource struct{ fakeEffects }

func (replanEffectSource) CapabilityContract(context.Context) (string, map[string]struct{}, bool, bool) {
	return "- file_read: worker_agent", map[string]struct{}{"file_read": {}}, false, true
}

func (r replanEffectSource) EffectContract(ctx context.Context) (string, *domain.StepEffectVocabulary) {
	return buildEffectBlock(ctx, r.fakeEffects, goldenAgents()), r.vocab
}

// Under-declaration: an agent that lacked the tool yielded the step, nobody could
// take it, and the step failed. The replan is told so (the yield reason is in
// <failure>) under replan_cap v3, whose rules say to add the missing effect, and
// it keeps the effect contract (block + enum).
func TestReplanEffects_YieldReasonFeedsTheReplan(t *testing.T) {
	gen := &requestRecorder{reply: `{"reasoning":"add move","subject":"Replan: s","cache_policy":"tool","steps":[{"query":"move the report","depends_on":[],"required_capabilities":[],"required_effects":["move"]}]}`}
	h := NewPlannerReplanHandler(gen, WithCapabilityContract(replanEffectSource{goldenEffects(t)}))
	yieldErr := errString(`yield: agent "research_agent" did not perform "move the report into Archive" itself (it yielded it, e.g. for lack of a tool) and dispatching it failed: no candidates`)
	plan, err := h.Replan(context.Background(), 0, yieldErr, nil, makePlan(domain.Step{Query: "file the report", RequiredEffects: []string{"read"}}))
	if err != nil {
		t.Fatal(err)
	}
	req := gen.only(t)
	if !strings.Contains(req.Messages[0].Content, "it yielded it, e.g. for lack of a tool") {
		t.Errorf("the yield reason did not reach <failure>:\n%s", req.Messages[0].Content)
	}
	for _, rule := range []string{"under-declared its effects", "no agent can move"} {
		if !strings.Contains(req.System[0].Text, rule) {
			t.Errorf("replan_cap v3 lacks the rule %q", rule)
		}
	}
	if !strings.Contains(req.Messages[0].Content, "- worker_agent: move, read, write") || !strings.Contains(string(req.ResponseSchema.Schema), `"enum"`) {
		t.Error("the replan did not keep the effect contract (block + enum)")
	}
	if got := plan.Steps[0].RequiredEffects; !reflect.DeepEqual(got, []string{"move"}) {
		t.Errorf("replanned effects = %v", got)
	}
	checkGolden(t, "replan_cap_effects.golden", renderRequest(req))
}
