package executer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/scope"
)

// ArtifactStepLister lists artifacts produced by a session+step (ADR-0034). Used to
// surface prior-step artifacts into working_memory. Optional.
type ArtifactStepLister interface {
	ListStepArtifacts(sessionID string, stepIndex int) ([]domain.Artifact, error)
}

// SessionCallerScopes returns the non-forgeable caller_scope for a session, used to
// scope-filter surfaced artifacts (ADR-0034 Phase 2). Optional.
type SessionCallerScopes interface {
	CallerScope(ctx context.Context, sessionID string) domain.ScopeConfig
}

// TaskEventWriter is the interface that BBoltAdapter (and test mocks) implement
// to persist TaskEvent records after each DAG step completes.
// The EventWriter field on DAGExecutor may be nil — all writes are best-effort.
type TaskEventWriter interface {
	WriteTaskEvent(event domain.TaskEvent) error
}

// LLMGateway is the narrow consumer-side interface DAGExecutor needs for
// session-token lifecycle management (ADR-0018). Nil = zero behaviour change.
type LLMGateway interface {
	Acquire(ctx context.Context, sa domain.StepAllocation, tokenLimit int, estimatedDuration time.Duration) (string, error)
	Complete(ctx context.Context, sessionID string) (llm.TokenUsage, error)
}

// ContextGrowthPenalty returns k * growthBytes as a float64 penalty value.
// It is a thin wrapper around domain.ContextGrowthPenalty for package-local callers.
func ContextGrowthPenalty(growthBytes int, k float64) float64 {
	return domain.ContextGrowthPenalty(growthBytes, k)
}

// contextByteSize returns the sum of len(k)+len(v) for all entries in ctx.
// A nil map returns 0.
func contextByteSize(ctx map[string]string) int {
	total := 0
	for k, v := range ctx {
		total += len(k) + len(v)
	}
	return total
}

// StepFunc is the per-step execution callback injected into DAGExecutor.
// It receives the step index and a fully-constructed Handoff (with context
// snapshot) and returns the agent's response. Decoupling traversal from
// per-node execution makes DAGExecutor testable without gRPC or a live LLM.
type StepFunc func(ctx context.Context, stepIndex int, handoff *domain.Handoff) (*domain.Handoff, error)

// finalResultKey is the master-context key under which DAGExecutor stores the
// result of the last topologically ordered step, for Server.Execute to return
// as the response payload. It is an internal convention; callers must delete it
// before exposing the context to external consumers.
const finalResultKey = "_dag_final_result"

// EnqueueVerification is an optional callback invoked after each successful step
// so the VerificationWorker can decide whether to sample the task. It never
// blocks — the implementation must perform any queuing asynchronously.
// A nil value disables verification sampling for this executor.
type EnqueueVerification func(taskID, agentID string, req, resp *domain.Handoff)

// Create checkpoint stores a snapshot of the master context after a successful
// step merge. It is a lightweight in-memory record; persistence is handled
// separately via the CheckpointStore interface.
type ContextCheckpoint struct {
	PlanID    string
	StepIndex int
	Context   map[string]string
	Timestamp time.Time
}

// CheckpointStore persists context checkpoints for crash recovery.
// The substrate calls SaveCheckpoint after each successful step merge.
type CheckpointStore interface {
	SaveCheckpoint(sessionID, planID string, stepIndex int, ctx map[string]string) error
	LoadCheckpoint(sessionID, planID string, stepIndex int) (map[string]string, error)
	ListCheckpoints(sessionID string) ([]CheckpointMeta, error)
}

// CheckpointMeta describes a persisted checkpoint.
type CheckpointMeta struct {
	SessionID string
	PlanID    string
	StepIndex int
	Timestamp time.Time
}

// DAGExecutor traverses an ExecutionPlan as a DAG, dispatching independent
// steps concurrently and accumulating results into a shared context map.
//
// EventWriter, if non-nil, receives a TaskEvent after each step completes
// successfully. The write is best-effort: errors are logged via slog.Warn
// and never propagate to the step caller.
//
// EnqueueVerification, if non-nil, is called after each successful step to
// allow the VerificationWorker to sample task completions for quality scoring.
//
// MemoryRecorder, if non-nil, is called after each successful step merge to
// feed the Tier-1 pending channel (ADR-0015). Write is non-blocking.
type DAGExecutor struct {
	EventWriter         TaskEventWriter        // may be nil
	EnqueueVerification EnqueueVerification    // may be nil
	MemoryRecorder      domain.MemoryRecorder // ADR-0015: may be nil
	WorkspaceStage      domain.WorkspaceStage // ADR-0016: may be nil; nil disables GWS enrichment
	LLMGateway          LLMGateway            // ADR-0018: may be nil
	Observer            domain.TelemetryObserver // ADR-0019: may be nil
	PlanEventWriter     domain.PlanEventWriter // ADR-0021: may be nil; nil disables plan-level telemetry
	EventBus            domain.EventBus        // ADR-0047 D7/0047-17: may be nil; publishes PlanStateChanged for the operator feed
	InjectPlanner       InjectPlanner          // ADR-0047 A1.1: may be nil; nil ⇒ deterministic instruction-as-step default
	ThoughtFn           StepFunc              // used for IsThought=true steps; may be nil
	CheckpointValidator domain.CheckpointValidator // ADR-0013 H1 gate; may be nil (nil disables checkpoints)
	PauseController     *PauseController    // optional; nil disables HITL pausing
	ReplanHandler       ReplanHandler       // optional; nil disables plan-level replan
	CheckpointStore     CheckpointStore     // optional; nil disables checkpointing
	ContentStore        domain.ContentStore // ADR-0022 Phase 1: may be nil; nil disables CAS
	SceneWriter         domain.SceneWriter  // ADR-0025: may be nil; nil disables scene writing
	StepCache           domain.StepCache        // ADR-0026: may be nil; nil disables step-level memoization
	StepCachePolicies   map[string]int          // ADR-0026: operator TTL overrides; nil = use heuristic defaults
	ContextRefSnippetChars int              // ADR-0022: snippet length; 0 defaults to 500
	// ADR-0022 Phase 3: circuit-breaker flag.
	// true  → PrimeForStep called per step; Handoff.WorkingMemory populated; Handoff.Context empty.
	// false → filterSnapshotForStep into Handoff.Context; PrimeForStep NOT called (Phase 0 behavior).
	// Default false for backward compat; set true after Phase 3 validation passes.
	UseGlobalWorkspace bool
	MaxContextSlots    int // ADR-0022: hard ceiling for PrimeForStep; 0 defaults to 20
	StepFactCosineThreshold float64 // AGENTCONTEXTREQ REQ2: min cosine for forwarding a planning fact to a step (default 0.55)
	MaxReplanAttempts   int                 // max replan attempts; 0 disables replan
	MaxPlanCost           float64             // total plan budget; 0 disables budget enforcement
	DefaultInputCostPer1M float64             // cost per 1M input tokens for cost estimation
	DefaultOutputCostPer1M float64            // cost per 1M output tokens for cost estimation
	CurrentSessionID      string              // session scope for checkpoints; empty = no session
	CheckpointFlushSecs   int                 // periodic flush interval in seconds (0 = only on pause)

	// ADR-0034 / REQ-SDK-007b: optional artifact discovery in working_memory.
	// When both are set, prior-step artifacts are surfaced (scope-filtered) into
	// the step's working memory as ContextRefs. Best-effort discovery — the
	// authoritative gate remains GetArtifact (agent_scope). nil → disabled.
	ArtifactLister ArtifactStepLister
	SessionScopes  SessionCallerScopes

	// replanSuppressor is notified when replanning starts/ends to suppress
	// duplicate signals (usually set to the Watcher).
	replanSuppressor interface{ SetReplanning(bool) }
	completedMu       sync.Mutex
	completedIdx      []int

	// Per-execution pause state (set during Execute, accessed by Pause/Resume/HotSwap).
	pausedMu         sync.Mutex
	paused           bool
	resumeCh         chan struct{}
	hotSwapPlan      *domain.ExecutionPlan
	pendingInjection string // ADR-0047 A1.1: operator instruction folded in on resume
	replanCount      int
	errorPause       bool // true when pause was triggered by step error (auto-replan)
}

// ReplanCount returns how many times this execution replanned. Read after
// ExecuteFrom returns; used by ROUTE-08 phase-A scout-usefulness logging (a plan
// that ran without replan is the "discovery was sufficient" signal).
func (d *DAGExecutor) ReplanCount() int { return d.replanCount }

// InjectPlanner composes the new forward plan for an operator InjectCorrection
// from the instruction plus the live execution state (ADR-0047 A1.1). Optional:
// when nil, the executor uses a deterministic default — a single-step forward
// plan carrying the instruction, routed by the normal auction (Zero-Hardcode),
// with prior results available via masterContext. completed is the set of
// already-finished step indices.
type InjectPlanner func(instruction string, ctx map[string]string, plan *domain.ExecutionPlan, completed []int) (*domain.ExecutionPlan, error)

// stepResult carries the outcome of one goroutine back to the coordinator.
type stepResult struct {
	index             int
	resp              *domain.Handoff
	err               error
	estimatedCost     float64
	promptTokens      int
	completionTokens  int
	totalTokens       int
	budgetOverrun     bool
	fallbackModelUsed bool
	snapshot          map[string]string // masterContext clone at dispatch time (ADR-0015)
}

// executeStep runs a single plan step synchronously. Callers wrap it in a
// goroutine with wg tracking. Handles HITL gating, thought/agent dispatch,
// telemetry logging, TaskEvent persistence, and completion tracking.
func (d *DAGExecutor) executeStep(
	ctx context.Context,
	plan *domain.ExecutionPlan,
	stepIndex int,
	snapshot map[string]string,
	workingMemory []domain.ContextRef,
	stepFn StepFunc,
	planID string,
) stepResult {
	sizeBefore := contextByteSize(snapshot)

	slog.Info("🚀 Dispatching Step", "index", stepIndex)

	if d.PauseController != nil && d.PauseController.NeedsIntervention(plan.Steps[stepIndex].Query) {
		// ADR-0047 0047-19: surface the intervention on the operator feed. This
		// destructive-step pause is resolved via PauseSession/ResumeSession on the
		// control hub (the dangerous-tool path pairs with ResolveHITL separately).
		if d.EventBus != nil {
			_ = d.EventBus.Publish(domain.HITLRaisedEvent{
				InterventionID: fmt.Sprintf("%s:%d", planID, stepIndex),
				SessionID:      d.CurrentSessionID,
				Description:    plan.Steps[stepIndex].Query,
				IsDestructive:  true,
			})
		}
		d.PauseController.Pause()
		d.PauseController.Wait()
		if d.PauseController.IsAborted() {
			return stepResult{index: stepIndex, err: fmt.Errorf("plan aborted by user at step %d", stepIndex)}
		}
	}

	// ADR-0026: step-level cache lookup. On hit, skip stepFn entirely.
	var cacheKey string
	if d.StepCache != nil {
		cacheKey = stepCacheKey(plan.Subject, planID, plan.Steps[stepIndex], snapshot)
		if cached, ok, getErr := d.StepCache.Get(ctx, cacheKey); getErr != nil {
			slog.Warn("StepCache.Get failed, falling through to live execution", "step", stepIndex, "err", getErr)
		} else if ok && cached != nil {
			cached.SessionToken = nil // stale session tokens must not be restored from cache
			slog.Info("⚡ Step served from cache", "index", stepIndex)
			return stepResult{index: stepIndex, resp: cached}
		}
	}

	var resp *domain.Handoff
	var stepErr error

	// ADR-0022: WorkingMemory and Context are mutually exclusive.
	// When WorkingMemory is non-nil (use_global_workspace=true), Context is nil.
	// ADR-0049 D3: hand the agent its per-step correlation key so its tool calls
	// (actions) get stamped with it; the same key is set on the StepResult below, so
	// RecordExecution can count the step's actions and dedup its prose synthesis.
	if snapshot == nil {
		snapshot = map[string]string{}
	}
	snapshot["_task_id"] = formatTaskID(stepIndex, planID)
	snapshot["_step_index"] = strconv.Itoa(stepIndex)

	stepHandoff := &domain.Handoff{
		Payload:       &domain.Payload{Data: []byte(plan.Steps[stepIndex].Query)},
		Context:       snapshot,
		WorkingMemory: workingMemory,
	}
	if plan.Steps[stepIndex].IsThought && d.ThoughtFn != nil {
		resp, stepErr = d.ThoughtFn(ctx, stepIndex, stepHandoff)
	} else {
		resp, stepErr = stepFn(ctx, stepIndex, stepHandoff)
	}

	if stepErr != nil {
		return stepResult{index: stepIndex, err: stepErr}
	}
	if resp == nil {
		return stepResult{index: stepIndex, resp: nil, err: nil}
	}

	sizeAfter := contextByteSize(resp.Context)
	growthBytes := sizeAfter - sizeBefore
	if growthBytes < 0 {
		growthBytes = 0
	}
	agentID := resp.FromAgent
	if plan.Steps[stepIndex].IsThought {
		agentID = "System_Thought"
	}

	slog.Info("✅ Step Completed", "index", stepIndex, "agent", agentID)

	d.completedMu.Lock()
	d.completedIdx = append(d.completedIdx, stepIndex)
	d.completedMu.Unlock()

	taskID := formatTaskID(stepIndex, planID)
	promptTokens := llm.EstimateTokens(plan.Steps[stepIndex].Query)
	respText := ""
	if resp.Payload != nil {
		respText = string(resp.Payload.Data)
	}
	responseTokens := llm.EstimateTokens(respText)
	estimatedCost := llm.EstimateCostFromText(plan.Steps[stepIndex].Query, respText,
		d.DefaultInputCostPer1M, d.DefaultOutputCostPer1M)

	if d.EventWriter != nil || d.Observer != nil {
		event := domain.TaskEvent{
			TaskID:             taskID,
			AgentID:            agentID,
			BidConfidence:      float64(resp.Confidence),
			ContextGrowthBytes: growthBytes,
			PromptTokens:       promptTokens,
			CompletionTokens:   responseTokens,
			TotalTokens:        promptTokens + responseTokens,
			EstimatedCost:      estimatedCost,
		}
		if d.EventWriter != nil {
			if err := d.EventWriter.WriteTaskEvent(event); err != nil {
				slog.Warn("DAGExecutor: failed to write TaskEvent", "step", stepIndex, "err", err)
			}
		}
		if d.Observer != nil {
			d.Observer.OnTaskCompleted(event)
		}
	}
	// Skip verification for budget-signal responses — the agent refused the task
	// on cost grounds, not quality grounds. Verifying would incorrectly penalise
	// the agent's TrustScore. ADR-0023 Fix 5.
	isBudgetSignal := resp.Payload != nil && resp.Payload.Type == "budget_signal"
	if d.EnqueueVerification != nil && !isBudgetSignal {
		d.EnqueueVerification(taskID, agentID, &domain.Handoff{
			Payload: &domain.Payload{Data: []byte(plan.Steps[stepIndex].Query)},
			Context: snapshot,
		}, resp)
	}

	// ADR-0026: cache the successful result for future steps with identical inputs.
	if d.StepCache != nil && resp != nil && cacheKey != "" {
		if ttl := resolveCacheTTL(plan.Steps[stepIndex], d.StepCachePolicies); ttl > 0 {
			if putErr := d.StepCache.Put(ctx, cacheKey, resp, ttl); putErr != nil {
				slog.Warn("StepCache.Put failed", "step", stepIndex, "err", putErr)
			}
		}
	}

	return stepResult{
		index:             stepIndex,
		resp:              resp,
		err:               nil,
		estimatedCost:     estimatedCost,
		promptTokens:      promptTokens,
		completionTokens:  responseTokens,
		totalTokens:       promptTokens + responseTokens,
		snapshot:          snapshot,
	}
}

// mergeStepResult merges a successful step's response into the master context
// under the caller's lock. It must be called with mu already held.
//
// Phase 1 (ADR-0022): when store is non-nil, the step result is written to CAS
// and step_N_cid is added to masterContext alongside the existing step_N_result.
// store may be nil — in that case behaviour is identical to the pre-Phase-1 form.
// snippetChars controls the inline snippet length; 0 uses 500 as default.
func mergeStepResult(ctx context.Context, masterContext map[string]string, r stepResult, store domain.ContentStore, snippetChars int) domain.CID {
	if r.resp == nil {
		return ""
	}
	// Agent-added context keys always go into masterContext (unchanged from pre-Phase-1).
	// They are intentionally NOT stored in CAS — only the step result payload is CAS'd.
	for k, v := range r.resp.Context {
		masterContext[fmt.Sprintf("step_%d_%s", r.index, k)] = v
	}
	if r.resp.Payload == nil {
		return ""
	}
	data := r.resp.Payload.Data
	masterContext[fmt.Sprintf("step_%d_result", r.index)] = string(data)

	if store == nil {
		return ""
	}
	if snippetChars <= 0 {
		snippetChars = 500
	}
	snippet := buildSnippet(data, snippetChars)
	labels := []string{fmt.Sprintf("step_%d", r.index), "result"}
	cid, err := store.Put(ctx, data, "step_result", labels, snippet)
	if err != nil {
		slog.Warn("mergeStepResult CAS write failed", "step", r.index, "err", err)
		return ""
	}
	masterContext[fmt.Sprintf("step_%d_cid", r.index)] = string(cid)
	return cid
}

// buildSnippet returns the first snippetChars of data as a string.
// Returns "" for non-UTF-8 content (binary payloads) — a garbled snippet
// is worse than no snippet (would inject corrupt bytes into agent prompts).
func buildSnippet(data []byte, snippetChars int) string {
	if !utf8.Valid(data) {
		return ""
	}
	s := string(data)
	if len(s) <= snippetChars {
		return s
	}
	// Truncate at a rune boundary.
	i := 0
	for count := 0; count < snippetChars && i < len(s); count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

// applyReplannedPlan swaps in a new plan after replan/hot-swap. It re-runs
// topological sort, resets dispatch state, and clears the error flag.
// The caller must hold d.pausedMu.
func (d *DAGExecutor) applyReplannedPlan(newPlan *domain.ExecutionPlan, plan **domain.ExecutionPlan, n *int, alreadyDispatched map[int]bool, completed map[int]bool, topoOrder *[]int) error {
	*plan = newPlan
	*n = len((*plan).Steps)
	var err error
	*topoOrder, err = TopologicalSort((*plan).Steps)
	if err != nil {
		return err
	}
	for k := range alreadyDispatched {
		delete(alreadyDispatched, k)
	}
	for k := range completed {
		delete(completed, k)
	}
	return nil
}

// Pause stops dispatching new steps. In-flight goroutines complete normally.
// Safe to call from any goroutine concurrently with Execute.
func (d *DAGExecutor) Pause() {
	d.pausedMu.Lock()
	defer d.pausedMu.Unlock()
	d.paused = true
}

// Resume signals the paused coordinator to check for a hot-swapped plan and
// resume dispatching. Does not unset paused — the coordinator handles that
// when it applies the hot-swap and re-dispatches.
// Safe to call from any goroutine concurrently with Execute.
func (d *DAGExecutor) Resume() {
	d.pausedMu.Lock()
	ch := d.resumeCh
	d.pausedMu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// publishPlanState emits a PlanStateChanged on the operator feed (ADR-0047
// D7/0047-17). Best-effort and nil-safe. MUST be called only from the Execute
// coordinator goroutine so the global feed order preserves this plan's causal
// order (one-timeline-one-publisher, D4).
func (d *DAGExecutor) publishPlanState(planID, status string, activeStep int, cost float64, terminal bool) {
	if d.EventBus == nil {
		return
	}
	_ = d.EventBus.Publish(domain.PlanStateChanged{
		SessionID:  d.CurrentSessionID,
		PlanID:     planID,
		ActiveStep: activeStep,
		Status:     status,
		CostSoFar:  cost,
		Terminal:   terminal,
	})
}

// buildInjectionPlan composes the new forward plan for an operator injection.
// Uses InjectPlanner when set; otherwise the deterministic default — a single
// NL step carrying the instruction (routed by the auction; prior step results
// remain available via masterContext). Called only from the coordinator.
func (d *DAGExecutor) buildInjectionPlan(instruction string, ctx map[string]string, plan *domain.ExecutionPlan, completed []int) *domain.ExecutionPlan {
	if d.InjectPlanner != nil {
		if p, err := d.InjectPlanner(instruction, ctx, plan, completed); err == nil && p != nil && len(p.Steps) > 0 {
			return p
		}
		slog.Warn("InjectPlanner failed or empty; using deterministic injection default")
	}
	subject := ""
	if plan != nil {
		subject = plan.Subject
	}
	return &domain.ExecutionPlan{
		Subject: subject,
		Steps:   []domain.Step{{Query: instruction}},
	}
}

// Inject delivers an operator instruction into the running plan (ADR-0047
// A1.1). It records the instruction, pauses the coordinator, and signals resume;
// the coordinator folds it into a new forward plan (InjectPlanner or the
// deterministic default) and hot-swaps — so the executor, which holds the live
// masterContext and completed set, owns the mechanism. Safe to call from any
// goroutine. Returns an error only for an empty instruction.
func (d *DAGExecutor) Inject(instruction string) error {
	if strings.TrimSpace(instruction) == "" {
		return fmt.Errorf("inject: empty instruction")
	}
	d.pausedMu.Lock()
	d.pendingInjection = instruction
	d.pausedMu.Unlock()
	d.Pause()
	d.Resume()
	return nil
}

// HotSwap replaces the current plan. The new plan takes effect when the
// coordinator resumes after Pause/Resume. Safe to call from any goroutine
// concurrently with Execute.
func (d *DAGExecutor) HotSwap(newPlan *domain.ExecutionPlan) {
	d.pausedMu.Lock()
	defer d.pausedMu.Unlock()
	d.hotSwapPlan = newPlan
	d.replanCount++
}

// Execute runs all steps in plan, dispatching independent steps as goroutines
// the moment their declared predecessors (DependsOn) have completed.
//
// Concurrency contract:
//   - Each goroutine receives a snapshot copy of the master context at dispatch
//     time; it never writes to the master context directly.
//   - The coordinator merges results under a mutex after each goroutine returns.
//   - On first error cancel() is called; sync.WaitGroup drains unconditionally
//     before Execute returns — no goroutine leaks regardless of failure mode.
//   - masterContext["step_{i}_result"]   = resp.Payload.Data for step i
//   - masterContext["step_{i}_{k}"]      = resp.Context[k]   for step i
//   - masterContext[finalResultKey]       = result of last step in topo order
func (d *DAGExecutor) Execute(
	ctx context.Context,
	plan *domain.ExecutionPlan,
	initialContext map[string]string,
	stepFn StepFunc,
) (map[string]string, error) {
	return d.ExecuteFrom(ctx, plan, initialContext, stepFn, 0)
}

// ExecuteFrom runs the plan starting from the given step index (0 = fresh).
// Steps 0..startFromStepIndex-1 are treated as already completed. Their
// results are expected to be in initialContext under "step_{i}_result" keys.
func (d *DAGExecutor) ExecuteFrom(
	ctx context.Context,
	plan *domain.ExecutionPlan,
	initialContext map[string]string,
	stepFn StepFunc,
	startFromStepIndex int,
) (finalCtx map[string]string, retErr error) {
	n := len(plan.Steps)
	if n == 0 {
		return cloneMap(initialContext), nil
	}

	// ADR-0022 Phase 1: collect CIDs for plan-scoped GC.
	// defer ensures GC fires even on panic — orphaned CIDs accumulate otherwise.
	var planCIDs []domain.CID
	if d.ContentStore != nil {
		defer func() {
			if err := d.ContentStore.GC(context.Background(), planCIDs); err != nil {
				slog.Warn("ContentStore GC failed", "err", err)
			}
		}()
	}

	// Initialize per-execution pause state.
	d.pausedMu.Lock()
	d.paused = false
	d.resumeCh = make(chan struct{}, 1)
	d.hotSwapPlan = nil
	d.replanCount = 0
	d.errorPause = false
	d.pausedMu.Unlock()

	// planID is generated once per Execute call. TaskIDs are derived as
	// "step-{index}-{planID}" so every step in this execution has a stable,
	// unique, traceable audit key regardless of timing or retries.
	// A new planID on each call means a Planner retry produces a distinct audit scope.
	planID := newPlanID()

	// Reset completed index tracker so each Execute call starts clean.
	d.completedMu.Lock()
	d.completedIdx = d.completedIdx[:0]
	d.completedMu.Unlock()

	topoOrder, err := TopologicalSort(plan.Steps)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	// Buffered with n: every goroutine sends exactly one result, never blocks.
	results := make(chan stepResult, n)

	var mu sync.Mutex
	// ADR-0016: Enrich initial context with cross-session LTM facts before cloneMap.
	if d.WorkspaceStage != nil && startFromStepIndex == 0 {
		enriched, err := d.WorkspaceStage.PrimeForExecution(ctx, plan, initialContext)
		if err == nil && enriched != nil {
			if initialContext == nil {
				initialContext = make(map[string]string, len(enriched))
			}
			for k, v := range enriched {
				initialContext[k] = v
			}
		}
	}
	masterContext := cloneMap(initialContext)
	runningCost := 0.0

	// ADR-0047 0047-17: operator-feed plan state. Emitted only from this
	// coordinator goroutine (one-timeline-one-publisher, D4); absolute-state so
	// re-delivery folds idempotently. The plan enters "Plans in Flight" now and a
	// single deferred terminal removes it on any return path.
	lastActiveStep := startFromStepIndex
	d.publishPlanState(planID, "running", lastActiveStep, runningCost, false)
	defer func() {
		status := "completed"
		if retErr != nil {
			status = "failed"
		}
		d.publishPlanState(planID, status, lastActiveStep, runningCost, true)
	}()

	// ADR-0021: Plan-level telemetry accumulator.
	planStartTime := time.Now()
	var totalPromptTokens, totalCompletionTokens, totalTokens int
	var fallbackCount, budgetOverrunCount int

	completed := make(map[int]bool, n)
	alreadyDispatched := make(map[int]bool, n)
	inFlight := 0
	var firstErr error
	var failedStepIdx int

	// If resuming from a checkpoint, mark completed steps.
	for idx := 0; idx < startFromStepIndex && idx < n; idx++ {
		completed[idx] = true
	}

	// dispatch launches all steps whose DependsOn indices are all in completed.
	// Only called from the coordinator goroutine — no concurrent access to
	// alreadyDispatched, completed, or inFlight.
	dispatch := func() {
		d.pausedMu.Lock()
		isPaused := d.paused
		d.pausedMu.Unlock()
		if isPaused {
			return
		}
		for _, i := range topoOrder {
			if alreadyDispatched[i] {
				continue
			}
			ready := true
			for _, dep := range plan.Steps[i].DependsOn {
				if !completed[dep] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			alreadyDispatched[i] = true
			inFlight++

			// ADR-0022: build step context snapshot.
			// Phase 3 (use_global_workspace=true): populate WorkingMemory from the
			//   winning step's DependsOn outputs only (ADR-0036: cognitive agents pull
			//   their own LTM via seed_recall; the executor does NOT run PrimeForStep —
			//   it is unwired, see ADR-0048 D3).
			// Phase 0 fallback (use_global_workspace=false or WorkspaceStage=nil): filterSnapshotForStep.
			mu.Lock()
			contextSnapshot := filterSnapshotForStep(masterContext, plan.Steps[i])
			mu.Unlock()

			var workingMemory []domain.ContextRef
			if d.UseGlobalWorkspace && d.WorkspaceStage != nil {
				// ADR-0036: cognitive agents retrieve LTM themselves via seed_recall in the ReAct loop.
				// The executor only injects prior step results (DependsOn) which the agent cannot
				// know on its own.  LTM facts are left to the agent's own memory query budget.
				mu.Lock()
				for _, dep := range plan.Steps[i].DependsOn {
					if cidStr, ok := masterContext[fmt.Sprintf("step_%d_cid", dep)]; ok {
						ref := domain.ContextRef{
							CID:       domain.CID(cidStr),
							Precision: 1.0, // direct dependency = highest relevance
						}
						if res, ok2 := masterContext[fmt.Sprintf("step_%d_result", dep)]; ok2 {
							ref.Snippet = buildSnippet([]byte(res), 500)
						}
						workingMemory = append(workingMemory, ref)
					}
				}
				mu.Unlock()
				contextSnapshot = nil // mutually exclusive with WorkingMemory

				// ADR-0034 / REQ-SDK-007b: surface prior-step artifacts (scope-filtered)
				// into working_memory as discovery refs. Best-effort — GetArtifact is the
				// authoritative gate. Filtered by the session caller_scope (Phase 2); when
				// absent, ScopeSystem surfaces all (still gated downstream at fetch).
				if d.ArtifactLister != nil && d.CurrentSessionID != "" {
					eff := domain.ScopeSystem
					if d.SessionScopes != nil {
						if cs := d.SessionScopes.CallerScope(cancelCtx, d.CurrentSessionID); !cs.IsZero() {
							e := domain.NewEffectiveScope(cs, domain.ScopeConfig{})
							eff = &e
						}
					}
					for _, dep := range plan.Steps[i].DependsOn {
						arts, lerr := d.ArtifactLister.ListStepArtifacts(d.CurrentSessionID, dep)
						if lerr == nil && len(arts) > 0 {
							workingMemory = append(workingMemory,
								scope.ArtifactContextRefs(eff, arts, fmt.Sprintf("step_%d", dep))...)
						}
					}
				}
			}

			wg.Add(1)
			stepIdx := i
			stepWorkingMemory := workingMemory
			stepContextSnapshot := contextSnapshot
			go func() {
				defer wg.Done()
				results <- d.executeStep(cancelCtx, plan, stepIdx,
					stepContextSnapshot, stepWorkingMemory, stepFn, planID)
			}()
		}
	}

	dispatch() // seed: all root steps (DependsOn == nil/empty) go first

	for inFlight > 0 || (func() bool { d.pausedMu.Lock(); defer d.pausedMu.Unlock(); return d.paused }()) {
		// When paused and all in-flight steps have completed, block until resume.
		if inFlight == 0 {
			d.pausedMu.Lock()
			isPaused := d.paused
			isErrorPause := d.errorPause
			resumeCh := d.resumeCh
			d.pausedMu.Unlock()

			if !isPaused {
				continue
			}

			// Error-triggered pause: call ReplanHandler automatically.
			if isErrorPause && d.ReplanHandler != nil {
				if d.replanSuppressor != nil {
					d.replanSuppressor.SetReplanning(true)
				}
				newPlan, replanErr := d.ReplanHandler.Replan(cancelCtx, failedStepIdx, firstErr, cloneMap(masterContext), plan)
				if d.replanSuppressor != nil {
					d.replanSuppressor.SetReplanning(false)
				}
				d.replanCount++
				if replanErr != nil || newPlan == nil {
					slog.Info("ReplanHandler: replan failed, aborting", "error", replanErr)
					return nil, &PartialPlanError{
						FailedStep:  failedStepIdx,
						LastError:   firstErr,
						Context:     cloneMap(masterContext),
						ReplanCount: d.replanCount,
					}
				}

				if d.replanCount > d.MaxReplanAttempts {
					return nil, &PartialPlanError{
						FailedStep:  failedStepIdx,
						LastError:   firstErr,
						Context:     cloneMap(masterContext),
						ReplanCount: d.replanCount,
					}
				}

				if topoErr := d.applyReplannedPlan(newPlan, &plan, &n, alreadyDispatched, completed, &topoOrder); topoErr != nil {
					return nil, topoErr
				}

				if len(newPlan.Steps) > 0 && failedStepIdx < len(newPlan.Steps) {
					if newPlan.Steps[0].Query == plan.Steps[failedStepIdx].Query {
						slog.Warn("Replan dry-run: new plan repeats same faulty step query",
							"step", newPlan.Steps[0].Query)
						return nil, &PartialPlanError{
							FailedStep:  failedStepIdx,
							LastError:   fmt.Errorf("replan validation: replanned plan repeats the same step that failed: %s", newPlan.Steps[0].Query),
							Context:     cloneMap(masterContext),
							ReplanCount: d.replanCount,
						}
					}
				}

				firstErr = nil
				failedStepIdx = 0
				d.pausedMu.Lock()
				d.paused = false
				d.errorPause = false
				d.pausedMu.Unlock()
				dispatch()
				continue
			}

			// External pause: wait for resume signal.
			select {
			case <-resumeCh:
				d.pausedMu.Lock()
				hsPlan := d.hotSwapPlan
				d.hotSwapPlan = nil
				injection := d.pendingInjection
				d.pendingInjection = ""
				d.paused = false
				d.pausedMu.Unlock()

				if hsPlan != nil && len(hsPlan.Steps) > 0 {
					if err := d.applyReplannedPlan(hsPlan, &plan, &n, alreadyDispatched, completed, &topoOrder); err != nil {
						return nil, err
					}
				}

				// ADR-0047 A1.1: fold an operator InjectCorrection into a new
				// forward plan (prior results stay in masterContext). The Planner
				// routes the injected NL step (Zero-Hardcode).
				if injection != "" {
					d.publishPlanState(planID, "replanning", lastActiveStep, runningCost, false)
					completedIdx := make([]int, 0, len(completed))
					for idx := range completed {
						completedIdx = append(completedIdx, idx)
					}
					injPlan := d.buildInjectionPlan(injection, masterContext, plan, completedIdx)
					if err := d.applyReplannedPlan(injPlan, &plan, &n, alreadyDispatched, completed, &topoOrder); err != nil {
						return nil, err
					}
				}

				if d.replanCount > 0 && (d.MaxReplanAttempts == 0 || d.replanCount > d.MaxReplanAttempts) {
					return nil, &PartialPlanError{
						FailedStep:  failedStepIdx,
						LastError:   firstErr,
						Context:     cloneMap(masterContext),
						ReplanCount: d.replanCount,
					}
				}

				dispatch()
				continue
			case <-cancelCtx.Done():
				return nil, cancelCtx.Err()
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		r := <-results
		inFlight--

		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
				failedStepIdx = r.index
				// If ReplanHandler is wired, pause for replanning instead of
				// immediately cancelling. In-flight siblings continue normally.
				if d.ReplanHandler != nil && d.MaxReplanAttempts != 0 {
					d.pausedMu.Lock()
					d.paused = true
					d.errorPause = true
					d.pausedMu.Unlock()
				} else {
					cancel() // signal all in-flight siblings
				}
			}
			continue // do not merge, do not dispatch successors
		}

		mu.Lock()
		cid := mergeStepResult(cancelCtx, masterContext, r, d.ContentStore, d.ContextRefSnippetChars)
		if cid != "" {
			planCIDs = append(planCIDs, cid)
		}
		mu.Unlock()

		// ADR-0021: Accumulate plan-level telemetry.
		totalPromptTokens += r.promptTokens
		totalCompletionTokens += r.completionTokens
		totalTokens += r.totalTokens
		if r.fallbackModelUsed {
			fallbackCount++
		}
		if r.budgetOverrun {
			budgetOverrunCount++
		}

		// ADR-0049 D5: scenes are no longer per-step. The ONE plan-wide scene is written
		// at plan completion (WritePlanScene below); per-step SceneID stays empty.
		var sceneID string

		// ADR-0015: Feed step result to Tier-1 pending channel (non-blocking, best-effort).
		// SceneID forwarded so Tier-2 drain can write the discussed_in edge (ADR-0025).
		if d.MemoryRecorder != nil && r.resp != nil && r.resp.Payload != nil {
			_ = d.MemoryRecorder.RecordExecution(ctx, domain.StepResult{
				Index:     r.index,
				Output:    string(r.resp.Payload.Data),
				Snapshot:  r.snapshot,
				SceneID:   sceneID,
				SessionID: d.CurrentSessionID, // ADR-0029: tags Tier-2 commits with session_id for KeyFacts
				TaskID:    formatTaskID(r.index, planID), // ADR-0049 D3: per-step dedup correlation key
				DependsOnTaskIDs: dependsOnTaskIDs(plan.Steps[r.index].DependsOn, planID), // ADR-0049 D10: follows edges
			})
		}

		runningCost += r.estimatedCost
		completed[r.index] = true
		lastActiveStep = r.index
		d.publishPlanState(planID, "running", r.index, runningCost, false) // ADR-0047 0047-17: step completed

		// Budget check: if plan cost exceeds limit, pause for replan or operator approval.
		if d.MaxPlanCost > 0 && runningCost > d.MaxPlanCost {
			slog.Warn("DAGExecutor: plan cost exceeded budget", "runningCost", runningCost, "maxPlanCost", d.MaxPlanCost)
			if firstErr == nil {
				firstErr = &domain.BudgetExceededError{RunningCost: runningCost, MaxCost: d.MaxPlanCost}
				failedStepIdx = r.index
			}
			if d.ReplanHandler != nil && d.MaxReplanAttempts != 0 {
				d.pausedMu.Lock()
				d.paused = true
				d.errorPause = true
				d.pausedMu.Unlock()
			}
			continue
		}

		// Checkpoint: record snapshot after successful merge.
		if d.CheckpointStore != nil {
			if err := d.CheckpointStore.SaveCheckpoint(d.CurrentSessionID, planID, r.index, cloneMap(masterContext)); err != nil {
				slog.Warn("checkpoint write failed", "session", d.CurrentSessionID, "step", r.index, "err", err)
			}
		}

		// Semantic Checkpoint Gate (ADR-0013): run coherence probe before
		// dispatching successor steps. Mirrors BudgetExceededError control flow.
		step := plan.Steps[r.index]
		if step.CheckpointAfter && d.CheckpointValidator != nil {
			assessment, incoherent := d.runCheckpoint(ctx, r.index, planID, step, masterContext)
			masterContext[fmt.Sprintf("step_%d_checkpoint", r.index)] = assessment
			if incoherent && firstErr == nil {
				firstErr = &domain.SemanticCheckpointError{
					StepIndex:      r.index,
					Assessment:     assessment,
					OriginalResult: masterContext[fmt.Sprintf("step_%d_result", r.index)],
				}
				failedStepIdx = r.index
				if d.ReplanHandler != nil && d.MaxReplanAttempts != 0 {
					d.pausedMu.Lock()
					d.paused = true
					d.errorPause = true
					d.pausedMu.Unlock()
				}
			}
		}

		if firstErr == nil {
			dispatch() // unblock any steps that were waiting on this one
		}
	}

	// Drain unconditionally: goroutines may still be executing defer wg.Done()
	// after sending to results. This prevents goroutine leaks on any exit path.
	wg.Wait()

	// ADR-0021: Write plan-level telemetry.
	planEndTime := time.Now()
	planDurationMs := planEndTime.Sub(planStartTime).Milliseconds()
	outcome := domain.PlanOutcomeSuccess
	if firstErr != nil {
		if d.replanCount > 0 && d.replanCount > d.MaxReplanAttempts {
			outcome = domain.PlanOutcomeReplanExhausted
		} else {
			outcome = domain.PlanOutcomePartial
		}
	}
	planEvent := domain.PlanEvent{
		PlanID:                planID,
		Subject:               plan.Subject,
		StepCount:             len(plan.Steps),
		Outcome:               outcome,
		TotalPromptTokens:     totalPromptTokens,
		TotalCompletionTokens: totalCompletionTokens,
		TotalTokens:           totalTokens,
		TotalEstimatedCost:    runningCost,
		ReplanCount:           d.replanCount,
		FailedStepIndex:       failedStepIdx,
		FallbackCount:         fallbackCount,
		BudgetOverrunCount:    budgetOverrunCount,
		StartTime:             planStartTime,
		EndTime:               planEndTime,
		DurationMs:            planDurationMs,
		PlannerPromptVersion:  plan.PlannerPromptVersion,
		CachePolicy:           plan.CachePolicy,
	}
	if d.PlanEventWriter != nil {
		if err := d.PlanEventWriter.WritePlanEvent(planEvent); err != nil {
			slog.Warn("DAGExecutor: failed to write PlanEvent", "plan_id", planID, "err", err)
		}
	}
	if d.Observer != nil {
		d.Observer.OnPlanCompleted(planEvent)
	}

	// ADR-0049 D5/D7: materialize the plan's ONE immutable scene now that its
	// engaged-entity scope is fully known — for BOTH success and failure (a failure
	// scene is the highest-value precedent), with the outcome recorded.
	if d.MemoryRecorder != nil {
		_ = d.MemoryRecorder.WritePlanScene(ctx, planID, plan.Subject, firstErr == nil)
	}

	if firstErr != nil {
		return nil, &PartialPlanError{
			FailedStep:  failedStepIdx,
			LastError:   firstErr,
			Context:     cloneMap(masterContext),
			ReplanCount: d.replanCount,
		}
	}

	masterContext[finalResultKey] = masterContext[fmt.Sprintf("step_%d_result", topoOrder[n-1])]
	return masterContext, nil
}

// CompletedIndices returns a sorted snapshot of step indices that have
// successfully completed. Safe to call concurrently with Execute.
func (d *DAGExecutor) CompletedIndices() []int {
	d.completedMu.Lock()
	defer d.completedMu.Unlock()
	out := make([]int, len(d.completedIdx))
	copy(out, d.completedIdx)
	return out
}

// cloneMap returns a shallow copy of m. A nil input produces an empty map.
func cloneMap(m map[string]string) map[string]string {
	clone := make(map[string]string, len(m))
	for k, v := range m {
		clone[k] = v
	}
	return clone
}

// CyclicPlanError is returned when TopologicalSort detects a cycle in the plan.
// The Description field is embedded verbatim into the Planner retry prompt.
type CyclicPlanError struct {
	Description string
}

func (e *CyclicPlanError) Error() string {
	return e.Description
}

// PartialPlanError is returned by DAGExecutor.Execute when a step fails after
// some steps have already completed. It carries the accumulated context from
// successful steps so callers can inspect partial results.
type PartialPlanError struct {
	FailedStep  int
	LastError   error
	Context     map[string]string
	ReplanCount int
}

func (e *PartialPlanError) Error() string {
	return fmt.Sprintf("plan partially failed at step %d: %v", e.FailedStep, e.LastError)
}

func (e *PartialPlanError) Unwrap() error {
	return e.LastError
}

// TopologicalSort validates a plan's dependency graph and returns step indices
// in a valid execution order using Kahn's algorithm.
//
// Guarantees:
//   - Every index in DependsOn is in-bounds and non-self-referential before Kahn's runs.
//   - Duplicate edges in a single DependsOn slice are collapsed (no in-degree inflation).
//   - Returns CyclicPlanError with a human-readable edge list for the Planner retry prompt.
//   - An empty plan returns an empty slice with no error.
func TopologicalSort(steps []domain.Step) ([]int, error) {
	n := len(steps)
	if n == 0 {
		return []int{}, nil
	}

	// --- Phase 1: structural validation ---
	for i, step := range steps {
		seen := make(map[int]bool, len(step.DependsOn))
		for _, dep := range step.DependsOn {
			if dep < 0 || dep >= n {
				return nil, fmt.Errorf("step %d has out-of-bounds dependency index %d (plan has %d steps)", i, dep, n)
			}
			if dep == i {
				return nil, &CyclicPlanError{
					Description: fmt.Sprintf("cycle detected: step %d depends on itself", i),
				}
			}
			seen[dep] = true // collapse duplicates
		}
	}

	// --- Phase 2: build adjacency list and in-degree table ---
	// adj[i] holds the indices of steps that list i in their DependsOn,
	// i.e., steps that cannot start until step i has finished.
	adj := make([][]int, n)
	inDegree := make([]int, n)

	for i, step := range steps {
		dedupDeps := deduplicate(step.DependsOn)
		for _, dep := range dedupDeps {
			adj[dep] = append(adj[dep], i)
			inDegree[i]++
		}
	}

	// --- Phase 3: Kahn's BFS ---
	queue := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if inDegree[i] == 0 {
			queue = append(queue, i)
		}
	}

	order := make([]int, 0, n)
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		order = append(order, node)

		for _, successor := range adj[node] {
			inDegree[successor]--
			if inDegree[successor] == 0 {
				queue = append(queue, successor)
			}
		}
	}

	// --- Phase 4: cycle detection ---
	if len(order) != n {
		return nil, buildCyclicPlanError(steps, inDegree)
	}

	return order, nil
}

// buildCyclicPlanError constructs a CyclicPlanError with a readable edge list
// from the set of steps that still have a positive in-degree after Kahn's runs.
func buildCyclicPlanError(steps []domain.Step, inDegree []int) *CyclicPlanError {
	cycleSet := make(map[int]bool)
	for i, d := range inDegree {
		if d > 0 {
			cycleSet[i] = true
		}
	}

	var cycleIndices []string
	for i := range cycleSet {
		cycleIndices = append(cycleIndices, fmt.Sprintf("%d", i))
	}

	var edges []string
	for i := range cycleSet {
		for _, dep := range steps[i].DependsOn {
			if cycleSet[dep] {
				edges = append(edges, fmt.Sprintf("step %d → step %d", dep, i))
			}
		}
	}

	desc := fmt.Sprintf(
		"cycle detected involving steps [%s]: %s",
		strings.Join(cycleIndices, ", "),
		strings.Join(edges, ", "),
	)
	return &CyclicPlanError{Description: desc}
}

// newPlanID generates a random 8-byte hex string used as the planID for a
// single DAGExecutor.Execute call. Each call to Execute produces a unique
// planID so all TaskIDs within the plan form a stable, traceable audit scope.
func newPlanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// formatTaskID returns the canonical TaskID for step stepIndex within a plan
// identified by planID: "step-{stepIndex}-{planID}".
//
// The format is stable across the lifetime of a plan execution: same plan,
// same step always yields the same TaskID. A Planner retry regenerates planID,
// intentionally producing distinct TaskIDs for the new strategic intent.
func formatTaskID(stepIndex int, planID string) string {
	return fmt.Sprintf("step-%d-%s", stepIndex, planID)
}

// dependsOnTaskIDs maps a step's dependency indices to their per-step TaskIDs
// (ADR-0049 D10) so the memory layer can write `follows` edges between step records.
func dependsOnTaskIDs(deps []int, planID string) []string {
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		out = append(out, formatTaskID(d, planID))
	}
	return out
}

// runCheckpoint runs the H1 coherence gate (ADR-0013) via the dedicated
// CheckpointValidator. It assesses the step's output against its original intent
// and returns a structured verdict — no input echo, no magic-token substring
// search. A validator error fails open (coherent) so the gate never fabricates a
// replan from a transient failure.
func (d *DAGExecutor) runCheckpoint(
	ctx context.Context,
	stepIndex int,
	planID string,
	step domain.Step,
	masterContext map[string]string,
) (assessment string, incoherent bool) {
	if d.CheckpointValidator == nil {
		return "", false
	}

	result := masterContext[fmt.Sprintf("step_%d_result", stepIndex)]
	verdict, err := d.CheckpointValidator.Validate(ctx, domain.CheckpointRequest{
		StepQuery:  step.Query,
		StepOutput: result,
		Question:   step.CheckpointQuery,
	})
	if err != nil {
		slog.Warn("checkpoint validator error; proceeding", "step", stepIndex, "err", err)
	}

	if d.EventWriter != nil {
		_ = d.EventWriter.WriteTaskEvent(domain.TaskEvent{
			TaskID:  fmt.Sprintf("checkpoint-%d-%s", stepIndex, planID),
			AgentID: "System_Checkpoint",
		})
	}

	return verdict.Assessment, !verdict.Coherent
}

func deduplicate(indices []int) []int {
	if len(indices) == 0 {
		return indices
	}
	seen := make(map[int]bool, len(indices))
	result := make([]int, 0, len(indices))
	for _, v := range indices {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	return result
}
