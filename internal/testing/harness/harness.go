package harness

import (
	"context"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// Config holds optional overrides for the SystemHarness.
type Config struct {
	MaxConcurrentSessions int
}

// SystemHarness wires all subsystems with deterministic fakes for E2E testing.
type SystemHarness struct {
	Generator  *FakeGenerator
	Embedder   *FakeEmbedder
	Gateway    *FakeLLMGateway
	Registry   *HarnessRegistry
	Observer   *CapturingTelemetryObserver
	ExecCfg    config.ExecutionConfig
}

func New(cfg Config) *SystemHarness {
	if cfg.MaxConcurrentSessions == 0 {
		cfg.MaxConcurrentSessions = 10
	}
	return &SystemHarness{
		Generator:  NewFakeGenerator(),
		Embedder:   NewFakeEmbedder(),
		Gateway:    NewFakeLLMGateway(cfg.MaxConcurrentSessions),
		Registry:   NewHarnessRegistry(),
		Observer:   &CapturingTelemetryObserver{},
		ExecCfg: config.ExecutionConfig{
			PlanTimeoutMs:       120000,
			MinAuctionConfidence: 0.3,
		},
	}
}

// ExecutePlan runs a full DAG execution with the fake generator.
func (h *SystemHarness) ExecutePlan(ctx context.Context) (*domain.Handoff, []domain.TaskEvent, error) {
	fakeWriter := &fakeTaskEventWriter{}

	stepFn := executer.StepFunc(func(ctx context.Context, i int, handoff *domain.Handoff) (*domain.Handoff, error) {
		resp, err := h.Generator.Generate(ctx, string(handoff.Payload.Data))
		if err != nil {
			return nil, err
		}
		return &domain.Handoff{
			ID:        "step-result",
			FromAgent: "test-agent",
			Payload:   &domain.Payload{Data: []byte(resp)},
		}, nil
	})

	dag := &executer.DAGExecutor{
		EventWriter:      fakeWriter,
		Observer:         h.Observer,
		ThoughtFn:        stepFn,
		MaxReplanAttempts: 2,
	}

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{{Query: "execute this task"}},
	}

	masterCtx, err := dag.Execute(ctx, plan, nil, stepFn)
	if err != nil {
		return nil, fakeWriter.events, err
	}

	finalResult := masterCtx["_dag_final_result"]
	return &domain.Handoff{
		Payload: &domain.Payload{Data: []byte(finalResult)},
	}, fakeWriter.events, nil
}

func (h *SystemHarness) AddResponse(resp string) {
	h.Generator.responses = append(h.Generator.responses, resp)
}

type fakeTaskEventWriter struct {
	events []domain.TaskEvent
}

func (w *fakeTaskEventWriter) WriteTaskEvent(evt domain.TaskEvent) error {
	w.events = append(w.events, evt)
	return nil
}
