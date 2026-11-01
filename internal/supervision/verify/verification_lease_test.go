package verify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/substrate/network"
)

// Tests for T-20261101-verification-mints-no-lease-so-verifier-llm (ADR-0131,
// owner decision 2026-11-01): each verifier call carries a lease minted through
// the REAL substrate gateway, bound to the originating session, booked there but
// never refused at its ceiling, and completed afterwards.

// usageModel answers with fixed text and reports fixed provider usage.
type usageModel struct{ prompt, compl int }

func (u usageModel) GenerateStream(context.Context, string) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk, 2)
	ch <- domain.StreamChunk{Text: "0.9 fine", IsFinal: true}
	ch <- domain.StreamChunk{IsFinal: true, UsagePromptTokens: u.prompt, UsageCompletionTokens: u.compl,
		UsageTotalTokens: u.prompt + u.compl}
	close(ch)
	return ch, nil
}

// generatingVerifier plays a verifier agent: it generates through the gateway
// under the lease the kernel handed it. Its ctx is a FRESH one — on the real
// path the verifier calls back over its own RPC, so no kernel ctx (session,
// background marker) crosses the hop; only the lease does.
type generatingVerifier struct {
	gw     *network.SubstrateLLMGateway
	mu     sync.Mutex
	leases []domain.LeaseID
	errs   []error
}

func (v *generatingVerifier) VerifyOutput(_ context.Context, _ domain.AgentDefinition, req domain.VerifyRequest) (domain.VerifyResponse, error) {
	out := make(chan domain.StreamChunk, 16)
	err := v.gw.StreamChunks(context.Background(), req.LeaseID, "verify this", domain.GenerateOptions{}, out)
	v.mu.Lock()
	v.leases = append(v.leases, req.LeaseID)
	v.errs = append(v.errs, err)
	v.mu.Unlock()
	return domain.VerifyResponse{QualityScore: 0.9, Critique: "fine"}, nil
}

type leaseRig struct {
	meter     *llm.SpendMeter
	gw        *network.SubstrateLLMGateway
	verifier  *generatingVerifier
	worker    *VerificationWorker
	completed chan domain.LeaseID
}

// newLeaseRig: ceiling 100 tokens; every verifier generate books 150.
func newLeaseRig(t *testing.T, crossVerifyRate float64) *leaseRig {
	t.Helper()
	meter := llm.NewSpendMeter(100, nil, nil)
	gw := network.NewLLMGateway(config.DefaultConfig().Execution)
	gw.RegisterModelClient("llm:fake", usageModel{prompt: 120, compl: 30})
	gw.SetDefaultModelID("llm:fake")
	gw.SetSpendMeter(meter)
	completed := make(chan domain.LeaseID, 8)
	gw.SetCompleteHook(func(id domain.LeaseID) { completed <- id })

	profiles := map[string]*domain.AgentProfile{
		"verifier1:vh1": {TrustScore: 0.9, SuccessRate: 0.9},
		"verifier2:vh2": {TrustScore: 0.9, SuccessRate: 0.9},
	}
	pool := &VerifierPool{
		Registry: newAgentSourceWith(
			domain.AgentDefinition{ID: "verifier1", SourceHash: "vh1"},
			domain.AgentDefinition{ID: "verifier2", SourceHash: "vh2"},
			domain.AgentDefinition{ID: "subject", SourceHash: "sh1"},
		),
		Profiles:      &vwMockGatekeeperReader{profiles: profiles},
		Threshold:     0.8,
		RecencyWindow: 3,
	}
	v := &generatingVerifier{gw: gw}
	w := NewVerificationWorker(pool, v, &vwMockTaskEventRW{events: map[string]domain.TaskEvent{}},
		&vwMockJudicialStore{}, &vwMockProfileStore{profiles: map[string]*domain.AgentProfile{"subject:sh1": {TrustScore: 0.2}}},
		&mockEmbedder{}, gw,
		VerificationWorkerConfig{QueueCapacity: 4, VerifierRecencyWindow: 3, TrustBoostThreshold: 0.4,
			CrossVerifyRate: crossVerifyRate,
			LeaseTokenLimit: network.StepLeaseTokenLimit, LeaseDuration: network.StepLeaseDuration})
	return &leaseRig{meter: meter, gw: gw, verifier: v, worker: w, completed: completed}
}

func (r *leaseRig) assertAllCompleted(t *testing.T, want int) {
	t.Helper()
	if n := r.gw.SessionCount(); n != 0 {
		t.Fatalf("%d lease(s) left in the gateway after verification; every verifier lease must be completed", n)
	}
	r.verifier.mu.Lock()
	issued := append([]domain.LeaseID(nil), r.verifier.leases...)
	r.verifier.mu.Unlock()
	if len(issued) != want {
		t.Fatalf("verifier calls = %d, want %d", len(issued), want)
	}
	for _, id := range issued {
		if id == "" {
			t.Fatal("a verifier call carried no lease")
		}
		select {
		case got := <-r.completed:
			if !contains(issued, got) {
				t.Fatalf("completion hook ran for unknown lease %q", got)
			}
		case <-time.After(time.Second):
			t.Fatalf("completion hook never ran for lease %q", id)
		}
	}
}

func contains(ids []domain.LeaseID, id domain.LeaseID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// A verification of a session already OVER its ceiling still reaches the model:
// both the primary and the cross-verifier generate, both calls are booked to the
// originating session, and both leases are completed.
func TestVerificationLease_BookedToSessionNeverRefusedAtCeiling(t *testing.T) {
	r := newLeaseRig(t, 1.0)
	const sid domain.SessionID = "ses-over"
	r.meter.RecordSession(sid, "agent_step", "llm:fake", llm.TokenUsage{PromptTokens: 150, TotalTokens: 150})

	// Control: an ordinary (request-path) lease on the same session IS refused —
	// the ceiling is live, so the verifier's admission below is the background rule.
	ctl, _ := r.gw.Acquire(context.Background(), domain.StepAllocation{}, network.StepLeaseTokenLimit, network.StepLeaseDuration)
	r.gw.BindLease(ctl, domain.LeaseBinding{SessionID: sid, StepIndex: 0})
	if err := r.gw.StreamChunks(context.Background(), ctl, "p", domain.GenerateOptions{}, make(chan domain.StreamChunk, 16)); !errors.Is(err, llm.ErrSpendCeiling) {
		t.Fatalf("control lease: err = %v, want ErrSpendCeiling", err)
	}
	if _, err := r.gw.Complete(context.Background(), ctl); err != nil {
		t.Fatal(err)
	}
	<-r.completed

	h := &domain.Handoff{}
	if !r.worker.Enqueue(domain.WithSessionID(context.Background(), sid), findNonFNVTaskID(t), "subject", h, h) {
		t.Fatal("a Surveillance-mode subject must be sampled")
	}
	r.worker.processOne(context.Background(), <-r.worker.queue)

	r.verifier.mu.Lock()
	for i, err := range r.verifier.errs {
		if err != nil {
			r.verifier.mu.Unlock()
			t.Fatalf("verifier generate #%d refused: %v (background leases are never refused)", i, err)
		}
	}
	r.verifier.mu.Unlock()
	if got := r.meter.Session(sid).TotalTokens(); got != 150+2*150 {
		t.Fatalf("session total = %d, want %d (pre-load + primary + cross-verifier)", got, 450)
	}
	if got := r.meter.NoSession().TotalTokens(); got != 0 {
		t.Fatalf("no-session bucket = %d, want 0: session-bound verification must not land there", got)
	}
	r.assertAllCompleted(t, 2)
}

// A verification whose step had no session is booked to the kernel no-session
// bucket (reported, never enforced), and its lease is still completed.
func TestVerificationLease_NoSessionLandsInKernelBucket(t *testing.T) {
	r := newLeaseRig(t, 0)
	h := &domain.Handoff{}
	if !r.worker.Enqueue(context.Background(), findNonFNVTaskID(t), "subject", h, h) {
		t.Fatal("a Surveillance-mode subject must be sampled")
	}
	r.worker.processOne(context.Background(), <-r.worker.queue)

	r.verifier.mu.Lock()
	if len(r.verifier.errs) != 1 || r.verifier.errs[0] != nil {
		r.verifier.mu.Unlock()
		t.Fatalf("verifier generate: %v, want one successful call", r.verifier.errs)
	}
	r.verifier.mu.Unlock()
	if got := r.meter.NoSession().TotalTokens(); got != 150 {
		t.Fatalf("no-session bucket = %d, want 150", got)
	}
	r.assertAllCompleted(t, 1)
}

// Without a gateway (nil leases) the worker still verifies — lease-less, as before.
func TestVerificationLease_NilGatewayCallsWithoutLease(t *testing.T) {
	req := &vwMockVerifyRequester{called: make(chan struct{}, 1)}
	pool := &VerifierPool{
		Registry:      newAgentSourceWith(domain.AgentDefinition{ID: "verifier1", SourceHash: "vh1"}),
		Profiles:      &vwMockGatekeeperReader{profiles: map[string]*domain.AgentProfile{"verifier1:vh1": {TrustScore: 0.9, SuccessRate: 0.9}}},
		Threshold:     0.8,
		RecencyWindow: 3,
	}
	w := NewVerificationWorker(pool, req, &vwMockTaskEventRW{events: map[string]domain.TaskEvent{}},
		&vwMockJudicialStore{}, &vwMockProfileStore{profiles: map[string]*domain.AgentProfile{}}, &mockEmbedder{}, nil,
		VerificationWorkerConfig{QueueCapacity: 1})
	w.processOne(context.Background(), domain.VerificationRequest{TaskID: "t", AgentID: "subject", Request: &domain.Handoff{}, Response: &domain.Handoff{}})
	req.mu.Lock()
	defer req.mu.Unlock()
	if req.callCount != 1 || req.lastReq.LeaseID != "" {
		t.Fatalf("calls %d lease %q; want 1 call without a lease", req.callCount, req.lastReq.LeaseID)
	}
}
