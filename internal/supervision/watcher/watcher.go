package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/supervision/signal"
)

// ProactivePlanApprovalKind is the ApprovalRequest.ToolName a signal-born plan is
// raised under. The operator plane's approval surface (WatchToolApprovals ->
// ApprovalOp.tool_name, and HITLRaisedOp.description on the feed) shows it as
// the thing being approved; ResolveHITL on the request id decides it.
const ProactivePlanApprovalKind = "proactive_plan"

// maxPlanPreview bounds the ArgsPreview of a proactive-plan approval. The preview
// is what the operator decides on, so it carries every step's query, but a
// runaway plan must not turn one approval into a megabyte feed event.
const maxPlanPreview = 4000

// ErrInvalidSignal is returned when a Handoff is not a valid signal.
var ErrInvalidSignal = errors.New("invalid signal")

// ErrSignalInhibited is returned when the circuit breaker trips.
var ErrSignalInhibited = errors.New("signal inhibited: neural noise detected")

// WatcherConfig holds tuning parameters for the Watcher.
type WatcherConfig struct {
	SignalNoiseThreshold  int
	SignalNoiseWindowSecs int
	// MaxPendingPlans bounds how many proactive plans may be awaiting an operator
	// decision or running at once. A plan that arrives when the bound is reached
	// is dropped with a warning rather than queued without limit: a chattering
	// daemon must not grow goroutines without bound (AGENTS.md §3.3). 0 => 4.
	MaxPendingPlans int
}

// MemoryFetcher is the consumer-side interface Watcher needs for LTM access.
type MemoryFetcher interface {
	FetchContext(ctx context.Context, query string) string
}

// ManagerAccess is the consumer-side interface Watcher needs from AgentManager.
// Structurally equivalent to signal.ManagerAccess — both are satisfied by *agentmgr.AgentManager.
type ManagerAccess interface {
	FindInstanceByToken(token string) *domain.Instance
	GetInstanceIDs(agentID string) []string
	EvictInstance(instanceID string)
}

// PlannerAccess is the consumer-side interface Watcher needs from the Planner.
type PlannerAccess interface {
	GetExecutionPlan(ctx context.Context, userInput string) (*domain.ExecutionPlan, error)
}

// Watcher listens on SignalStream, validates signals, enriches them with LTM
// context, and presents them as Inspirations to the Planner.
// In the OSS build, it is wired as the domain.SignalReceiver. ADR-0032.
//
// A plan born from a signal is never executed on the daemon's say-so (ADR-0009:
// agents cannot trigger plans directly). It is queued on the operator approval
// hub and runs through the kernel's one plan execution path only when an
// operator approves it. Both collaborators are constructor arguments; a Watcher
// built without them can plan but never execute, so a missing wire fails closed
// instead of auto-running daemon-initiated work (AGENTS.md §2.3).
type Watcher struct {
	Manager     ManagerAccess
	MemoryAgent MemoryFetcher
	Planner     PlannerAccess
	Config      WatcherConfig

	approval domain.ApprovalController
	executor domain.PlanExecutor
	// pending is a counting semaphore over plans awaiting a decision or running.
	pending chan struct{}

	cb *signal.CircuitBreaker

	replanningMu sync.RWMutex
	replanning   bool
}

// SetReplanning enables or disables signal processing suppression.
func (w *Watcher) SetReplanning(v bool) {
	w.replanningMu.Lock()
	defer w.replanningMu.Unlock()
	w.replanning = v
}

func (w *Watcher) isReplanning() bool {
	w.replanningMu.RLock()
	defer w.replanningMu.RUnlock()
	return w.replanning
}

// New creates a new Watcher with the given dependencies.
//
// approval is the operator approval hub a proactive plan waits on; executor runs
// an approved plan. Pass the operator-gated hub, never an auto-approving
// controller: tools_auto_approve is a decision about dangerous TOOLS, and reusing
// it here would silently turn every daemon signal into executed work. Either nil
// => plans are produced and dropped (logged), never run.
func New(manager ManagerAccess, memoryAgent MemoryFetcher, planner PlannerAccess,
	approval domain.ApprovalController, executor domain.PlanExecutor, cfg WatcherConfig) *Watcher {
	if cfg.SignalNoiseThreshold == 0 {
		cfg.SignalNoiseThreshold = 3
	}
	if cfg.SignalNoiseWindowSecs == 0 {
		cfg.SignalNoiseWindowSecs = 10
	}
	if cfg.MaxPendingPlans <= 0 {
		cfg.MaxPendingPlans = 4
	}
	return &Watcher{
		Manager:     manager,
		MemoryAgent: memoryAgent,
		Planner:     planner,
		Config:      cfg,
		approval:    approval,
		executor:    executor,
		pending:     make(chan struct{}, cfg.MaxPendingPlans),
		cb:          signal.NewCircuitBreaker(cfg.SignalNoiseThreshold, cfg.SignalNoiseWindowSecs),
	}
}

// ValidateSignal checks that a Handoff carries a valid _signal_type.
func (w *Watcher) ValidateSignal(ctx context.Context, handoff *domain.Handoff) error {
	if handoff.Context == nil {
		return ErrInvalidSignal
	}
	signalType := handoff.Context["_signal_type"]
	if signalType == "" {
		return fmt.Errorf("%w: _signal_type is missing or empty", ErrInvalidSignal)
	}
	return nil
}

// ValidateToken extracts the Bearer token from gRPC metadata and validates it.
// Delegates to signal.ValidateToken for the shared implementation.
func (w *Watcher) ValidateToken(ctx context.Context) (*domain.Instance, error) {
	return signal.ValidateToken(ctx, w.Manager)
}

// EnrichSignal fetches relevant LTM context for a signal.
func (w *Watcher) EnrichSignal(ctx context.Context, signalType, signalData string) string {
	if w.MemoryAgent == nil {
		return ""
	}
	query := fmt.Sprintf("%s %s", signalType, signalData)
	return w.MemoryAgent.FetchContext(ctx, query)
}

// BuildInspiration constructs an unstructured prompt from a signal.
func (w *Watcher) BuildInspiration(signalType, signalData, ltmContext string) string {
	var b strings.Builder
	b.WriteString("[SYSTEM SIGNAL RECEIVED]\n")
	b.WriteString(fmt.Sprintf("Signal: %s\n", signalType))
	if signalData != "" {
		b.WriteString(fmt.Sprintf("Data: %s\n", signalData))
	}
	if ltmContext != "" {
		b.WriteString(fmt.Sprintf("Memory Context: %s\n", ltmContext))
	}
	b.WriteString("\nAnalyze this signal and decide whether to generate an execution plan. If no action is warranted, respond with an empty plan.")
	return b.String()
}

// ProcessSignal enriches a signal with LTM context, presents it to the Planner,
// and returns the resulting ExecutionPlan.
func (w *Watcher) ProcessSignal(ctx context.Context, signalType, signalData, ltmContext string) (*domain.ExecutionPlan, error) {
	if w.isReplanning() {
		return nil, nil
	}
	prompt := w.BuildInspiration(signalType, signalData, ltmContext)
	if w.Planner == nil {
		return nil, nil
	}
	return w.Planner.GetExecutionPlan(ctx, prompt)
}

// RecordInvalidSignal records a failed signal attempt for circuit breaker tracking.
func (w *Watcher) RecordInvalidSignal(agentID string) {
	w.cb.RecordInvalidSignal(agentID)
}

// recordSignalAt records a signal at a specific time (test helper).
func (w *Watcher) recordSignalAt(agentID string, at time.Time) {
	w.cb.RecordAt(agentID, at)
}

// InvalidSignalCount returns the number of invalid signals for an agent within the window.
func (w *Watcher) InvalidSignalCount(agentID string) int {
	return w.cb.Count(agentID)
}

// ResetInvalidSignals clears the invalid signal counter for an agent.
func (w *Watcher) ResetInvalidSignals(agentID string) {
	w.cb.ResetInvalidSignals(agentID)
}

// ShouldInhibit returns true if the agent has exceeded the noise threshold.
func (w *Watcher) ShouldInhibit(agentID string) bool {
	return w.cb.ShouldInhibit(agentID)
}

// HandleInvalidSignal records an invalid signal and returns ErrSignalInhibited
// when the circuit breaker trips.
func (w *Watcher) HandleInvalidSignal(ctx context.Context, handoff *domain.Handoff) error {
	agentID := handoff.FromAgent
	w.RecordInvalidSignal(agentID)
	if w.ShouldInhibit(agentID) {
		w.ResetInvalidSignals(agentID)
		return fmt.Errorf("%w: agent %s exceeded noise threshold", ErrSignalInhibited, agentID)
	}
	return fmt.Errorf("%w: invalid signal from %s", ErrInvalidSignal, agentID)
}

// OnSignal implements domain.SignalReceiver for the OSS Watcher (ADR-0032).
//
// It is the ONE place a signal is planned. SignalStream gatekeeps first (signal
// shape, daemon token, circuit breaker) and then hands the signal here. It used
// to also call ProcessSignal itself, so every OSS signal cost two planner calls
// and both plans were discarded.
//
// A non-empty plan is queued for operator approval and OnSignal returns at once:
// the decision can take the hub's whole grace window and the plan longer still,
// and OnSignal runs on the daemon's stream loop, which must keep receiving.
func (w *Watcher) OnSignal(ctx context.Context, s domain.Signal) error {
	plan, err := w.ProcessSignal(ctx, s.StreamID, s.RawText, w.EnrichSignal(ctx, s.StreamID, s.RawText))
	if err != nil {
		return err
	}
	if plan == nil || len(plan.Steps) == 0 {
		return nil
	}
	w.queueForApproval(ctx, s, plan)
	return nil
}

// queueForApproval raises plan on the operator approval hub and, if an operator
// approves it, runs it through the kernel's plan executor (the Orchestrator
// Execute authored-plan path, contract 0074, so session binding, run
// persistence, checkpoints and the feed are the existing code).
//
// The hub is fail-closed: no operator watching, the grace window elapsing, or a
// rejection all drop the plan. That is the "queue for operator approval"
// semantics until a durable pending-plan surface exists: daemon-initiated work
// never runs without a human saying so.
func (w *Watcher) queueForApproval(ctx context.Context, s domain.Signal, plan *domain.ExecutionPlan) {
	if w.approval == nil || w.executor == nil {
		slog.Warn("watcher: proactive plan dropped: no operator approval path wired",
			"signal", s.StreamID, "from", s.FromAgent, "steps", len(plan.Steps))
		return
	}
	select {
	case w.pending <- struct{}{}:
	default:
		slog.Warn("watcher: proactive plan dropped: too many plans pending approval",
			"signal", s.StreamID, "from", s.FromAgent, "max_pending", cap(w.pending))
		return
	}

	// The approval wait and the run outlive this signal's stream, so cancellation
	// is dropped while values (trace) are kept (AGENTS.md §2.7). The daemon's
	// incoming gRPC metadata (its bearer token) is replaced with an empty set: an
	// approved plan is operator-sanctioned work and must not execute under
	// anything the daemon sent.
	runCtx := metadata.NewIncomingContext(context.WithoutCancel(ctx), metadata.MD{})
	req := domain.ApprovalRequest{
		AgentID:     s.FromAgent,
		ToolName:    ProactivePlanApprovalKind,
		ArgsPreview: planPreview(s, plan),
	}
	go func() {
		defer func() { <-w.pending }()
		dec, err := w.approval.Request(runCtx, req)
		if err != nil || !dec.Approved {
			slog.Info("watcher: proactive plan not approved; dropped",
				"signal", s.StreamID, "from", s.FromAgent, "err", err)
			return
		}
		slog.Info("watcher: proactive plan approved; executing",
			"signal", s.StreamID, "from", s.FromAgent, "approver", dec.ApproverID, "steps", len(plan.Steps))
		h := &domain.Handoff{
			FromAgent: s.FromAgent,
			Payload:   &domain.Payload{Data: []byte(plan.Subject)},
		}
		if _, err := w.executor.Execute(runCtx, plan, h); err != nil {
			slog.Warn("watcher: approved proactive plan failed",
				"signal", s.StreamID, "from", s.FromAgent, "err", err)
		}
	}()
}

// planPreview renders what the operator is approving: the triggering signal and
// every step's query, bounded by maxPlanPreview.
func planPreview(s domain.Signal, plan *domain.ExecutionPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "signal %s from %s", s.StreamID, s.FromAgent)
	if plan.Subject != "" {
		fmt.Fprintf(&b, "; subject: %s", plan.Subject)
	}
	for i, st := range plan.Steps {
		fmt.Fprintf(&b, "\n%d. %s", i+1, st.Query)
	}
	out := b.String()
	if len(out) > maxPlanPreview {
		out = out[:maxPlanPreview] + "..."
	}
	return out
}
