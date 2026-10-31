package harness

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// FakeModelID is the model agent id the harness gateway streams through. It is
// the gateway's DEFAULT model, which is the path a kernel step takes when its
// StepAllocation names no winner (server.go stepFn leaves it empty on purpose).
const FakeModelID = "llm:fake"

// Config holds optional overrides for the SystemHarness.
type Config struct {
	// MaxConcurrentSessions is the gateway's CONWIP width
	// (execution.llm.llm_gateway_max_concurrency). 0 means the config default.
	MaxConcurrentSessions int
	// StepTokenLimit is the token budget leased to each step
	// (BudgetLeaseState.TokenLimit). 0 means 4096, the limit server.go leases at
	// dispatch; the gateway cuts a stream at 90% of it (ADR-0018 Pass 1).
	StepTokenLimit int
}

// SystemHarness runs a one-step plan through the REAL DAGExecutor and the REAL
// SubstrateLLMGateway, with a fake model and an in-memory registry behind them.
// Every step: resolve an executor from Registry, Gateway.Acquire(StepTokenLimit),
// Gateway.StreamChunks(FakeModelStreamer), Gateway.Complete. That is the shape
// of server.go's stepFn, so the gateway CONWIP and budget behaviour the
// integration suite asserts is the production code's, not a fake's.
type SystemHarness struct {
	Generator *FakeGenerator
	Streamer  *FakeModelStreamer
	Gateway   *subnetwork.SubstrateLLMGateway
	Registry  *HarnessRegistry
	Observer  *CapturingTelemetryObserver
	ExecCfg   config.ExecutionConfig

	tokenLimit int
	mu         sync.Mutex
	usages     []llm.TokenUsage
}

func New(cfg Config) *SystemHarness {
	exec := config.DefaultConfig().Execution
	if cfg.MaxConcurrentSessions > 0 {
		exec.LLM.LLMGatewayMaxConcurrency = cfg.MaxConcurrentSessions
	}
	exec.Plan.PlanTimeoutMs = 120000
	if cfg.StepTokenLimit == 0 {
		cfg.StepTokenLimit = 4096
	}

	gen := NewFakeGenerator()
	streamer := NewFakeModelStreamer(gen)
	obs := &CapturingTelemetryObserver{}

	gw := subnetwork.NewLLMGateway(exec)
	gw.Observer = obs
	gw.RegisterModelClient(FakeModelID, streamer)
	gw.SetDefaultModelID(FakeModelID)

	reg := NewHarnessRegistry()
	reg.Register(
		domain.AgentDefinition{ID: "test-agent", Trait: domain.TraitCognitive, Description: "harness executor"},
		domain.AgentManifest{SupportedFormats: []string{"text"}},
	)

	return &SystemHarness{
		Generator:  gen,
		Streamer:   streamer,
		Gateway:    gw,
		Registry:   reg,
		Observer:   obs,
		ExecCfg:    exec,
		tokenLimit: cfg.StepTokenLimit,
	}
}

// StepUsages returns the gateway-reconciled token usage of every completed
// step, in completion order.
func (h *SystemHarness) StepUsages() []llm.TokenUsage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]llm.TokenUsage(nil), h.usages...)
}

// ExecutePlan runs a one-step plan. The step is executed by the first agent the
// registry knows, generating through the gateway under a StepTokenLimit lease.
func (h *SystemHarness) ExecutePlan(ctx context.Context) (*domain.Handoff, []domain.TaskEvent, error) {
	fakeWriter := &fakeTaskEventWriter{}

	stepFn := executer.StepFunc(func(ctx context.Context, i int, handoff *domain.Handoff) (*domain.Handoff, error) {
		agents, err := h.Registry.GetAllAgents(ctx)
		if err != nil {
			return nil, fmt.Errorf("harness: registry: %w", err)
		}
		if len(agents) == 0 {
			return nil, fmt.Errorf("harness: no agent registered to execute step %d", i)
		}
		executor := agents[0]

		// Mirrors server.go: the lease is minted at dispatch with an empty
		// allocation, the gateway resolves the model, Complete runs on the way out.
		lease, err := h.Gateway.Acquire(ctx, domain.StepAllocation{}, h.tokenLimit, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("harness: acquire: %w", err)
		}
		defer func() {
			usage, cerr := h.Gateway.Complete(ctx, lease)
			if cerr == nil {
				h.mu.Lock()
				h.usages = append(h.usages, usage)
				h.mu.Unlock()
			}
		}()

		prompt := string(handoff.Payload.Data)
		// StreamChunks is synchronous and never closes out; size the buffer for
		// the whole response so the gateway cannot block on a slow reader.
		out := make(chan domain.StreamChunk, 4096)
		if err := h.Gateway.StreamChunks(ctx, lease, prompt, domain.GenerateOptions{}, out); err != nil {
			return nil, fmt.Errorf("harness: gateway: %w", err)
		}
		close(out)
		var chunks []domain.StreamChunk
		for c := range out {
			chunks = append(chunks, c)
		}

		return &domain.Handoff{
			ID:        "step-result",
			FromAgent: executor.ID,
			Payload:   &domain.Payload{Data: []byte(chunksText(chunks))},
		}, nil
	})

	dag := &executer.DAGExecutor{
		EventWriter:       fakeWriter,
		Observer:          h.Observer,
		LLMGateway:        h.Gateway,
		ThoughtFn:         stepFn,
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
	h.Generator.mu.Lock()
	defer h.Generator.mu.Unlock()
	h.Generator.responses = append(h.Generator.responses, resp)
}

type fakeTaskEventWriter struct {
	events []domain.TaskEvent
}

func (w *fakeTaskEventWriter) WriteTaskEvent(evt domain.TaskEvent) error {
	w.events = append(w.events, evt)
	return nil
}
