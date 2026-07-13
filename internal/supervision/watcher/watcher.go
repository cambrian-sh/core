package watcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/supervision/signal"
)

// ErrInvalidSignal is returned when a Handoff is not a valid signal.
var ErrInvalidSignal = errors.New("invalid signal")

// ErrSignalInhibited is returned when the circuit breaker trips.
var ErrSignalInhibited = errors.New("signal inhibited: neural noise detected")

// WatcherConfig holds tuning parameters for the Watcher.
type WatcherConfig struct {
	SignalNoiseThreshold  int
	SignalNoiseWindowSecs int
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
type Watcher struct {
	Manager     ManagerAccess
	MemoryAgent MemoryFetcher
	Planner     PlannerAccess
	Config      WatcherConfig

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
func New(manager ManagerAccess, memoryAgent MemoryFetcher, planner PlannerAccess, cfg WatcherConfig) *Watcher {
	if cfg.SignalNoiseThreshold == 0 {
		cfg.SignalNoiseThreshold = 3
	}
	if cfg.SignalNoiseWindowSecs == 0 {
		cfg.SignalNoiseWindowSecs = 10
	}
	return &Watcher{
		Manager:     manager,
		MemoryAgent: memoryAgent,
		Planner:     planner,
		Config:      cfg,
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

// GenerateAuthToken produces a random hex token suitable for Instance authentication.
// Delegates to signal.GenerateAuthToken.
func GenerateAuthToken() (string, error) {
	return signal.GenerateAuthToken()
}

// OnSignal implements domain.SignalReceiver for the OSS Watcher.
// It validates the signal, enriches with LTM, and presents it to the Planner.
// Equivalent to the SignalStream handler's per-signal processing. ADR-0032.
func (w *Watcher) OnSignal(ctx context.Context, s domain.Signal) error {
	if w.isReplanning() {
		return nil
	}
	if w.Planner == nil {
		return nil
	}
	ltmCtx := w.EnrichSignal(ctx, s.StreamID, s.RawText)
	_, err := w.Planner.GetExecutionPlan(ctx, w.BuildInspiration(s.StreamID, s.RawText, ltmCtx))
	return err
}
