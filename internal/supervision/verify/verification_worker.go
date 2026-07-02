package verify

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
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

	queue        chan domain.VerificationRequest
	droppedCount atomic.Int64
}

// NewVerificationWorker creates a VerificationWorker with a buffered queue.
func NewVerificationWorker(
	pool *VerifierPool,
	requester domain.VerifyRequester,
	events domain.TaskEventReadWriter,
	judicial domain.JudicialStore,
	profiles domain.VerifierProfileStore,
	embedder domain.Embedder,
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
	task := &domain.AuctionTask{ID: vreq.TaskID, Description: vreq.TaskID}

	subjectProfile, _ := w.ProfileStore.GetProfile(ctx, vreq.AgentID, vreq.SourceHash)

	verifier, err := w.Pool.Select(ctx, task, vreq.AgentID, subjectProfile)
	if err != nil {
		slog.Warn("VerificationWorker: no verifier available",
			"task_id", vreq.TaskID, "agent_id", vreq.AgentID, "err", err)
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

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
	task := &domain.AuctionTask{ID: crossVerifyTaskID, Description: crossVerifyTaskID}

	crossVerifier, err := w.Pool.Select(ctx, task, vreq.AgentID, crossExcludeProfile)
	if err != nil {
		slog.Debug("VerificationWorker: no cross-verifier available",
			"task_id", vreq.TaskID, "primary_verifier", primaryVerifier.ID, "err", err)
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

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
