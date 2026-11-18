package harness

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Owner decision 2026-11-12 (T-20261112-self-healer-retries-a-step-whose-yield-chain):
// transient → retry the same input; deterministic → never retry; needs_replan →
// never retry, straight to the re-planner.

var errDeterministicLimit = domain.NewDeterministicError("yield: max sub-goal chain depth exceeded")

func TestClassifyStepFailure_EachClass(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code codes.Code
		typ  string
		want domain.StepFailureClass
	}{
		{"timeout", fmt.Errorf("dispatch: step timed out: %w", context.DeadlineExceeded), codes.Unknown, "", domain.StepFailureTransient},
		{"unavailable", status.Error(codes.Unavailable, "agent down"), codes.Unavailable, "", domain.StepFailureTransient},
		{"rate limit", status.Error(codes.ResourceExhausted, "429"), codes.ResourceExhausted, "", domain.StepFailureTransient},
		{"agent system error", errors.New("provider 503"), codes.Unknown, "system", domain.StepFailureTransient},
		{"yield depth limit", fmt.Errorf("agent x at depth 3: %w", errDeterministicLimit), codes.Unknown, "", domain.StepFailureDeterministic},
		{"validation", status.Error(codes.InvalidArgument, "bad args"), codes.InvalidArgument, "", domain.StepFailureDeterministic},
		{"denied", status.Error(codes.PermissionDenied, "no"), codes.PermissionDenied, "", domain.StepFailureDeterministic},
		{"dispatch refusal", &domain.NoEffectMatchError{TaskID: "t", Missing: []string{"move"}, Check: &domain.EffectCheckResult{}}, codes.Unknown, "", domain.StepFailureDeterministic},
		{"capability missing", fmt.Errorf("yield: %w", &domain.CapabilityMissingError{Needed: []string{"fetch a web page"}}), codes.Unknown, "", domain.StepFailureNeedsReplan},
		{"repeated tool failure", &domain.StepStuckError{Tool: "write_file", Count: 3, Err: "exists"}, codes.Unknown, "", domain.StepFailureNeedsReplan},
		{"agent logic error", errors.New("agent returned bad output"), codes.Internal, "", domain.StepFailureUnclassified},
	}
	for _, c := range cases {
		if got := ClassifyStepFailure(c.err, c.code, c.typ); got != c.want {
			t.Errorf("%s: class %q, want %q", c.name, got, c.want)
		}
	}
}

func countingStep(err error) (StepFunc, *int) {
	n := 0
	return func(context.Context, *domain.Handoff) (*domain.Handoff, error) {
		n++
		return nil, err
	}, &n
}

func TestWrap_DeterministicIsNeverRetried(t *testing.T) {
	inner, calls := countingStep(fmt.Errorf("agent x: %w", errDeterministicLimit))
	_, err := (&SelfHealer{StepIndex: 1}).Wrap(inner)(context.Background(), &domain.Handoff{})
	var he *HealingExhaustedError
	if !errors.As(err, &he) || *calls != 1 || he.Class != domain.StepFailureDeterministic {
		t.Fatalf("calls=%d err=%v, want one attempt ending deterministic", *calls, err)
	}
	if !errors.Is(err, errDeterministicLimit) {
		t.Fatal("the cause must stay in the chain for the replanner")
	}
}

func TestWrap_NeedsReplanIsNeverRetried(t *testing.T) {
	for _, cause := range []error{
		&domain.CapabilityMissingError{Needed: []string{"run a PowerShell command"}},
		&domain.StepStuckError{Tool: "mcp:filesystem/write_file", Count: 3, Err: "refused"},
	} {
		inner, calls := countingStep(cause)
		_, err := (&SelfHealer{StepIndex: 2}).Wrap(inner)(context.Background(), &domain.Handoff{})
		var he *HealingExhaustedError
		if !errors.As(err, &he) || *calls != 1 || he.Class != domain.StepFailureNeedsReplan {
			t.Fatalf("%T: calls=%d err=%v, want one attempt ending needs_replan", cause, *calls, err)
		}
	}
}

func TestWrap_TransientIsRetriedWithTheSameInput(t *testing.T) {
	inner, calls := countingStep(status.Error(codes.Unavailable, "agent restarting"))
	h := &domain.Handoff{Context: map[string]string{}}
	_, err := (&SelfHealer{StepIndex: 0, MaxAttempts: 3}).Wrap(inner)(context.Background(), h)
	if err == nil || *calls != 3 {
		t.Fatalf("calls=%d err=%v, want 3 attempts", *calls, err)
	}
	if h.Context["_heal_error"] != "" {
		t.Fatalf("a transient retry must not hand the agent a heal note: %q", h.Context["_heal_error"])
	}
}

// T-20261112-worker-resolve-cid-inline-has-no-size-cap-a: a prompt over the
// model's context window is refused again on every attempt.
func TestClassifyStepFailure_ContextOverflowDeterministic(t *testing.T) {
	cases := map[string]struct {
		err  error
		code codes.Code
	}{
		"in process": {fmt.Errorf("gateway: %w", domain.ErrContextOverflow), codes.Unknown},
		"over the wire (gateway / worker OUT_OF_RANGE)": {status.Error(codes.OutOfRange, "Prompt is too long"), codes.OutOfRange},
		"sentinel under an Internal status":             {fmt.Errorf("rpc: %w", domain.ErrContextOverflow), codes.Internal},
	}
	for name, c := range cases {
		if got := ClassifyStepFailure(c.err, c.code, ""); got != domain.StepFailureDeterministic {
			t.Errorf("%s: class = %v, want deterministic", name, got)
		}
	}
}

func TestSelfHealer_DoesNotRetryContextOverflow(t *testing.T) {
	calls := 0
	sh := &SelfHealer{StepIndex: 1, MaxAttempts: 3}
	_, err := sh.Wrap(func(context.Context, *domain.Handoff) (*domain.Handoff, error) {
		calls++
		return nil, status.Error(codes.OutOfRange, "llm: prompt exceeds the model's context window: Prompt is too long")
	})(context.Background(), &domain.Handoff{})
	if calls != 1 {
		t.Fatalf("the step ran %d times; a context overflow must not be retried", calls)
	}
	var he *HealingExhaustedError
	if !errors.As(err, &he) || he.Class != domain.StepFailureDeterministic || he.LoopDetected {
		t.Fatalf("err = %v, want a deterministic HealingExhaustedError (not a loop)", err)
	}
}

// T-20261112-a-provider-auth-failure-streamed-through-the (owner decision
// 2026-11-18): a refused provider credential is refused again on every attempt,
// in process (domain.ErrProviderAuth) and over the wire (gateway Unauthenticated).
func TestClassifyStepFailure_ProviderAuthDeterministic(t *testing.T) {
	cases := map[string]struct {
		err  error
		code codes.Code
	}{
		"in process": {fmt.Errorf("openai: %w: Failed to authenticate", domain.ErrProviderAuth), codes.Unknown},
		"over the wire (gateway UNAUTHENTICATED)": {status.Error(codes.Unauthenticated, "openai: HTTP 401: OAuth token revoked"), codes.Unauthenticated},
		"sentinel under an Internal status":       {fmt.Errorf("rpc: %w", domain.ErrProviderAuth), codes.Internal},
	}
	for name, c := range cases {
		if got := ClassifyStepFailure(c.err, c.code, ""); got != domain.StepFailureDeterministic {
			t.Errorf("%s: class = %v, want deterministic", name, got)
		}
	}
}

func TestSelfHealer_DoesNotRetryProviderAuth(t *testing.T) {
	calls := 0
	sh := &SelfHealer{StepIndex: 1, MaxAttempts: 3}
	_, err := sh.Wrap(func(context.Context, *domain.Handoff) (*domain.Handoff, error) {
		calls++
		return nil, status.Error(codes.Unauthenticated, "openai: received error while streaming: Failed to authenticate: OAuth token revoked")
	})(context.Background(), &domain.Handoff{})
	if calls != 1 {
		t.Fatalf("the step ran %d times; a refused credential must not be retried", calls)
	}
	var he *HealingExhaustedError
	if !errors.As(err, &he) || he.Class != domain.StepFailureDeterministic {
		t.Fatalf("err = %v, want a deterministic HealingExhaustedError", err)
	}
}
