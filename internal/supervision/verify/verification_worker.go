package verify

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// convergenceBound is the number of verified events needed for a fully dishonest
// agent (signal=0 at every event) to decay TrustScore below 0.1 at alpha=0.5.
const convergenceBound = 4

// DefaultVerificationQueueCapacity is the default buffer size for the
// verification channel.
const DefaultVerificationQueueCapacity = 256

// VerificationWorkerConfig is the subset of config fields used by VerificationWorker.
type VerificationWorkerConfig struct {
	QueueCapacity         int
	VerifierRecencyWindow int
	TrustBoostThreshold   float64
	CrossVerifyRate       float64
	// LeaseTokenLimit / LeaseDuration size the lease minted for each verifier
	// call. The kernel sets them to the agent step lease's sizing
	// (network.StepLeaseTokenLimit / StepLeaseDuration): a verifier call is one
	// agent turn, so it is budgeted like one.
	LeaseTokenLimit int
	LeaseDuration   time.Duration
}

// LeaseGateway is the managed-LLM lease lifecycle the worker needs — a narrow
// view of the substrate LLM gateway, the SAME Acquire → BindLease → Complete
// path every other lease site uses (agent steps, interview, chat, retrieval
// dispatch). Complete runs the gateway's completion hook, so a verifier lease
// is cleaned up exactly like a step lease.
type LeaseGateway interface {
	Acquire(ctx context.Context, sa domain.StepAllocation, tokenLimit int, estimatedDuration time.Duration) (domain.LeaseID, error)
	BindLease(leaseID domain.LeaseID, b domain.LeaseBinding)
	Complete(ctx context.Context, leaseID domain.LeaseID) (llm.TokenUsage, error)
}

// VerificationWorker is the background component that receives sampled task
// completions, selects a verifier, scores the output, updates the TaskEvent,
// and stores a Judicial Record.
type VerificationWorker struct {
	Pool          *VerifierPool
	Requester     domain.VerifyRequester
	EventStore    domain.TaskEventReadWriter
	JudicialStore domain.JudicialStore
	ProfileStore  domain.VerifierProfileStore
	Embedder      domain.Embedder
	Config        VerificationWorkerConfig
	// EventBus is optional (ADR-0047 D3); when set, a completed verification
	// round publishes a VerifierRoundEvent for the operator feed. nil ⇒ no-op.
	EventBus domain.EventBus
	// leases mints the managed-LLM lease each verifier call carries (ADR-0131;
	// T-20261101-verification-mints-no-lease-so-verifier-llm). Constructor-
	// injected; nil only where no gateway exists (unit tests, maintenance
	// tools), and then the verifier is called without a lease as before.
	leases LeaseGateway

	queue        chan domain.VerificationRequest
	droppedCount atomic.Int64
}

// NewVerificationWorker creates a VerificationWorker with a buffered queue.
// leases may be nil (no gateway): verifier calls then carry no lease.
func NewVerificationWorker(
	pool *VerifierPool,
	requester domain.VerifyRequester,
	events domain.TaskEventReadWriter,
	judicial domain.JudicialStore,
	profiles domain.VerifierProfileStore,
	embedder domain.Embedder,
	leases LeaseGateway,
	cfg VerificationWorkerConfig,
) *VerificationWorker {
	cap := cfg.QueueCapacity
	if cap <= 0 {
		cap = DefaultVerificationQueueCapacity
	}
	return &VerificationWorker{
		Pool:          pool,
		Requester:     requester,
		EventStore:    events,
		JudicialStore: judicial,
		ProfileStore:  profiles,
		Embedder:      embedder,
		Config:        cfg,
		leases:        leases,
		queue:         make(chan domain.VerificationRequest, cap),
	}
}

// Enqueue decides whether to sample this task completion for verification.
func (w *VerificationWorker) Enqueue(ctx context.Context, taskID, agentID string, req, resp *domain.Handoff) bool {
	sourceHash, trustScore := w.agentMeta(ctx, agentID)

	if !shouldSample(taskID, trustScore, w.Config.TrustBoostThreshold) {
		return false
	}

	vreq := domain.VerificationRequest{
		TaskID:        taskID,
		AgentID:       agentID,
		SourceHash:    sourceHash,
		BidConfidence: handoffConfidence(resp),
		Request:       req,
		Response:      resp,
		SessionID:     sessionOf(ctx),
	}

	select {
	case w.queue <- vreq:
		return true
	default:
		w.droppedCount.Add(1)
		slog.Warn("VerificationWorker: enqueue dropped — queue full",
			"task_id", taskID, "agent_id", agentID)
		return false
	}
}

// DroppedCount returns the total number of dropped verification requests.
func (w *VerificationWorker) DroppedCount() int64 {
	return w.droppedCount.Load()
}

// Start launches the worker loop. It blocks until ctx is cancelled.
func (w *VerificationWorker) Start(ctx context.Context) error {
	const (
		maxRestarts    = 5
		windowDuration = 60 * time.Second
		maxBackoff     = 30 * time.Second
	)
	var crashTimestamps []time.Time

	slog.Info("VerificationWorker: starting loop")
	for w.runLoop(ctx) {
		now := time.Now()
		crashTimestamps = append(crashTimestamps, now)

		cutoff := now.Add(-windowDuration)
		kept := crashTimestamps[:0]
		for _, ts := range crashTimestamps {
			if ts.After(cutoff) {
				kept = append(kept, ts)
			}
		}
		crashTimestamps = kept

		if len(crashTimestamps) >= maxRestarts {
			slog.Error("VerificationWorker: max restarts exceeded, stopping worker",
				"crash_count", len(crashTimestamps),
				"window_seconds", windowDuration/time.Second)
			return nil
		}

		backoff := time.Duration(1<<len(crashTimestamps)) * time.Second
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		slog.Warn("VerificationWorker: panic recovered, restarting with backoff",
			"crash_count", len(crashTimestamps),
			"backoff_seconds", backoff/time.Second)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
	}
	return nil
}

func (w *VerificationWorker) runLoop(ctx context.Context) (restart bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("VerificationWorker: panic recovered, restarting", "panic", r)
			restart = true
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return false
		case vreq, ok := <-w.queue:
			if !ok {
				return false
			}
			w.processOne(ctx, vreq)
		}
	}
}

func (w *VerificationWorker) processOne(ctx context.Context, vreq domain.VerificationRequest) {
	task := &domain.DispatchTask{ID: vreq.TaskID, Description: vreq.TaskID}

	subjectProfile, _ := w.ProfileStore.GetProfile(ctx, vreq.AgentID, vreq.SourceHash)

	verifier, err := w.Pool.Select(ctx, task, vreq.AgentID, subjectProfile)
	if err != nil {
		slog.Warn("VerificationWorker: no verifier available",
			"task_id", vreq.TaskID, "agent_id", vreq.AgentID, "err", err)
		return
	}

	callCtx, cancel := context.WithTimeout(spendCtx(ctx, vreq), 10*time.Second)
	defer cancel()
	lease, endLease := w.mintLease(callCtx, vreq, verifier.ID)
	defer endLease()

	originalQuery := ""
	if vreq.Request != nil && vreq.Request.Payload != nil {
		originalQuery = string(vreq.Request.Payload.Data)
	}
	winnerOutput := ""
	if vreq.Response != nil && vreq.Response.Payload != nil {
		winnerOutput = string(vreq.Response.Payload.Data)
	}

	verifyResp, err := w.Requester.VerifyOutput(callCtx, *verifier, domain.VerifyRequest{
		TaskID:        vreq.TaskID,
		OriginalQuery: originalQuery,
		WinnerOutput:  winnerOutput,
		WinnerAgentID: vreq.AgentID,
		BidConfidence: float32(vreq.BidConfidence),
		LeaseID:       lease,
	})
	if err != nil {
		slog.Warn("VerificationWorker: verifier call failed",
			"task_id", vreq.TaskID, "verifier_id", verifier.ID, "err", err)
		return
	}

	verifierScore := clamp(float64(verifyResp.QualityScore), 0, 1)
	critique := verifyResp.Critique

	w.updateTaskEvent(vreq.TaskID, verifierScore)
	w.storeJudicialRecord(ctx, vreq, verifierScore, critique)
	w.updateRecentVerifiers(ctx, vreq.AgentID, vreq.SourceHash, verifier.ID, subjectProfile)
	w.publishRound(vreq, verifierScore, critique)

	if w.Config.CrossVerifyRate > 0 && shouldCrossVerify(vreq.TaskID, verifier.ID, w.Config.CrossVerifyRate) {
		w.crossVerify(ctx, vreq, verifier, verifierScore, "crossverify-"+vreq.TaskID)
	}
}

// publishRound emits a VerifierRoundEvent for the operator feed (ADR-0047 D3).
// Best-effort and nil-safe — verification correctness never depends on it.
func (w *VerificationWorker) publishRound(vreq domain.VerificationRequest, score float64, critique string) {
	if w.EventBus == nil {
		return
	}
	_ = w.EventBus.Publish(domain.VerifierRoundEvent{
		TaskID:       vreq.TaskID,
		WinnerAgent:  vreq.AgentID,
		QualityScore: score,
		BidConf:      vreq.BidConfidence,
		Critique:     critique,
	})
}

func (w *VerificationWorker) crossVerify(
	ctx context.Context,
	vreq domain.VerificationRequest,
	primaryVerifier *domain.AgentDefinition,
	primaryVerifierConfidence float64,
	crossVerifyTaskID string,
) {
	crossExcludeProfile := &domain.AgentProfile{
		RecentVerifierIDs: []string{primaryVerifier.ID},
	}
	task := &domain.DispatchTask{ID: crossVerifyTaskID, Description: crossVerifyTaskID}

	crossVerifier, err := w.Pool.Select(ctx, task, vreq.AgentID, crossExcludeProfile)
	if err != nil {
		slog.Debug("VerificationWorker: no cross-verifier available",
			"task_id", vreq.TaskID, "primary_verifier", primaryVerifier.ID, "err", err)
		return
	}

	callCtx, cancel := context.WithTimeout(spendCtx(ctx, vreq), 10*time.Second)
	defer cancel()
	lease, endLease := w.mintLease(callCtx, vreq, crossVerifier.ID)
	defer endLease()

	crossVerifyPrompt := buildCrossVerificationPrompt(vreq, primaryVerifier.ID, primaryVerifierConfidence)
	originalQuery := ""
	if vreq.Request != nil && vreq.Request.Payload != nil {
		originalQuery = string(vreq.Request.Payload.Data)
	}

	crossResp, err := w.Requester.VerifyOutput(callCtx, *crossVerifier, domain.VerifyRequest{
		TaskID:        crossVerifyTaskID,
		OriginalQuery: originalQuery,
		WinnerOutput:  crossVerifyPrompt,
		WinnerAgentID: primaryVerifier.ID,
		BidConfidence: float32(primaryVerifierConfidence),
		LeaseID:       lease,
	})
	if err != nil {
		slog.Warn("VerificationWorker: cross-verifier call failed",
			"task_id", vreq.TaskID, "cross_verifier", crossVerifier.ID, "err", err)
		return
	}

	crossScore := clamp(float64(crossResp.QualityScore), 0, 1)

	event := domain.TaskEvent{
		TaskID:        crossVerifyTaskID,
		AgentID:       primaryVerifier.ID,
		SourceHash:    primaryVerifier.SourceHash,
		BidConfidence: primaryVerifierConfidence,
		VerifierScore: crossScore,
		Verified:      true,
	}
	if err := w.EventStore.WriteTaskEvent(event); err != nil {
		slog.Warn("VerificationWorker: failed to write cross-verification TaskEvent",
			"task_id", crossVerifyTaskID, "err", err)
	}
	slog.Debug("VerificationWorker: cross-verification complete",
		"original_task_id", vreq.TaskID,
		"cross_verify_task_id", crossVerifyTaskID,
		"primary_verifier", primaryVerifier.ID,
		"cross_verifier", crossVerifier.ID,
		"cross_score", crossScore,
	)
}

func (w *VerificationWorker) updateTaskEvent(taskID string, verifierScore float64) {
	existing, err := w.EventStore.ReadTaskEvent(taskID)
	if err != nil || existing == nil {
		slog.Warn("VerificationWorker: TaskEvent not found for update", "task_id", taskID)
		return
	}
	existing.VerifierScore = verifierScore
	existing.Verified = true
	if err := w.EventStore.WriteTaskEvent(*existing); err != nil {
		slog.Warn("VerificationWorker: failed to update TaskEvent",
			"task_id", taskID, "err", err)
	}
}

func (w *VerificationWorker) storeJudicialRecord(ctx context.Context, vreq domain.VerificationRequest, score float64, critique string) {
	if critique == "" {
		return
	}
	embedding, err := w.Embedder.Embed(ctx, critique)
	if err != nil {
		slog.Warn("VerificationWorker: critique embedding failed",
			"task_id", vreq.TaskID, "err", err)
		return
	}
	metadata := map[string]any{
		"document_type":  "judicial_record",
		"agent_id":       vreq.AgentID,
		"source_hash":    vreq.SourceHash,
		"task_id":        vreq.TaskID,
		"verifier_score": fmt.Sprintf("%.4f", score),
	}
	if err := w.JudicialStore.Save(ctx, critique, embedding, metadata); err != nil {
		slog.Warn("VerificationWorker: judicial record store failed",
			"task_id", vreq.TaskID, "err", err)
	}
}

func (w *VerificationWorker) updateRecentVerifiers(ctx context.Context, agentID, sourceHash, verifierID string, profile *domain.AgentProfile) {
	if profile == nil {
		return
	}
	profile.RecentVerifierIDs = prependVerifierID(
		profile.RecentVerifierIDs, verifierID, w.Config.VerifierRecencyWindow,
	)
	if err := w.ProfileStore.SaveProfile(ctx, agentID, sourceHash, nil, *profile); err != nil {
		slog.Warn("VerificationWorker: failed to update RecentVerifierIDs",
			"agent_id", agentID, "err", err)
	}
}

func shouldCrossVerify(taskID, verifierID string, rate float64) bool {
	if rate <= 0 {
		return false
	}
	if rate >= 1.0 {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(taskID + ":crossverify:" + verifierID))
	return float64(h.Sum32()%1000) < rate*1000
}

func shouldSample(taskID string, trustScore, boostThreshold float64) bool {
	if trustScore < boostThreshold {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(taskID))
	return h.Sum32()%10 == 0
}

func (w *VerificationWorker) agentMeta(ctx context.Context, agentID string) (sourceHash string, trustScore float64) {
	def, err := w.Pool.Registry.GetAgentByName(ctx, agentID)
	if err != nil || def == nil {
		return "", 0.5
	}
	sourceHash = def.SourceHash

	profile, err := w.ProfileStore.GetProfile(ctx, agentID, sourceHash)
	if err != nil || profile == nil {
		return sourceHash, 0.5
	}
	return sourceHash, profile.TrustScore
}

func handoffConfidence(resp *domain.Handoff) float64 {
	if resp == nil {
		return 0
	}
	return float64(resp.Confidence)
}

func prependVerifierID(ids []string, id string, window int) []string {
	result := append([]string{id}, ids...)
	if window > 0 && len(result) > window {
		result = result[:window]
	}
	return result
}

func buildCrossVerificationPrompt(vreq domain.VerificationRequest, primaryVerifierID string, primaryConfidence float64) string {
	req := ""
	if vreq.Request != nil && vreq.Request.Payload != nil {
		req = string(vreq.Request.Payload.Data)
	}
	resp := ""
	if vreq.Response != nil && vreq.Response.Payload != nil {
		resp = string(vreq.Response.Payload.Data)
	}
	return fmt.Sprintf(
		"Cross-verify the following verifier assessment. Score 0-1 for quality of the verification.\nOriginal task: %s\nOriginal response: %s\nVerifier %s assessed this with confidence %.2f. Evaluate the quality of that assessment.",
		req, resp, primaryVerifierID, primaryConfidence,
	)
}

func buildVerificationPrompt(vreq domain.VerificationRequest) string {
	req := ""
	if vreq.Request != nil && vreq.Request.Payload != nil {
		req = string(vreq.Request.Payload.Data)
	}
	resp := ""
	if vreq.Response != nil && vreq.Response.Payload != nil {
		resp = string(vreq.Response.Payload.Data)
	}
	return fmt.Sprintf("Verify the following task completion. Score 0-1 and provide a critique.\nTask: %s\nResponse: %s", req, resp)
}

// sessionOf reads the originating session from the enqueuing ctx ("" if none).
func sessionOf(ctx context.Context) domain.SessionID {
	sid, _ := domain.SessionIDFromContext(ctx)
	return sid
}

// mintLease issues the managed-LLM lease one verifier call carries, through the
// gateway's one lease path: Acquire under callCtx (which carries the
// background-spend marker, so the gateway books the lease's calls to the
// session and never refuses them at its ceiling), BindLease to the ORIGINATING
// session — the binding is what the gateway attributes the verifier's
// generate calls to, since the verifier calls back over its own RPC and no
// kernel ctx crosses that hop; an empty session books to the kernel no-session
// bucket — and the returned end func Completes it, which also runs the
// gateway's completion hook. A failed Acquire degrades to a lease-less call
// (the pre-fix behaviour), logged, never a dropped verification.
func (w *VerificationWorker) mintLease(callCtx context.Context, vreq domain.VerificationRequest, verifierID string) (domain.LeaseID, func()) {
	if w.leases == nil {
		return "", func() {}
	}
	// Empty StepAllocation ⇒ the gateway's configured default model, as for
	// chat and retrieval-dispatch leases.
	lease, err := w.leases.Acquire(callCtx, domain.StepAllocation{}, w.Config.LeaseTokenLimit, w.Config.LeaseDuration)
	if err != nil || lease == "" {
		slog.Warn("VerificationWorker: lease acquire failed; verifier called without a lease",
			"task_id", vreq.TaskID, "verifier_id", verifierID, "err", err)
		return "", func() {}
	}
	w.leases.BindLease(lease, domain.LeaseBinding{SessionID: vreq.SessionID, StepIndex: -1, AgentID: verifierID})
	return lease, func() {
		// WithoutCancel: the call ctx may already be past its deadline, and the
		// lease must be completed regardless (AGENTS.md §2.7).
		if _, err := w.leases.Complete(context.WithoutCancel(callCtx), lease); err != nil {
			slog.Debug("VerificationWorker: lease complete failed", "lease", lease, "err", err)
		}
	}
}

// spendCtx is the ctx of a verifier call: the worker's own ctx (never the
// long-gone request ctx, never context.Background()) carrying the originating
// session and the background-spend marker, so whatever LLM spend the call
// causes is booked to that session and never refused at its ceiling — a
// refusal would silently drop the verification (ADR-0131, owner decision
// 2026-11-01). Only the verifier call gets it: the judicial/profile writes
// keep the worker ctx, so their scope and stamping are unchanged.
func spendCtx(ctx context.Context, vreq domain.VerificationRequest) context.Context {
	ctx = domain.WithBackgroundSpend(ctx)
	if vreq.SessionID != "" {
		ctx = domain.WithSessionID(ctx, vreq.SessionID)
	}
	return ctx
}
