package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type stubTailStep struct{ name string }

func (s stubTailStep) Step() string { return s.name }
func (s stubTailStep) RunErasureStep(context.Context, domain.ErasedRecords) (int, error) {
	return 0, nil
}

// tailStepPlugin contributes ADR-0137 D7 tail steps through the registry.
type tailStepPlugin struct {
	testPlugin
	steps []domain.ErasureTailStep
}

func (p *tailStepPlugin) Register(r *Registry) error {
	for _, s := range p.steps {
		r.AddErasureTailStep(s)
	}
	return nil
}

// ENG-1966: plugin tail steps reach Options in registration order (the composition
// root hands them to the erase transaction by name and to the erasure worker), and
// a nil one is dropped.
func TestApplyPlugins_ErasureTailStepsReachOptions(t *testing.T) {
	p := &tailStepPlugin{testPlugin: testPlugin{name: "tails"},
		steps: []domain.ErasureTailStep{stubTailStep{"a"}, nil, stubTailStep{"b"}}}
	composed, err := applyPlugins(Options{Plugins: []Plugin{p}})
	if err != nil {
		t.Fatal(err)
	}
	names := erasureTailStepNames(composed.opts.ErasureTailSteps)
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("tail steps in Options = %v, want [a b]", names)
	}
}
