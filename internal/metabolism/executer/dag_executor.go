package executer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// (The former SessionCallerScopes seam is gone: composing a session's caller term
// with anything else is the decision point's job, so the executor asks
// domain.Authorizer for a predicate instead of assembling one. ADR-0085.)

// TaskEventWriter is the interface that BBoltAdapter (and test mocks) implement
// to persist TaskEvent records after each DAG step completes.
// The EventWriter field on DAGExecutor may be nil — all writes are best-effort.
type TaskEventWriter interface {
	WriteTaskEvent(ctx context.Context, event domain.TaskEvent) error
}

// LLMGateway is the narrow consumer-side interface DAGExecutor needs for
// session-token lifecycle management (ADR-0018). Nil = zero behaviour change.
type LLMGateway interface {
	Acquire(ctx context.Context, sa domain.StepAllocation, tokenLimit int, estimatedDuration time.Duration) (domain.LeaseID, error)
	Complete(ctx context.Context, leaseID domain.LeaseID) (llm.TokenUsage, error)
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
// plan's report (buildPlanReport: the result of every final step, labelled when
// there are several), for Server.Execute to return as the response payload. It is an internal convention; callers must delete it
// before exposing the context to external consumers.
const finalResultKey = "_dag_final_result"

// EnqueueVerification is an optional callback invoked after each successful step
// so the VerificationWorker can decide whether to sample the task. It never
// blocks — the implementation must perform any queuing asynchronously.
// A nil value disables verification sampling for this executor.
//
// ctx is the step's ctx: the implementation reads the originating session from
// it so the verifier's spend is booked to that session (ADR-0131, owner decision
// 2026-11-01). It must not keep ctx for the asynchronous work — the request ends
// long before verification does.
type EnqueueVerification func(ctx context.Context, taskID, agentID string, req, resp *domain.Handoff)

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
// CheckpointStore is domain.CheckpointStore. It is an ALIAS, not a second declaration:
// having two structurally-identical ports meant an implementation could satisfy one and not
// the other, and the mismatch only ever surfaced at a distant assignment.
type CheckpointStore = domain.CheckpointStore

// CheckpointMeta describes a persisted checkpoint.
// CheckpointMeta is domain.CheckpointMeta (alias — see CheckpointStore).
type CheckpointMeta = domain.CheckpointMeta

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
	EventWriter         TaskEventWriter       // may be nil
	EnqueueVerification EnqueueVerification   // may be nil
	MemoryRecorder      domain.MemoryRecorder // ADR-0015: may be nil
	// Surprise scores each step against merit (ADR-0049 A2.3). Nil ⇒ no prediction is
	// available and the episode is recorded with surprise -1 ("unknown"), which is NOT
	// the same as zero: an unscored fleet is unpredictable, not predictable.
	Surprise               domain.SurpriseOracle
	WorkspaceStage         domain.WorkspaceStage    // ADR-0016: may be nil; nil disables GWS enrichment
	LLMGateway             LLMGateway               // ADR-0018: may be nil
	Observer               domain.TelemetryObserver // ADR-0019: may be nil
	PlanEventWriter        domain.PlanEventWriter   // ADR-0021: may be nil; nil disables plan-level telemetry
	EventBus               domain.EventBus          // ADR-0047 D7/0047-17: may be nil; publishes PlanStateChanged for the operator feed
	InjectPlanner          InjectPlanner            // ADR-0047 A1.1: may be nil; nil ⇒ deterministic instruction-as-step default
	ThoughtFn              StepFunc                 // used for IsThought=true steps; may be nil
	ReplanHandler          ReplanHandler            // optional; nil disables plan-level replan
	CheckpointStore        CheckpointStore          // optional; nil disables checkpointing
	ContentStore           domain.ContentStore      // ADR-0022 Phase 1: may be nil; nil disables CAS
	StepCache              domain.StepCache         // ADR-0026: may be nil; nil disables step-level memoization
	StepCachePolicies      map[string]int           // ADR-0026: operator TTL overrides; nil = use heuristic defaults
	ContextRefSnippetChars int                      // ADR-0022: snippet length; 0 defaults to 500
	// DependencyFit bounds the upstream results inlined into a dependent step's
	// domain.StepDependency list: a byte budget across its dependencies sized from
	// the receiving worker's prompt budget, and the page size a shortened result's
	// outline is numbered in (execution.content; the server resolves it). A zero
	// budget means unbounded — tests and harnesses only.
	DependencyFit domain.DependencyFit
	// EffectVocabulary decides which steps the step cache may serve or store
	// (step_cache_guard.go, ADR-0145 D9): only thought steps and steps whose
	// declared effects are all read-only. nil ⇒ no action step is cacheable.
	EffectVocabulary *domain.StepEffectVocabulary
	// StepCacheDefaultTTL is the TTL of a cacheable step no policy names
	// (execution.plan.step_cache_default_ttl_minutes). <= 0 ⇒ one hour.
	StepCacheDefaultTTL time.Duration
	// RunTracer records each step's output as an invoke_agent span (ADR-0145 D10,
	// step_trace.go). nil ⇒ off (execution.trace.enabled).
	RunTracer domain.RunTracer
	// ADR-0022 Phase 3: circuit-breaker flag.
	// true  → Handoff.WorkingMemory populated from the step's DependsOn outputs; Handoff.Context empty.
	// false → filterSnapshotForStep into Handoff.Context (Phase 0 behavior).
	// (PrimeForStep, the executor-push LTM path this once selected, was deleted
	// 2026-11-01 — ADR-0048 D3 / T-20261031-primeforstep-is-unwired-so-activation-threshold.)
	UseGlobalWorkspace      bool
	StepFactCosineThreshold float64 // AGENTCONTEXTREQ REQ2: min cosine for forwarding a planning fact to a step (default 0.55)
	MaxReplanAttempts       int     // max replan attempts; 0 disables replan
	MaxFanOutWidth          int     // ADR-0078 R2: cap on parametric fan-out expansion; 0 disables the cap
	// FanOutConcurrency bounds how many children of fan-out expansions (Step.FanOutItem
	// set) run at once in one plan (execution.plan.fan_out_concurrency). > 1 also serves
	// each child on a dedicated agent instance (domain.WithDedicatedInstance), because an
	// agent process runs one Execute at a time; <= 1 runs children one at a time on the
	// agent's shared instance.
	FanOutConcurrency int
	// Calendar is the kernel's one "today" (domain.Calendar). Its block is stamped on
	// every step's handoff (domain.HandoffKeyCurrentDate) so an agent or thought step
	// resolves "the last two weeks" against the organisation's date, and it scopes the
	// step cache to that date: a step that sees the date has it as an input, so
	// yesterday's answer to a relative ask is never replayed today
	// (T-20261107-planner-and-chat-agent-are-not-told-today-s). nil ⇒ the wall clock
	// in UTC.
	Calendar               *domain.Calendar
	MaxPlanCost            float64          // total plan budget; 0 disables budget enforcement
	DefaultInputCostPer1M  float64          // cost per 1M input tokens for cost estimation
	DefaultOutputCostPer1M float64          // cost per 1M output tokens for cost estimation
	CurrentSessionID       domain.SessionID // session the run belongs to; empty = no session
	// CurrentRunID is the execution checkpoints are written under. Checkpoints are keyed by
	// RUN because a step index only means something relative to the plan of the run it was
	// taken against. Empty ⇒ no checkpointing.
	CurrentRunID        domain.RunID
	CheckpointFlushSecs int // periodic flush interval in seconds (0 = only on pause)

	// Authz is the decision point whose read predicate filters the prior-step
	// artifacts surfaced into working memory (REQ-SDK-007b; ADR-0157 D5: they are
	// run-ledger artifact entries, read through LedgerReader). nil ⇒ the system
	// scope (the OSS default); GetArtifact remains the authoritative gate.
	Authz domain.Authorizer

	// Ledger is the ADR-0145 D1 run ledger's writer (ledger_steps.go): every step
	// output is recorded as a step_output entry and step_N_cid holds its id. nil ⇒
	// the legacy content-store path. Set together with LedgerReader, and with
	// ContentStore nil (one pathway: step results are no longer content-store scratch).
	Ledger domain.LedgerWriter
	// LedgerReader is the ENFORCED ledger reader dependants' upstream results are
	// read from, under LedgerReadScope (the kernel acting for the run's owner; nil ⇒
	// a plain bypass, which keeps no owner fence open).
	LedgerReader    domain.LedgerReader
	LedgerReadScope *domain.TagPredicate

	// Requirements is the ADR-0145 D5/D6 state of the request
	// (envelope_steps.go, execution.plan.checklist_enabled): steps that answer
	// checklist items are told which, their envelopes are validated against the
	// ledger with one repair turn, and the accepted results drive Coverage. nil ⇒
	// the legacy prose path. Requires Ledger and LedgerReader.
	Requirements *StepRequirements
	// planMu/currentPlan expose the plan being run (it changes on fan-out and
	// replan) to the envelope check's dependency walk.
	planMu      sync.Mutex
	currentPlan *domain.ExecutionPlan
	// generation counts the plans swapped in by applyReplannedPlan (under planMu);
	// see planGeneration.
	generation int

	// SessionCloser marks a session finished when its plan completes.
	//
	// Nothing else closes a task. `SessionCompleted` was previously set in exactly
	// one place — the operator's explicit Complete command — so a task whose plan
	// ran to the end stayed `active` for ever, and the console filled with rows
	// that were neither working nor finished.
	//
	// nil ⇒ tasks stay open until an operator closes them, which is the previous
	// behaviour and remains correct for a kernel that wires no session manager.
	SessionCloser SessionCloser

	// replanSuppressor is notified when replanning starts/ends to suppress
	// duplicate signals (usually set to the Watcher).
	replanSuppressor interface{ SetReplanning(bool) }
	completedMu      sync.Mutex
	completedIdx     []int

	// Per-execution pause state (set during Execute, accessed by Pause/Resume/HotSwap).
	pausedMu         sync.Mutex
	paused           bool
	resumeCh         chan struct{}
	hotSwapPlan      *domain.ExecutionPlan
	pendingInjection string // ADR-0047 A1.1: operator instruction folded in on resume
	replanCount      int
	errorPause       bool // true when pause was triggered by step error (auto-replan)
	// effectReplans counts replans spent on an effect-check refusal in this run.
	// The owner rule is ONE: the first refusal replans with the reason ("no agent
	// can move"); a second ends the plan, and the server turns it into a question
	// for the user — never a dispatch to an arbitrary agent.
	effectReplans int
}

// InjectPlanner composes the new forward plan for an operator InjectCorrection
// from the instruction plus the live execution state (ADR-0047 A1.1). Optional:
// when nil, the executor uses a deterministic default — a single-step forward
// plan carrying the instruction, routed by the normal auction (Zero-Hardcode).
// completed is the set of already-finished step indices; ctx is the plan context,
// for the planner to read.
//
// How the new plan's steps SEE earlier results: not through masterContext (a step
// receives only the results of its own in-plan DependsOn, and the swap resets the
// completed set). applyReplannedPlan captures the swapped-out plan's outcomes as
// prior-plan domain.StepDependency entries, and every step of the new plan that
// declares no in-plan dependency receives them (priorPlanResults,
// T-20261106-an-injected-injectcorrection-step-cannot-see).
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
	cached            bool              // served from the step cache (ADR-0026); read by the run trace
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
	deps stepDeps,
	stepFn StepFunc,
	planID string,
) stepResult {
	sizeBefore := contextByteSize(snapshot)
	// The step's plan generation rides its ctx to the lease binding (server), so
	// its tool results are recorded under the generation's ledger run id.
	ctx = domain.WithPlanGeneration(ctx, d.planGeneration())

	slog.Info("🚀 Dispatching Step", "index", stepIndex)

	// The step as dispatched: a fan-out source is told to end with its items as a
	// JSON array (fanout_source.go). The cache key reads the dispatched query, so a
	// result cached without that instruction is never replayed to a source.
	dispatched := plan.Steps[stepIndex]
	dispatched.Query = fanOutSourceQuery(plan, stepIndex)
	// The date the step is told is today, read once so the stamp and the cache key
	// agree even across midnight.
	today := d.Calendar.Block()

	// ADR-0026: step-level cache lookup. On hit, skip stepFn entirely.
	var cacheKey string
	// ADR-0145 D6: a step that answers checklist items is never served from the
	// cache — its envelope must cite THIS run's tool results.
	answersItems := d.Requirements != nil && len(plan.Steps[stepIndex].Answers) > 0
	if d.StepCache != nil && !answersItems {
		// Keyed on the FULL upstream results (deps.results), not the snapshot: in
		// Phase 3 the snapshot carries no step_N_result keys, so a dependent step's
		// key used to ignore its inputs and replay another plan's answer for the same
		// subject and query (T-20261105-a-planner-thought-step-that-depends-on-two).
		// ADR-0145 D9: the key also carries whose result it is (session, principal,
		// scope), and only read-only steps get one; "" ⇒ neither served nor stored.
		cacheKey = d.guardedStepCacheKey(ctx, plan, planID, stepIndex, dispatched, deps.results, today)
		if cacheKey == "" {
			// not cacheable: fall through to live execution
		} else if cached, ok, getErr := d.StepCache.Get(ctx, cacheKey); getErr != nil {
			slog.Warn("StepCache.Get failed, falling through to live execution", "step", stepIndex, "err", getErr)
		} else if ok && cached != nil {
			cached.BudgetLease = nil // stale session tokens must not be restored from cache
			slog.Info("⚡ Step served from cache", "index", stepIndex)
			return stepResult{index: stepIndex, resp: cached, cached: true}
		}
	}

	var resp *domain.Handoff
	var stepErr error

	// ADR-0022: WorkingMemory and Context are mutually exclusive.
	// When WorkingMemory is non-nil (use_global_workspace=true), Context is nil.
	// ADR-0049 D3: hand the agent its per-step correlation key so its tool calls
	// (actions) get stamped with it, which is how an action record finds its plan and
	// its episode parent (ADR-0095 D1).
	if snapshot == nil {
		snapshot = map[string]string{}
	}
	snapshot["_task_id"] = formatTaskID(stepIndex, planID)
	snapshot["_step_index"] = strconv.Itoa(stepIndex)
	// Kernel-authored, always overwritten: the organisation's date (domain.Calendar).
	domain.StampCurrentDate(snapshot, today)
	// The upstream results, structured (domain.StepDependency), on every dependent
	// step whichever executor runs it. Kernel-authored: always overwritten, and a
	// root step carries none even if an earlier context held the key.
	if deps.manifest != "" {
		snapshot[domain.HandoffKeyStepDependencies] = deps.manifest
	} else {
		delete(snapshot, domain.HandoffKeyStepDependencies)
	}
	// ADR-0145 D6.3: a fan-out child receives its whole item object. Kernel-
	// authored like the keys above: stamped on a child, removed from every other step.
	if item := plan.Steps[stepIndex].FanOutItemJSON; item != "" {
		snapshot[domain.HandoffKeyFanOutItem] = item
	} else {
		delete(snapshot, domain.HandoffKeyFanOutItem)
	}

	// ADR-0145 D5/D6: the items this step answers and its bound inputs.
	d.stampRequirementKeys(plan, stepIndex, snapshot)

	stepHandoff := &domain.Handoff{
		Payload:       &domain.Payload{Data: []byte(dispatched.Query)},
		Context:       snapshot,
		WorkingMemory: workingMemory,
	}
	// The callback may hold an older copy of the plan (fan-out and replan rewrite it
	// mid-run), so hand it THIS plan's step: its routing contract must travel with it.
	// An agent step carries the planner's query there (it is the auction's task
	// description; the agent reads the handoff payload), a thought step the
	// dispatched one, since the thought call reads its task from the ctx step.
	if plan.Steps[stepIndex].IsThought && d.ThoughtFn != nil {
		resp, stepErr = d.ThoughtFn(domain.WithPlanStep(ctx, dispatched), stepIndex, stepHandoff)
	} else {
		resp, stepErr = stepFn(domain.WithPlanStep(ctx, plan.Steps[stepIndex]), stepIndex, stepHandoff)
	}

	if stepErr != nil {
		return stepResult{index: stepIndex, err: stepErr}
	}
	if resp == nil {
		return stepResult{index: stepIndex, resp: nil, err: nil}
	}
	// A worker that answered with a reference to one of its own tool results
	// (answer_ref) gets that result's text as its output, before anything reads it
	// (T-20261112-summarise-a-document-step-gets-only-the-first).
	resp, stepErr = d.applyAnswerRef(ctx, stepIndex, resp)
	if stepErr != nil {
		return stepResult{index: stepIndex, err: stepErr}
	}
	// ADR-0145 D6: validate the answer envelope (one repair turn) and apply it to
	// the request's coverage. Legacy path: unchanged.
	resp = d.envelopeStep(ctx, plan, stepIndex, stepHandoff, stepFn, resp, deps.refs)

	// The step lease's provider usage (ADR-0131 D2), consumed BEFORE the growth
	// measurement so kernel bookkeeping keys never count as context the step
	// added, and never merge into the plan context.
	usage, haveUsage := llm.StepUsageFrom(resp.Context)
	llm.ClearStepUsage(resp.Context)
	// ADR-0139 D6: the dispatched definition's identity, stamped by the Dispatcher
	// (domain.HandoffKeyWinnerSourceHash), consumed here for the same reason.
	winnerID := resp.Context[domain.HandoffKeyWinnerAgentID]
	winnerHash := resp.Context[domain.HandoffKeyWinnerSourceHash]
	winnerLatency := resp.Context[domain.HandoffKeyWinnerLatencyMs]
	delete(resp.Context, domain.HandoffKeyWinnerAgentID)
	delete(resp.Context, domain.HandoffKeyWinnerSourceHash)
	delete(resp.Context, domain.HandoffKeyWinnerLatencyMs)
	// The step's own input manifest, echoed back by step functions that return
	// the handoff context (thoughtFn does); it must not merge into the plan context.
	delete(resp.Context, domain.HandoffKeyStepDependencies)
	delete(resp.Context, domain.HandoffKeyCurrentDate) // kernel-stamped input, likewise
	delete(resp.Context, domain.HandoffKeyFanOutItem)  // likewise (ADR-0145 D6.3)
	delete(resp.Context, domain.HandoffKeyAnswerItems) // likewise (ADR-0145 D6)
	delete(resp.Context, domain.HandoffKeyStepInputs)
	delete(resp.Context, domain.HandoffKeyEnvelopeViolations)
	sizeAfter := contextByteSize(resp.Context)
	growthBytes := sizeAfter - sizeBefore
	if growthBytes < 0 {
		growthBytes = 0
	}
	agentID := resp.FromAgent
	if plan.Steps[stepIndex].IsThought {
		agentID = "System_Thought"
	}
	// The SourceHash keys the profile the Gatekeeper and Dispatcher read
	// (GetProfile(agent.ID, agent.SourceHash)). Without it every step outcome
	// aggregated into `profile:<agent>:`, which nothing reads, and the cold-start
	// prior stayed the agent's merit for ever
	// (T-20261104-routing-never-learns-from-real-outcomes-dag). Used only when the
	// stamp names the agent the response came from; otherwise (a runner-up fallback
	// or a yield resume that bypassed the Dispatcher's stamp) the hash stays empty,
	// the pre-fix behaviour.
	var sourceHash string
	// The winner's call wall time (domain.HandoffKeyWinnerLatencyMs), on the same
	// terms as the hash: it describes the stamped winner only. 0 = unknown, which
	// the aggregator leaves out of the latency median
	// (T-20261104-step-taskevents-carry-no-latency-so-the-merit).
	var latencyMs int
	if winnerID != "" && winnerID == agentID {
		sourceHash = winnerHash
		if ms, err := strconv.Atoi(winnerLatency); err == nil && ms > 0 {
			latencyMs = ms
		}
	}
	// Server-authoritative: stamped by the step function from the gateway's
	// reconciled lease, never by the agent (ADR-0018; domain.HandoffKeyBudgetOverrun).
	budgetOverrun := resp.Context[domain.HandoffKeyBudgetOverrun] == "true"

	slog.Info("✅ Step Completed", "index", stepIndex, "agent", agentID)

	d.completedMu.Lock()
	d.completedIdx = append(d.completedIdx, stepIndex)
	d.completedMu.Unlock()

	taskID := formatTaskID(stepIndex, planID)
	// ADR-0131 D2: bill the step from the provider usage its lease reconciled
	// (stamped by the step function, llm.StampStepUsage), and fall back to the
	// len/4 estimate only when there is none — a thought step, a step whose agent
	// made no gateway call, a step function without a gateway — flagging the
	// TaskEvent UsageEstimated so a reader knows which figure it holds. Priced with
	// the default generator's ledger prices either way: agent steps stream through
	// the configured default model until per-step allocation reaches the lease.
	if !haveUsage {
		respText := ""
		if resp.Payload != nil {
			respText = string(resp.Payload.Data)
		}
		usage = llm.EstimateUsage(plan.Steps[stepIndex].Query, respText)
	}
	promptTokens := usage.PromptTokens
	responseTokens := usage.CompletionTokens
	estimatedCost := llm.CalculateCost(usage, d.DefaultInputCostPer1M, d.DefaultOutputCostPer1M)

	if d.EventWriter != nil || d.Observer != nil {
		// ROUTE-06 / ADR-0069: stamp the step's (first) required capability so the
		// aggregator can compute capability-scoped merit.
		var stepCap string
		if rc := plan.Steps[stepIndex].RequiredCapabilities; len(rc) > 0 {
			stepCap = rc[0]
		}
		event := domain.TaskEvent{
			TaskID:             taskID,
			AgentID:            agentID,
			SourceHash:         sourceHash,
			BidConfidence:      float64(resp.Confidence),
			ContextGrowthBytes: growthBytes,
			// Wall time of the agent call; network and computation are not
			// separable from the kernel, so it is all booked as computation.
			ComputationLatencyMs: latencyMs,
			PromptTokens:         promptTokens,
			CompletionTokens:     responseTokens,
			TotalTokens:          promptTokens + responseTokens,
			EstimatedCost:        estimatedCost,
			BudgetOverrun:        budgetOverrun,
			Capability:           stepCap,
			UsageEstimated:       usage.Estimated,
		}
		if d.EventWriter != nil {
			// The step already ran and spent its tokens: its merit record must
			// land even when the request is cancelled now (AGENTS.md §2.7).
			if err := d.EventWriter.WriteTaskEvent(context.WithoutCancel(ctx), event); err != nil {
				slog.Warn("DAGExecutor: failed to write TaskEvent", "step", stepIndex, "err", err)
			}
		}
		if d.Observer != nil {
			d.Observer.OnTaskCompleted(ctx, event)
		}
	}
	// Skip verification for budget-signal responses — the agent refused the task
	// on cost grounds, not quality grounds. Verifying would incorrectly penalise
	// the agent's TrustScore. ADR-0023 Fix 5.
	isBudgetSignal := resp.Payload != nil && resp.Payload.Type == "budget_signal"
	if d.EnqueueVerification != nil && !isBudgetSignal {
		d.EnqueueVerification(ctx, taskID, agentID, &domain.Handoff{
			Payload: &domain.Payload{Data: []byte(plan.Steps[stepIndex].Query)},
			Context: snapshot,
		}, resp)
	}

	// ADR-0026: cache the successful result for future steps with identical inputs.
	// resp is non-nil here: a nil response returned early above.
	if d.StepCache != nil && cacheKey != "" {
		if ttl := resolveCacheTTL(plan.Steps[stepIndex], d.StepCachePolicies, d.StepCacheDefaultTTL); ttl > 0 {
			if putErr := d.StepCache.Put(ctx, cacheKey, resp, ttl); putErr != nil {
				slog.Warn("StepCache.Put failed", "step", stepIndex, "err", putErr)
			}
		}
	}

	return stepResult{
		index:            stepIndex,
		resp:             resp,
		err:              nil,
		estimatedCost:    estimatedCost,
		budgetOverrun:    budgetOverrun,
		promptTokens:     promptTokens,
		completionTokens: responseTokens,
		totalTokens:      promptTokens + responseTokens,
		snapshot:         snapshot,
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
	if r.resp == nil || r.resp.Payload == nil {
		// A step that finished with no output is RECORDED as empty, not skipped: a
		// replanned plan reuses step indices, so skipping left the previous plan's
		// step_N_result for a dependent to read as this step's, and an absent key
		// reads as "unavailable" to buildStepDependencies, not as "empty".
		masterContext[fmt.Sprintf("step_%d_result", r.index)] = ""
		delete(masterContext, fmt.Sprintf("step_%d_cid", r.index))
	}
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
	// A cid from an earlier plan's step at this index must not outlive its result.
	delete(masterContext, fmt.Sprintf("step_%d_cid", r.index))

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

// snippetChars is ContextRefSnippetChars with mergeStepResult's default (500).
func (d *DAGExecutor) snippetChars() int {
	if d.ContextRefSnippetChars <= 0 {
		return 500
	}
	return d.ContextRefSnippetChars
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

// applyReplannedPlan swaps in a new plan after replan/hot-swap/injection. It
// re-runs topological sort, records the swapped-out plan's outcomes (its completed
// results and its failed step, fail) into prior so the new plan's root steps receive
// them, and resets dispatch state. A plan that does not sort leaves everything
// untouched. Coordinator-only, with no step in flight (every call site waits for
// inFlight == 0), so master is read without the context lock.
func (d *DAGExecutor) applyReplannedPlan(newPlan *domain.ExecutionPlan, plan **domain.ExecutionPlan, n *int, alreadyDispatched map[int]bool, completed map[int]bool, topoOrder *[]int,
	master map[string]string, prior *priorPlanResults, fail *stepFailure) error {
	d.setCurrentPlan(newPlan)
	// A new plan generation: its steps record to the ledger under their own run id.
	d.planMu.Lock()
	d.generation++
	d.planMu.Unlock()
	if d.Requirements != nil {
		// A replanned plan's step indices start again: per-step evidence and
		// values of the swapped-out plan no longer name its steps (coverage stays).
		d.Requirements.ResetStage()
	}
	order, err := TopologicalSort(newPlan.Steps)
	if err != nil {
		return err
	}
	prior.capture(master, *plan, completed, fail)
	*plan = newPlan
	*n = len(newPlan.Steps)
	*topoOrder = order
	for k := range alreadyDispatched {
		delete(alreadyDispatched, k)
	}
	for k := range completed {
		delete(completed, k)
	}
	return nil
}

// applyExpandedPlan swaps in a fan-out-expanded plan (ADR-0078 R2). Expansion APPENDS
// children at fresh indices and leaves every existing index intact, so — unlike
// applyReplannedPlan — the completed and alreadyDispatched maps and all masterContext
// step_N keys stay valid and are NOT cleared. The inert parametric node is marked completed
// so it is fully accounted (the dispatch guard already prevents it from ever running); a
// demoted node (domain.ResolveFanOut: no item placeholder, or a side-effecting step over
// an unstructured source, or any step over JSON with no recognisable list) is an
// ordinary step again and is left to dispatch.
// Runs in the coordinator goroutine.
func (d *DAGExecutor) applyExpandedPlan(expanded *domain.ExecutionPlan, plan **domain.ExecutionPlan, n *int, completed map[int]bool, topoOrder *[]int, fanIdx int) error {
	order, err := TopologicalSort(expanded.Steps)
	if err != nil {
		return err
	}
	*plan = expanded
	d.setCurrentPlan(expanded)
	*n = len(expanded.Steps)
	*topoOrder = order
	// An expanded node is inert and done (its children carry the work). A DEMOTED one
	// (domain.ResolveFanOut) is now an ordinary step that still has to run, so it is
	// left for dispatch.
	if expanded.Steps[fanIdx].FanOutOver != nil {
		completed[fanIdx] = true
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
// SessionCloser marks a task finished. Satisfied by the session manager.
//
// It reads before it writes so a legitimate refusal never reaches the log — see
// closeSession.
type SessionCloser interface {
	GetSession(ctx context.Context, id domain.SessionID) (*domain.Session, error)
	TransitionStatusReason(ctx context.Context, sessionID domain.SessionID, target domain.SessionStatus, reason string) error
}

// closeSession marks this run's task finished, best-effort.
//
// Best-effort deliberately: the work succeeded, and a task that failed to close
// is a tidiness problem. Failing the plan over it would turn a cosmetic issue
// into a lost result.
//
// Only `active` is closed. A PAUSED task is an operator decision and the
// transition would be refused anyway; a DORMANT one the kernel put to sleep and
// may still wake. Reading the status first keeps a legitimate refusal out of the
// log, so a warning here means something actually went wrong.
func (d *DAGExecutor) closeSession(runCtx context.Context) {
	if d.SessionCloser == nil || d.CurrentSessionID == "" {
		return
	}
	// Detached from the RUN's context (AGENTS.md §2.7): the caller is finished
	// with the run and its cancellation on the way out must not be what stops the
	// task being closed — but its values (session, principal, scope, trace) are
	// kept, which a bare context.Background() would discard
	// (T-20261113-context-background-on-request-paths-drops item 8).
	ctx := context.WithoutCancel(runCtx)

	ses, err := d.SessionCloser.GetSession(ctx, d.CurrentSessionID)
	if err != nil || ses == nil {
		return
	}
	if ses.Status != domain.SessionActive {
		// Paused is an operator decision and dormant may still wake. Both would
		// be refused by the transition rules; returning here keeps that expected
		// refusal out of the log, so a warning below means something real.
		return
	}

	if err := d.SessionCloser.TransitionStatusReason(ctx, d.CurrentSessionID,
		domain.SessionCompleted, "every plan finished"); err != nil {
		slog.Warn("could not close the task after its plan finished",
			"session", d.CurrentSessionID, "err", err,
			"effect", "the task stays open until an operator completes it")
	}
}

func (d *DAGExecutor) publishPlanState(ctx context.Context, planID, status string, activeStep int, cost float64, terminal bool, steps []domain.PlanStepState) {
	if d.EventBus == nil {
		return
	}
	_ = d.EventBus.Publish(ctx, domain.PlanStateChanged{
		SessionID:  string(d.CurrentSessionID),
		PlanID:     planID,
		ActiveStep: activeStep,
		Status:     status,
		CostSoFar:  cost,
		Terminal:   terminal,
		Steps:      steps,
	})
}

// planStepLabelMax caps a step's query when used as a DAG node label so the operator
// feed carries a glanceable title, not the full multi-KB instruction.
const planStepLabelMax = 120

// buildStepStates snapshots the plan's DAG for the operator feed: one node per step with
// its dependency edges and current status derived from the coordinator's tracking maps.
// Coordinator-goroutine only (reads the same maps publishPlanState's callers own).
func buildStepStates(plan *domain.ExecutionPlan, completed, dispatched map[int]bool, agentByStep map[int]string, failedIdx int, hasErr bool) []domain.PlanStepState {
	if plan == nil {
		return nil
	}
	out := make([]domain.PlanStepState, len(plan.Steps))
	for i, s := range plan.Steps {
		status := "pending"
		switch {
		case hasErr && i == failedIdx:
			status = "failed"
		case completed[i]:
			status = "done"
		case dispatched[i]:
			status = "running"
		}
		label := s.Query
		if len(label) > planStepLabelMax {
			label = label[:planStepLabelMax]
		}
		out[i] = domain.PlanStepState{
			Index:     i,
			Label:     label,
			DependsOn: s.DependsOn,
			IsThought: s.IsThought,
			Status:    status,
			Agent:     agentByStep[i],
			// Contract 0072: the capability contract travels with the step so the
			// console can show WHY it routed, not only where it landed.
			RequiredCapabilities: s.RequiredCapabilities,
		}
	}
	return out
}

// buildInjectionPlan composes the new forward plan for an operator injection.
// Uses InjectPlanner when set; otherwise the deterministic default — a single
// NL step carrying the instruction, routed by the auction. That step has no
// in-plan dependency, so it receives the swapped-out plan's results as prior-plan
// StepDependency entries (see InjectPlanner). Called only from the coordinator.
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
//   - masterContext[finalResultKey]       = buildPlanReport: every final (sink) step's result
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
	d.setCurrentPlan(plan)
	n := len(plan.Steps)
	if n == 0 {
		return cloneMap(initialContext), nil
	}

	// ADR-0022 Phase 1: collect CIDs for plan-scoped GC.
	// defer ensures GC fires even on panic — orphaned CIDs accumulate otherwise.
	//
	// A store that tracks live plans (domain.PlanScratchTracker — the production
	// BBoltContentStore does) is given this plan's lifetime instead: its EndPlan
	// collects only scratch no other live plan can need, so this plan finishing
	// never evicts a concurrent plan's step/tool results, and after the last live
	// plan ends all scratch is gone, this plan's own step results included
	// (T-20261103-a-plan-s-content-store-gc-evicts-a-concurrently). A store
	// without it falls back to GC(keep = this plan's step CIDs).
	var planCIDs []domain.CID
	if d.ContentStore != nil {
		tracker, tracked := d.ContentStore.(domain.PlanScratchTracker)
		var lease domain.PlanLease
		if tracked {
			var berr error
			if lease, berr = tracker.BeginPlan(ctx); berr != nil {
				// Without a lease this plan's scratch is unprotected from other
				// plans' GC; fall back rather than fail the plan.
				slog.Warn("ContentStore BeginPlan failed; falling back to GC(keep)", "err", berr)
				tracked = false
			}
		}
		defer func() {
			// WithoutCancel: GC must run even when the plan's ctx was cancelled,
			// but keeps the caller's values (scope, session, trace) — AGENTS.md §2.7.
			// Both paths only evict plan-scoped scratch types (domain.IsPlanScopedNodeType).
			gcCtx := context.WithoutCancel(ctx)
			var err error
			if tracked {
				err = tracker.EndPlan(gcCtx, lease)
			} else {
				err = d.ContentStore.GC(gcCtx, planCIDs)
			}
			if err != nil {
				slog.Warn("ContentStore GC failed", "err", err)
			}
		}()
		// A resumed plan's completed steps were written to CAS by an earlier
		// execution whose GC may since have evicted them; re-Put each result whose
		// step_N_cid the resumed context still carries, so dependents' ContextRefs
		// resolve. Same bytes ⇒ same CID; inside the lease, so it is protected.
		for i := 0; i < startFromStepIndex; i++ {
			cidStr, hasCID := initialContext[fmt.Sprintf("step_%d_cid", i)]
			res, hasRes := initialContext[fmt.Sprintf("step_%d_result", i)]
			if !hasCID || !hasRes || cidStr == "" {
				continue
			}
			cid, perr := d.ContentStore.Put(ctx, []byte(res), domain.NodeTypeStepResult,
				[]string{fmt.Sprintf("step_%d", i), "result"}, buildSnippet([]byte(res), d.snippetChars()))
			if perr != nil {
				slog.Warn("ContentStore re-Put of resumed step failed", "step", i, "err", perr)
				continue
			}
			if string(cid) != cidStr {
				slog.Warn("resumed step result does not hash to its recorded cid", "step", i, "recorded", cidStr, "computed", cid)
			}
			planCIDs = append(planCIDs, cid)
		}
	}

	// Initialize per-execution pause state.
	d.pausedMu.Lock()
	d.paused = false
	d.resumeCh = make(chan struct{}, 1)
	d.hotSwapPlan = nil
	d.replanCount = 0
	d.effectReplans = 0
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
	// fanOutSem bounds how many fan-out CHILDREN of this plan execute at once
	// (execution.plan.fan_out_concurrency; <= 1 ⇒ one at a time on the agent's shared
	// instance, the pre-2026-11-07 effective behaviour). Ordinary steps are not bounded.
	fanOutSem := make(chan struct{}, max(1, d.FanOutConcurrency))

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
	// Outcomes of every plan a replan swaps out of this run, handed to the
	// replacement plan's root steps (T-20261106-an-injected-injectcorrection-step-cannot-see).
	prior := &priorPlanResults{}
	runningCost := 0.0

	// Coordinator-owned step tracking. Declared before the first publishPlanState so the
	// DAG snapshot (buildSteps below) reflects resume state on the very first emit.
	completed := make(map[int]bool, n)
	alreadyDispatched := make(map[int]bool, n)
	agentByStep := make(map[int]string, n) // ADR-0047: executor agent id per step, for the DAG view
	inFlight := 0
	var firstErr error
	var failedStepIdx int
	// abortErr is the error an ABANDONED coordinator loop owes the caller verbatim —
	// set by the exits that cannot be reconstructed from firstErr alone (a hot-swap that
	// outran the replan budget records no step error at all, and a malformed replanned
	// plan must surface its own topology error, not a PartialPlanError).
	//
	// It exists so those exits can `break coordinate` instead of returning from inside
	// the loop. They used to return directly, which skipped the drain, the ADR-0021
	// PlanEvent, and the ADR-0049/0095 completion write — so a plan that exhausted its
	// replans left its episode row stuck at "running" forever and produced no telemetry
	// and no memory of the one failure worth remembering.
	var abortErr error

	// If resuming from a checkpoint, mark completed steps.
	for idx := 0; idx < startFromStepIndex && idx < n; idx++ {
		completed[idx] = true
	}

	// buildSteps snapshots the current DAG for the operator feed. Closes over the
	// coordinator's tracking maps + failure state, so each emit reflects live per-node
	// status. plan is reassigned on replan, so it is read fresh each call.
	buildSteps := func() []domain.PlanStepState {
		return buildStepStates(plan, completed, alreadyDispatched, agentByStep, failedStepIdx, firstErr != nil)
	}

	// ADR-0047 0047-17: operator-feed plan state. Emitted only from this
	// coordinator goroutine (one-timeline-one-publisher, D4); absolute-state so
	// re-delivery folds idempotently. The plan enters "Plans in Flight" now and a
	// single deferred terminal removes it on any return path.
	lastActiveStep := startFromStepIndex
	d.publishPlanState(ctx, planID, "running", lastActiveStep, runningCost, false, buildSteps())
	defer func() {
		status := "completed"
		if retErr != nil {
			status = "failed"
		}
		// The terminal state must reach the operator feed even when the plan
		// ended because its request was cancelled (AGENTS.md §2.7).
		d.publishPlanState(context.WithoutCancel(ctx), planID, status, lastActiveStep, runningCost, true, buildSteps())

		// The task is finished when its work is. ONLY on success: a failed plan
		// leaves the task open, because a failure is the state that most needs a
		// human and `completed` is irreversible — closing it would file the
		// failure away as "Finished" and remove the one row worth looking at.
		if retErr == nil {
			d.closeSession(ctx)
		}
	}()

	// ADR-0021: Plan-level telemetry accumulator.
	planStartTime := time.Now()

	// ADR-0095 D1: open the EPISODE before the first step dispatches.
	//
	// Every record this plan writes hangs off a parent derived from planID, and the
	// action records are written WHILE the plan runs — so the parent has to exist now,
	// not at completion. It is a nullable FK resolved through a subselect, which means
	// getting this order wrong costs nothing at write time and silently leaves the
	// episode in pieces.
	//
	// Errors are swallowed for the same reason the outcome write is: bookkeeping about a
	// memory must never be able to fail the work that produced it (D5).
	if d.MemoryRecorder != nil {
		_ = d.MemoryRecorder.BeginExperience(ctx, planID)
	}
	var totalPromptTokens, totalCompletionTokens, totalTokens int
	var fallbackCount, budgetOverrunCount int

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
			// ADR-0078 R2: never dispatch a parametric fan-out node as-is — it is
			// replaced by concrete children when its source step completes (the
			// expansion hook below). Skipping it here prevents a race where the
			// node runs before expansion.
			if plan.Steps[i].FanOutOver != nil {
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
			// ADR-0145 D1: with the ledger on, upstream content is read from the ledger.
			deps := buildStepDependenciesFrom(masterContext, plan, i, d.DependencyFit, prior, d.ledgerResolver(cancelCtx))
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
							CID: domain.CID(cidStr),
							// Typed and labelled so a receiver can tell which upstream step
							// a ref is (SDK find_step_ref; the SDK renders these steps from
							// the HandoffKeyStepDependencies manifest, not as anonymous facts).
							Type:      domain.NodeTypeStepResult,
							Labels:    []string{fmt.Sprintf("step_%d", dep)},
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

				// REQ-SDK-007b: surface prior-step artifacts (predicate-filtered) into
				// working_memory as discovery refs. Best-effort — GetArtifact is the
				// authoritative gate. With no decision point wired this surfaces all
				// (the OSS default); with one, it surfaces only what the principal on
				// this context may see.
				if d.LedgerReader != nil {
					workingMemory = append(workingMemory, d.priorStepArtifactRefs(cancelCtx, plan.Steps[i].DependsOn)...)
				}
			}

			wg.Add(1)
			stepIdx := i
			stepWorkingMemory := workingMemory
			stepContextSnapshot := contextSnapshot
			// The plan VALUE at dispatch, not the variable: the coordinator reassigns
			// plan when a sibling's fan-out expands (applyExpandedPlan) or a replan
			// lands, while this goroutine may still be starting. ExpandFanOut clones,
			// so the captured plan is never mutated
			// (T-20261106-dag-executor-step-goroutine-reads-the-plan).
			stepPlan := plan
			fanOutChild := plan.Steps[i].FanOutItem != ""
			go func() {
				defer wg.Done()
				stepCtx := cancelCtx
				if fanOutChild {
					// Fan-out children run under the plan's fan-out bound
					// (execution.plan.fan_out_concurrency). The wait happens HERE, before
					// the step's deadline is stamped (dispatch), so a queued child does
					// not burn its step timeout waiting for a slot.
					select {
					case fanOutSem <- struct{}{}:
					case <-cancelCtx.Done():
						results <- stepResult{index: stepIdx, err: context.Cause(cancelCtx)}
						return
					}
					if d.FanOutConcurrency > 1 {
						// An agent process serves ONE Execute at a time (SDK runtime,
						// ADR-0036 D2), and every call to an agent id goes to its one
						// cached instance — so concurrently dispatched children of one
						// fan-out used to queue on that process and run strictly one
						// after another, ~11 s each, overrunning the 240 s chat turn
						// (T-20261107-plan-fan-out-steps-run-one-at-a-time-on-a). A
						// dedicated instance per child (the ADR-0139 D12 subagent
						// mechanism) is what lets the bound be real parallelism.
						stepCtx = domain.WithDedicatedInstance(stepCtx)
					}
				}
				res := d.executeStepTraced(stepCtx, stepPlan, stepIdx,
					stepContextSnapshot, stepWorkingMemory, deps, stepFn, planID)
				if fanOutChild {
					<-fanOutSem // free the slot before reporting, so the next child can start
				}
				results <- res
			}()
		}
	}

	dispatch() // seed: all root steps (DependsOn == nil/empty) go first

	// ADR-0049 A2.3: -1 means "no step had a merit history to predict from", which is
	// distinct from 0.0 ("predicted exactly right"). Collapsing the two would let an
	// unscored fleet look perfectly predictable and silence the gate.
	maxSurprise := -1.0
	// Labelled so an abort deep inside the pause/replan handling — including from within
	// the resume `select` — leaves the LOOP rather than the function. Everything after
	// the loop (drain, PlanEvent, plan outcome and episode close) must run exactly once per
	// Execute, on every one of these paths.
coordinate:
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
				// A plan whose context has ended — its ceiling fired (the caller-deadline
				// reserve, max duration or idle watchdog) or its caller went away — is
				// not replanned: the planner call would run on the dead context and
				// always fail ("ReplanHandler: planner call failed … context deadline
				// exceeded", 19 times in run 20261107T191751Z), logging an error and
				// spending the reserve the partial report needs. Leave the loop with
				// firstErr as it is, so the completion block returns the same
				// PartialPlanError — the completed steps' report
				// (T-20261108-replanhandler-replans-a-step-that-the-caller).
				if planErr := cancelCtx.Err(); planErr != nil {
					slog.Info("DAGExecutor: plan context ended; not replanning, reporting what the plan did",
						"failed_step", failedStepIdx, "cause", context.Cause(cancelCtx), "step_err", firstErr)
					break coordinate
				}
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
					// firstErr and failedStepIdx already hold everything the caller's
					// PartialPlanError is built from, so the completion block returns
					// the identical error — this exit only changes WHERE it returns from.
					break coordinate
				}

				if d.replanCount > d.MaxReplanAttempts {
					// The recovery ladder is spent. Falling through is the whole point:
					// the completion block reads the SAME counters to stamp
					// PlanOutcomeReplanExhausted, which is what lets the failure
					// precedent bypass the surprise gate (ADR-0049 A2.3 carve-out).
					break coordinate
				}

				// Loop guard: refuse a replan that just restates the step that failed.
				//
				// This MUST read the failing query before applyReplannedPlan, and must
				// decide before it too. applyReplannedPlan does `*plan = newPlan`, so
				// the old form — apply, then compare newPlan.Steps[0] against
				// plan.Steps[failedStepIdx] — compared the new plan with ITSELF. At
				// failedStepIdx 0, the overwhelmingly common failure position, that is
				// `newPlan.Steps[0] == newPlan.Steps[0]`: always true. Every step-0
				// replan aborted, including the ones that had produced a perfectly good
				// recovery step, so tiered recovery effectively did not exist. Observed
				// 2026-07-28: a replan offering "Check if the parent directory exists;
				// if not, create it" was rejected as a repeat of "Write the exact line
				// 'alpha record' to …".
				failedQuery := ""
				var failedStep domain.Step
				if failedStepIdx >= 0 && failedStepIdx < len(plan.Steps) {
					failedStep = plan.Steps[failedStepIdx]
					failedQuery = failedStep.Query
				}
				if len(newPlan.Steps) > 0 && failedQuery != "" && newPlan.Steps[0].Query == failedQuery &&
					!reroutedForCapability(firstErr, failedStep, newPlan.Steps[0]) {
					slog.Warn("Replan dry-run: new plan repeats same faulty step query",
						"step", newPlan.Steps[0].Query)
					// The rejection BECOMES the plan's error, exactly as the returned
					// PartialPlanError used to report it — so the completion block
					// records the same LastError the caller sees.
					if domain.IsDispatchRefusal(firstErr) {
						// Keep the refusal in the chain: the server turns it into a
						// question for the user rather than a generic failure.
						firstErr = fmt.Errorf("replan validation: replanned plan repeats the same step that failed: %s: %w", newPlan.Steps[0].Query, firstErr)
					} else {
						firstErr = fmt.Errorf("replan validation: replanned plan repeats the same step that failed: %s", newPlan.Steps[0].Query)
					}
					break coordinate
				}

				// Only now mutate: a rejected replan must leave the executor's state
				// untouched rather than half-swapped.
				if topoErr := d.applyReplannedPlan(newPlan, &plan, &n, alreadyDispatched, completed, &topoOrder,
					masterContext, prior, failureOf(firstErr, failedStepIdx)); topoErr != nil {
					// Returned verbatim (a topology error, not a PartialPlanError), but
					// still through the completion block.
					abortErr = topoErr
					break coordinate
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
					if err := d.applyReplannedPlan(hsPlan, &plan, &n, alreadyDispatched, completed, &topoOrder,
						masterContext, prior, failureOf(firstErr, failedStepIdx)); err != nil {
						abortErr = err
						break coordinate
					}
				}

				// ADR-0047 A1.1: fold an operator InjectCorrection into a new
				// forward plan. The Planner routes the injected NL step
				// (Zero-Hardcode); the swapped-out plan's results reach its root
				// steps as prior-plan StepDependency entries.
				if injection != "" {
					d.publishPlanState(ctx, planID, "replanning", lastActiveStep, runningCost, false, buildSteps())
					completedIdx := make([]int, 0, len(completed))
					for idx := range completed {
						completedIdx = append(completedIdx, idx)
					}
					injPlan := d.buildInjectionPlan(injection, masterContext, plan, completedIdx)
					// A failure belongs to the plan it happened in: when a hot-swap
					// was just applied, that plan was already captured with it.
					var injFail *stepFailure
					if hsPlan == nil {
						injFail = failureOf(firstErr, failedStepIdx)
					}
					if err := d.applyReplannedPlan(injPlan, &plan, &n, alreadyDispatched, completed, &topoOrder,
						masterContext, prior, injFail); err != nil {
						abortErr = err
						break coordinate
					}
				}

				if d.replanCount > 0 && (d.MaxReplanAttempts == 0 || d.replanCount > d.MaxReplanAttempts) {
					// Built HERE rather than left to the completion block: a hot-swap can
					// outrun the replan budget with no step error recorded at all, so
					// firstErr is nil and falling through would read as a success. The
					// value is identical to what this exit returned before.
					abortErr = &PartialPlanError{
						FailedStep:  failedStepIdx,
						LastError:   firstErr,
						Context:     cloneMap(masterContext),
						ReplanCount: d.replanCount,
					}
					break coordinate
				}

				dispatch()
				continue
			case <-cancelCtx.Done():
				// The plan's ctx ended while it was paused (its caller's deadline or
				// cancellation, or a plan ceiling). It used to RETURN the bare ctx error
				// from here, past the completion block: no PlanEvent, no plan outcome, and
				// no report — the caller learned nothing of the steps that had completed
				// (T-20261107-a-delegated-plan-stopped-by-its-caller-s). It now leaves the
				// loop like every other abort, as a PartialPlanError carrying the report.
				// cancelCtx is a child of ctx, so this case covers both.
				if firstErr == nil {
					firstErr = context.Cause(cancelCtx)
					failedStepIdx = -1 // no step failed: the plan was stopped between steps
				}
				break coordinate
			}
		}

		r := <-results
		inFlight--

		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
				failedStepIdx = r.index
				replan := d.ReplanHandler != nil && d.MaxReplanAttempts != 0
				// Effect-checked dispatch: an effect refusal gets exactly one replan.
				if replan && domain.IsDispatchRefusal(r.err) {
					if d.effectReplans >= 1 {
						slog.Warn("DAGExecutor: effect check refused a step again after its replan; ending the plan so the user is asked",
							"step", r.index, "err", r.err)
						replan = false
					} else {
						d.effectReplans++
					}
				}
				// If ReplanHandler is wired, pause for replanning instead of
				// immediately cancelling. In-flight siblings continue normally.
				if replan {
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

		// ADR-0145 D1: the step-output choke point, written before the merge and
		// outside the lock (it is I/O).
		mu.Lock()
		inputRefs := stepInputRefs(plan, r.index, masterContext)
		mu.Unlock()
		ledgerID := d.recordStepOutput(cancelCtx, plan, r, inputRefs)
		mu.Lock()
		cid := mergeStepResult(cancelCtx, masterContext, r, d.ContentStore, d.ContextRefSnippetChars)
		if cid != "" {
			planCIDs = append(planCIDs, cid)
		}
		if ledgerID != "" {
			masterContext[fmt.Sprintf("step_%d_cid", r.index)] = ledgerID
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

		// ADR-0049 A2.3: score this step against what merit predicted of its agent, and
		// keep the plan's WORST miss. The executing agent is the handoff's sender; the
		// step's first required capability scopes the expectation when ROUTE-06 has one.
		if d.Surprise != nil && r.resp != nil && r.resp.FromAgent != "" {
			var stepCap string
			if rc := plan.Steps[r.index].RequiredCapabilities; len(rc) > 0 {
				stepCap = rc[0]
			}
			if pe, ok := d.Surprise.PredictionError(ctx, r.resp.FromAgent, stepCap, r.err == nil); ok && pe > maxSurprise {
				maxSurprise = pe
			}
		}

		// Step results are no longer fed to memory: RecordExecution (the ADR-0015 Tier-1
		// channel and tier-2 judge, with its per-step SceneID and `follows` edges) was
		// deleted 2026-11-05 (decisions/scenes-cancelled.md).

		runningCost += r.estimatedCost
		completed[r.index] = true
		lastActiveStep = r.index
		d.publishPlanState(ctx, planID, "running", r.index, runningCost, false, buildSteps()) // ADR-0047 0047-17: step completed

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
			// WithoutCancel: the step DID complete, so its recovery point must be
			// recorded even if the run is being cancelled right now (§2.7).
			if err := d.CheckpointStore.SaveCheckpoint(context.WithoutCancel(ctx), d.CurrentRunID, r.index, cloneMap(masterContext)); err != nil {
				slog.Warn("checkpoint write failed", "run", d.CurrentRunID, "step", r.index, "err", err)
			}
		}

		// ADR-0078 R2: if parametric fan-out nodes are waiting on the step that just
		// completed, expand them now — a deterministic plan rewrite over the step's output,
		// so "discover N → do N" adapts the plan with no planner round-trip. EVERY pending
		// node over this source is expanded, in plan order: several parametric steps may
		// share one source, and expanding only the first left its siblings and their
		// dependents undispatched forever (T-20261107-only-the-first-fan-out-step-over-a-
		// source). Expansion appends and never renumbers, so expanding one after another on
		// the evolving plan keeps every index — and a dependent of two siblings ends up
		// barriering over both child sets. On over-width it is routed to replan (no silent
		// truncation); on any other extraction issue the node simply expands to whatever
		// items were found. domain.ResolveFanOut DEMOTES a node to one ordinary step —
		// dispatched rather than marked completed — when its query has no {item} token,
		// or when it is side-effecting and the source is not a structured list (free text
		// split into lines made every markdown heading a write child,
		// T-20261107-fan-out-over-a-prose-source-splits-markdown), or when the source
		// holds JSON with no recognisable item list (its lines made 26 read children for
		// 3 customers, T-20261107-read-only-fan-out-over-an-unrecognised-json).
		if firstErr == nil {
			srcOut := fanOutSourceOutput(masterContext[fmt.Sprintf("step_%d_result", r.index)])
			for _, fanIdx := range domain.PendingFanOuts(plan, r.index) {
				if completed[fanIdx] {
					continue // already expanded (an inert node keeps its FanOutOver tag)
				}
				expanded, items, how, expErr := domain.ResolveFanOut(plan, fanIdx, srcOut, d.MaxFanOutWidth)
				if expErr != nil {
					slog.Info("DAGExecutor: fan-out over-width; routing to replan",
						"src", r.index, "fan_step", fanIdx, "items", len(items), "err", expErr)
					firstErr = expErr
					failedStepIdx = fanIdx
					if d.ReplanHandler != nil && d.MaxReplanAttempts != 0 {
						d.pausedMu.Lock()
						d.paused = true
						d.errorPause = true
						d.pausedMu.Unlock()
					}
					break
				}
				firstChild := n
				if err := d.applyExpandedPlan(expanded, &plan, &n, completed, &topoOrder, fanIdx); err != nil {
					slog.Warn("DAGExecutor: fan-out expansion produced an invalid plan", "err", err)
					firstErr = err
					failedStepIdx = fanIdx
					break
				}
				d.expectFanOutMembers(plan, firstChild) // ADR-0145 D5.6
				switch {
				case how == domain.FanOutDemotedUntemplated:
					slog.Warn("DAGExecutor: fan-out step has no item placeholder; running it ONCE over the whole set instead of once per item",
						"src", r.index, "fan_step", fanIdx, "query", plan.Steps[fanIdx].Query)
				case how == domain.FanOutDemotedUnstructured:
					slog.Warn("DAGExecutor: side-effecting fan-out over a source that is not a structured list; running it ONCE over the whole source instead of splitting free text into items",
						"src", r.index, "fan_step", fanIdx, "source_bytes", len(srcOut), "query", plan.Steps[fanIdx].Query)
				case how == domain.FanOutDemotedUnrecognisedJSON:
					slog.Warn("DAGExecutor: fan-out source holds JSON but no recognisable item list; running the step ONCE over the whole source instead of once per line of JSON",
						"src", r.index, "fan_step", fanIdx, "source_bytes", len(srcOut), "source_head", fanOutSourceHead(srcOut))
				case len(items) == 0:
					// Nothing to fan out over: the inert node stays a dependency of its
					// dependents (domain.ExpandFanOut) and is recorded as an empty
					// result, so they are told it produced nothing.
					mu.Lock()
					masterContext[fmt.Sprintf("step_%d_result", fanIdx)] = ""
					delete(masterContext, fmt.Sprintf("step_%d_cid", fanIdx))
					mu.Unlock()
					slog.Info("DAGExecutor: fan-out expanded", "src", r.index, "fan_step", fanIdx, "children", 0,
						"source_bytes", len(srcOut), "source_head", fanOutSourceHead(srcOut))
				default:
					// The items and the source's size are logged so a wrong split is
					// diagnosable from the log alone (the step's answer_preview is 300
					// chars; T-20261107-read-only-fan-out-over-an-unrecognised-json).
					slog.Info("DAGExecutor: fan-out expanded", "src", r.index, "fan_step", fanIdx, "children", len(items),
						"source_bytes", len(srcOut), "items_head", fanOutItemsHead(items))
				}
			}
		}

		if firstErr == nil {
			dispatch() // unblock any steps that were waiting on this one
		}
	}

	// Drain unconditionally: goroutines may still be executing defer wg.Done()
	// after sending to results. This prevents goroutine leaks on any exit path.
	//
	// "Any exit path" is only true because the replan aborts `break coordinate` rather
	// than returning from inside the loop — they used to return past this line (and past
	// everything below it). A new abort must break, not return.
	wg.Wait()

	// ADR-0021: Write plan-level telemetry.
	planEndTime := time.Now()
	planDurationMs := planEndTime.Sub(planStartTime).Milliseconds()
	// An abandoned loop did not complete, even when no single step reported an error —
	// a hot-swap that outran its replan budget is exactly that shape.
	planFailed := firstErr != nil || abortErr != nil
	outcome := domain.PlanOutcomeSuccess
	if planFailed {
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
		// Plan telemetry records how the plan ended, cancellation included, so
		// it must not be dropped with the cancelled request (AGENTS.md §2.7).
		if err := d.PlanEventWriter.WritePlanEvent(context.WithoutCancel(ctx), planEvent); err != nil {
			slog.Warn("DAGExecutor: failed to write PlanEvent", "plan_id", planID, "err", err)
		}
	}
	if d.Observer != nil {
		d.Observer.OnPlanCompleted(ctx, planEvent)
	}

	// ADR-0049 A2.2 / ADR-0095: the plan-outcome hook — close the episode, write the
	// A2.3 failure record, feed the procedures — for BOTH success and failure.
	if d.MemoryRecorder != nil {
		// WithoutCancel (AGENTS.md §2.7): a plan stopped by its caller's deadline or
		// cancellation still has an outcome to record — that is when ctx is already
		// done, and the write would otherwise be dropped. Values (scope, session) stay.
		_ = d.MemoryRecorder.RecordPlanOutcome(context.WithoutCancel(ctx), domain.PlanRecord{
			PlanID:   planID,
			Goal:     plan.Subject,
			Success:  !planFailed,
			Surprise: maxSurprise,
			// ADR-0094 D8: the routines that informed this plan, so the memory agent
			// can feed their confidence from how it turned out. Without this the
			// co-evolution branch in RecordPlanOutcome is unreachable — the field was
			// read but never written, so no routine ever learned from an outcome.
			FollowedProcedures: plan.FollowedProcedures,
			// ADR-0049 A2.2: the failure MODE, not only the situation. Both values exist
			// here and nowhere downstream, so without this hop the failure precedent
			// records what was being attempted and never what went wrong — it cannot warn
			// about a repeat of the error it was written for.
			FailedStep:     failedStepIdx,
			FailureSummary: summarizeFailure(plan.Steps, failedStepIdx, firstErr),
			// Read off the outcome already computed above rather than re-testing the
			// replan counters, so the record and the PlanEvent can never disagree about
			// whether the recovery ladder was exhausted.
			ReplanExhausted: outcome == domain.PlanOutcomeReplanExhausted,
		})
	}

	// An abort carries its own error verbatim — a topology error stays a topology error,
	// and the replan-budget exit keeps the PartialPlanError it built at the exit site.
	if abortErr != nil {
		var pe *PartialPlanError
		if errors.As(abortErr, &pe) && pe.Report == "" {
			pe.Report = buildPlanReport(plan, masterContext, completed)
		}
		if pe != nil && pe.Context != nil {
			// ADR-0145 D8: every step of the final plan, for the structured result.
			pe.Context[PlanStepsKey] = encodeStepRefs(planStepRefs(plan, masterContext))
		}
		return nil, abortErr
	}

	if firstErr != nil {
		pe := &PartialPlanError{
			FailedStep:  failedStepIdx,
			LastError:   firstErr,
			Context:     cloneMap(masterContext),
			ReplanCount: d.replanCount,
			Report:      buildPlanReport(plan, masterContext, completed),
		}
		pe.Context[PlanStepsKey] = encodeStepRefs(planStepRefs(plan, masterContext))
		return nil, pe
	}

	masterContext[finalResultKey] = buildPlanReport(plan, masterContext, completed)
	masterContext[PlanStepsKey] = encodeStepRefs(planStepRefs(plan, masterContext))
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
	// Report is buildPlanReport over the steps that completed before the plan stopped
	// (its completed frontier), so the caller is told what DID happen, not only what
	// failed. "" when nothing completed.
	Report string
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

const (
	// maxFailureSummaryRunes bounds the whole rendered failure mode. Wide enough to
	// carry a step query plus the error that identifies the failure, narrow enough that
	// one stack-shaped error cannot dominate the record it is a field of.
	maxFailureSummaryRunes = 300
	// maxFailureQueryRunes bounds the step-query head inside it, so a long query cannot
	// crowd out the error text — the error is the part a precedent is matched on.
	maxFailureQueryRunes = 80
)

// summarizeFailure renders a plan's first error as the deterministic failure mode of its
// precedent (ADR-0049 A2.2): "step N (<query head>): <error head>", degrading to
// "step N: <error head>" when idx is outside the plan (a replan reassigns plan.Steps) or
// the step carries no query.
//
// Pure: no clock, no LLM, and nothing stripped beyond the rune bounds, so the same
// failure renders identically on every occurrence — which is what makes two occurrences
// matchable. Bounds cut on rune boundaries; a byte slice would split a multi-byte rune
// and produce a different string for the same error depending on where it landed.
func summarizeFailure(steps []domain.Step, idx int, err error) string {
	if err == nil {
		return ""
	}
	head := fmt.Sprintf("step %d", idx)
	if idx >= 0 && idx < len(steps) {
		if q := strings.TrimSpace(steps[idx].Query); q != "" {
			head += " (" + boundRunes(q, maxFailureQueryRunes) + ")"
		}
	}
	return boundRunes(head+": "+err.Error(), maxFailureSummaryRunes)
}

// boundRunes caps s at n runes, cutting on a rune boundary.
func boundRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// setCurrentPlan records the plan being run (ADR-0145 D6's dependency walk).
func (d *DAGExecutor) setCurrentPlan(p *domain.ExecutionPlan) {
	d.planMu.Lock()
	d.currentPlan = p
	d.planMu.Unlock()
}

// reroutedForCapability reports a replan that keeps the failed step's words but
// changes what it requires, after the step failed for a missing capability. That
// is the re-plan the capability_missing outcome asks for ("re-plan that step";
// the kernel composes the new worker from the changed requirements,
// T-20261112-yield-sub-goal-on-a-composed-lease-runs-under), not a restatement of
// the step that failed, so the loop guard lets it through. Any other failure, or
// an unchanged requirement set, is still refused.
func reroutedForCapability(err error, failed, next domain.Step) bool {
	var cm *domain.CapabilityMissingError
	if !errors.As(err, &cm) {
		return false
	}
	return !sameSet(failed.RequiredCapabilities, next.RequiredCapabilities) ||
		!sameSet(failed.RequiredEffects, next.RequiredEffects)
}

// sameSet compares two string lists as sets, normalised as capabilities are.
func sameSet(a, b []string) bool {
	as, bs := domain.NormalizeCapabilities(a), domain.NormalizeCapabilities(b)
	if len(as) != len(bs) {
		return false
	}
	in := make(map[string]bool, len(as))
	for _, x := range as {
		in[x] = true
	}
	for _, x := range bs {
		if !in[x] {
			return false
		}
	}
	return true
}

// priorStepArtifactRefs surfaces the artifacts the given upstream steps produced
// (run-ledger artifact entries of this run, ADR-0157 D5.2) into working memory
// as discovery refs: the CID is the ledger id, which GetArtifact / ledger_get
// read under the consumer's own scope. The entries are listed through the
// ENFORCED reader under the principal's read predicate, so an artifact the
// principal may not read is not even named. Best-effort: a failed lookup
// surfaces nothing for that step.
func (d *DAGExecutor) priorStepArtifactRefs(ctx context.Context, deps []int) []domain.ContextRef {
	if len(deps) == 0 {
		return nil
	}
	eff := domain.ScopeSystem
	if d.Authz != nil {
		eff, _ = d.Authz.ReadFilter(ctx, domain.PrincipalFromContext(ctx), domain.SurfaceFromContext(ctx))
	}
	if eff == nil {
		return nil // denied outright: surface nothing (fail closed)
	}
	rctx := domain.WithScope(ctx, eff)
	run := d.ledgerProvenance(ctx).RunID
	if run == "" {
		return nil
	}
	var out []domain.ContextRef
	for _, dep := range deps {
		rows, err := d.LedgerReader.Find(rctx, domain.LedgerQuery{RunID: run, StepIndex: dep, HasStep: true,
			Kind: domain.LedgerArtifact})
		if err != nil {
			continue
		}
		for _, e := range rows {
			labels := append([]string{fmt.Sprintf("step_%d", dep)}, e.Tags...)
			out = append(out, domain.ContextRef{CID: domain.CID(e.ID), Type: "agent_artifact", Labels: labels,
				Precision: 1.0, Snippet: e.Preview})
		}
	}
	return out
}
