package awareness

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// capturePlanGen records the prompt it was handed and returns a scripted plan JSON, so a
// test can assert what reached the Planner and what plan came back.
type capturePlanGen struct {
	captured string
	response string
}

func (g *capturePlanGen) Generate(_ context.Context, prompt string) (string, error) {
	g.captured = prompt
	return g.response, nil
}

func sevenStepPlan() string {
	steps := make([]string, 0, 7)
	for i := 1; i <= 7; i++ {
		steps = append(steps, fmt.Sprintf(`{"query":"write remaining section %d","depends_on":[]}`, i))
	}
	return `{"steps":[` + strings.Join(steps, ",") + `],"subject":"helicopter"}`
}

// A DiscoveryReport (however produced — now by the Python Scout agent) flows through ctx into
// the Planner: the prompt carries <DiscoveryLTM> and the plan is shaped to the observation.
func TestDiscoveryPlanner_ShapesPlanToObservation(t *testing.T) {
	report := &domain.DiscoveryReport{
		Entities: []domain.DiscoveredEntity{
			{Kind: "dir", ID: "helicopter", Exists: true, Summary: "3 of 10 written, 7 missing"},
		},
		Interpretation: "one file per section; 7 remain",
		Environment:    &domain.EnvFacts{OS: "windows", Home: `C:\Users\x`, Desktop: `C:\Users\x\Desktop`},
	}
	ctx := domain.WithDiscovery(context.Background(), report)

	gen := &capturePlanGen{response: sevenStepPlan()}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)
	plan, err := planner.GetExecutionPlan(ctx, "continue the helicopter folder where we left off")
	if err != nil {
		t.Fatalf("GetExecutionPlan: %v", err)
	}
	for _, want := range []string{"</DiscoveryLTM>", `<entity kind="dir"`, `id="helicopter"`, "7 remain", `os="windows"`} {
		if !strings.Contains(gen.captured, want) {
			t.Errorf("planner prompt missing %q\n---\n%s", want, gen.captured)
		}
	}
	if len(plan.Steps) != 7 {
		t.Errorf("expected a 7-step plan shaped to the observation; got %d", len(plan.Steps))
	}
}

// A no-existing-state request (fresh creation) yields NO entities, but env grounding still
// flows so the Planner emits correct absolute paths (the ~/Desktop-on-Windows fix).
func TestDiscoveryPlanner_EnvOnlyGrounding(t *testing.T) {
	report := &domain.DiscoveryReport{
		Environment: &domain.EnvFacts{OS: "windows", Home: `C:\Users\x`, Desktop: `C:\Users\x\Desktop`, Cwd: `C:\proj`},
	}
	if report.IsEmpty() {
		t.Fatal("an env-only report grounds the planner and must not be empty")
	}
	ctx := domain.WithDiscovery(context.Background(), report)

	gen := &capturePlanGen{response: `{"steps":[{"query":"create the folder","depends_on":[]}],"subject":"cambrian"}`}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)
	if _, err := planner.GetExecutionPlan(ctx, "create a folder Cambrian on the desktop"); err != nil {
		t.Fatalf("GetExecutionPlan: %v", err)
	}
	if !strings.Contains(gen.captured, "<environment") || !strings.Contains(gen.captured, `desktop="C:\Users\x\Desktop"`) {
		t.Errorf("planner prompt must carry environment facts with the real desktop path\n%s", gen.captured)
	}
	if strings.Contains(gen.captured, "<entity kind=") {
		t.Error("a no-entity request must not inject entity grounding")
	}
}
