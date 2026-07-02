package app

import (
	"context"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	subnetwork "github.com/cambrian-sh/cambrian-runtime/internal/substrate/network"
)

// Options carries the injection hooks the composition root (Run / bootstrapKernel)
// uses to wire optional or proprietary components. OSS defaults are inert (no
// tracing, no agent-call logging, no reactive engine); a downstream (premium)
// binary supplies real implementations and reuses the same bootstrap. ADR-0057 (Model C).
//
// This is the OSS-exported extension surface. Keep it minimal — additions here are
// public API.
type Options struct {
	// TraceWrapper wraps every acquired generator at the Provider's Acquire
	// chokepoint (ADR-0042). OSS default: identity. Premium: a Langfuse wrapper.
	// A nil value disables wrapping.
	TraceWrapper func(g domain.Generator, subsystem string) domain.Generator

	// AgentCallLogger records agent-initiated LLM calls (GenerateViaModelStream).
	// OSS default: nil (the call site nil-checks it). Premium: a Langfuse logger.
	AgentCallLogger subnetwork.AgentCallLogger

	// NewSignalReceiver, when non-nil, builds the reactive SignalReceiver (+ watch
	// CRUD handler) from the OSS capability bundle. OSS default: nil → the Watcher is
	// used (LTM enrichment + Planner dispatch). The premium binary injects a function
	// that constructs the ReactiveEngine. ADR-0032 / ADR-0057.
	NewSignalReceiver func(ReactiveServices) (domain.SignalReceiver, domain.WatchConfigHandler)
}

// ReactiveServices is the OSS-provided capability bundle handed to the premium
// reactive hook. Every field is an interface — premium depends on these, never on
// the kernel stacks. This is the spike-validated seam (ADR-0057 D14): the reactive
// engine + executors + watch handler are buildable from this bundle alone.
type ReactiveServices struct {
	Manager    ReactiveAgentDispatcher // direct dispatch + daemon lifecycle
	Auctioneer domain.Auctioneer       // full Gatekeeper → Auction
	Memory     ReactiveMemoryWriter    // async LTM ingest
	Planner    ReactivePlanner         // plan generation for start_plan actions
	LLM        domain.Generator        // LLM condition evaluation
	WatchStore ReactiveWatchStore      // WatchConfig persistence (BBolt)
	EventBus   domain.EventBus         // daemon-crash subscription, emit_event
}

// ReactiveAgentDispatcher is the agent-manager surface reactive needs:
// direct dispatch (DirectDispatcher) + daemon lifecycle (DaemonLifecycle).
type ReactiveAgentDispatcher interface {
	CallAgent(ctx context.Context, agentID string, h *domain.Handoff) (*domain.Handoff, error)
	SpawnDaemon(agentID, streamID string, params map[string]any) (instanceID string, err error)
	StopDaemon(streamID string) error
}

// ReactiveMemoryWriter ingests signal content into LTM asynchronously.
type ReactiveMemoryWriter interface {
	ProcessAndStoreAsync(ctx context.Context, text string, sourceAgent string)
}

// ReactivePlanner generates an execution plan for start_plan actions.
type ReactivePlanner interface {
	GetExecutionPlan(ctx context.Context, input string) (*domain.ExecutionPlan, error)
}

// ReactiveWatchStore is the WatchConfig persistence surface (satisfied by the BBolt
// AgentRepoDecorator).
type ReactiveWatchStore interface {
	WriteWatchConfig(cfg domain.WatchConfig) error
	ReadWatchConfig(id string) (domain.WatchConfig, error)
	ReadAllWatchConfigs() ([]domain.WatchConfig, error)
	DeleteWatchConfig(id string) error
	SetWatchConfigActive(id string, active bool) error
}

// DefaultOptions returns the OSS defaults: identity trace wrapper, no agent-call
// logging, no reactive engine (the Watcher is used). Premium overrides these.
func DefaultOptions() Options {
	return Options{
		TraceWrapper: func(g domain.Generator, _ string) domain.Generator { return g },
	}
}
