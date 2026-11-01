//go:build integration

package harness_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/testing/harness"
)

// ADR-0018 Pass 1: the REAL gateway cuts a step's stream once the estimate
// reaches 90% of the leased TokenLimit. A 400-char response (100 tokens at
// 4 chars/token) against a 40-token lease must come back truncated at the 36-token
// cut-off, and Complete must reconcile a usage that fits the budget.
func TestE2E_TokenBudgetEnforced(t *testing.T) {
	const limit = 40
	h := harness.New(harness.Config{StepTokenLimit: limit})
	full := strings.Repeat("0123456789abcdef", 25) // 400 chars = 100 tokens
	h.AddResponse(full)

	final, events, err := h.ExecutePlan(t.Context())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	got := string(final.Payload.Data)
	cutoff := int(float64(limit) * 0.9)
	if llm.EstimateTokens(got) > limit {
		t.Fatalf("delivered %d tokens, lease was %d: budget not enforced", llm.EstimateTokens(got), limit)
	}
	if len(got) >= len(full) {
		t.Fatalf("response was not truncated: %d chars delivered of %d", len(got), len(full))
	}
	if llm.EstimateTokens(got) < cutoff {
		t.Fatalf("stream cut too early: %d tokens delivered, cut-off is %d", llm.EstimateTokens(got), cutoff)
	}

	usages := h.StepUsages()
	if len(usages) != 1 {
		t.Fatalf("want 1 reconciled usage, got %d", len(usages))
	}
	if usages[0].TotalTokens == 0 || usages[0].TotalTokens > limit {
		t.Fatalf("reconciled usage %d outside (0, %d]", usages[0].TotalTokens, limit)
	}
	if len(events) != 1 || events[0].CompletionTokens != llm.EstimateTokens(got) {
		t.Fatalf("TaskEvent should count the truncated response: events=%d", len(events))
	}
	// The cut-off is reported on the event itself, server-authoritatively
	// (T-20261031-taskevent-budgetoverrun-is-written-by-no-kernel).
	if !events[0].BudgetOverrun {
		t.Fatalf("TaskEvent.BudgetOverrun must carry the gateway's Pass-1 cut, got %+v", events[0])
	}
}

// Under the limit the stream passes untouched and usage matches the response.
func TestE2E_TokenBudgetNotExceededPassesThrough(t *testing.T) {
	h := harness.New(harness.Config{StepTokenLimit: 4096})
	h.AddResponse("a short answer")

	final, events, err := h.ExecutePlan(t.Context())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if got := string(final.Payload.Data); got != "a short answer" {
		t.Fatalf("response altered: %q", got)
	}
	if len(events) != 1 || events[0].BudgetOverrun {
		t.Fatalf("a response under the limit must not be reported as an overrun: %+v", events)
	}
	if u := h.StepUsages(); len(u) != 1 || u[0].TotalTokens != llm.EstimateTokens("a short answer") {
		t.Fatalf("usage %+v does not match the response", u)
	}
}

func TestE2E_ObserverWiring(t *testing.T) {
	h := harness.New(harness.Config{})
	h.AddResponse("step result")

	_, events, err := h.ExecutePlan(t.Context())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	if len(events) != len(h.Observer.TaskCompletedEvents) {
		t.Errorf("TaskEvents=%d, Observer calls=%d — observer fires should match TaskEvent count",
			len(events), len(h.Observer.TaskCompletedEvents))
	}
	if len(events) == 0 {
		t.Error("expected at least one TaskEvent + observer call")
	}
	// The step generated through the gateway, so the model saw the step's query.
	if p := h.Streamer.Prompts(); len(p) != 1 || p[0] != "execute this task" {
		t.Errorf("gateway did not route the step to the model: prompts=%v", p)
	}
}

// runConcurrent starts n plans on h, holds the model open until `entered`
// streams are inside the gateway (or `failed` plans have errored), then releases
// the gate and returns successes and the errors seen.
func runConcurrent(t *testing.T, h *harness.SystemHarness, n, waitEntered, waitFailed int) (int, []error) {
	t.Helper()
	h.Streamer.Gate = make(chan struct{})
	for i := 0; i < n; i++ {
		h.AddResponse("quick")
	}

	var (
		mu     sync.Mutex
		errs   []error
		wg     sync.WaitGroup
		failed = make(chan struct{}, n)
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := h.ExecutePlan(t.Context())
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
				failed <- struct{}{}
			}
			mu.Unlock()
		}()
	}

	deadline := time.After(5 * time.Second)
	failures := 0
	for h.Streamer.Entered() < waitEntered || failures < waitFailed {
		select {
		case <-failed:
			failures++
		case <-deadline:
			t.Fatalf("timeout: entered=%d (want %d) failures=%d (want %d)",
				h.Streamer.Entered(), waitEntered, failures, waitFailed)
		case <-time.After(time.Millisecond):
		}
	}
	close(h.Streamer.Gate)
	wg.Wait()
	return n - len(errs), errs
}

// The REAL gateway's CONWIP semaphore: with width 1, one step holds the model
// open and the other four are refused with gateway_overflow at the door
// (StreamChunks does not queue). With width 5 all five run at once.
func TestE2E_ConwipBackpressure(t *testing.T) {
	h := harness.New(harness.Config{MaxConcurrentSessions: 1})
	successes, errs := runConcurrent(t, h, 5, 1, 4)
	if successes != 1 {
		t.Fatalf("width 1: want exactly 1 success, got %d (errs=%v)", successes, errs)
	}
	for _, err := range errs {
		if !strings.Contains(err.Error(), "gateway_overflow") {
			t.Fatalf("refused step should carry the gateway's overflow error, got %v", err)
		}
	}
	// Exactly the admitted step reported its (zero) CONWIP wait.
	if n := len(h.Observer.ConwipWaits); n != 1 {
		t.Fatalf("width 1: want 1 ConwipWait observation, got %d", n)
	}
	// The gateway must release the lease of every refused step on Complete.
	if n := h.Gateway.SessionCount(); n != 0 {
		t.Fatalf("leases leaked after refused steps: %d", n)
	}

	wide := harness.New(harness.Config{MaxConcurrentSessions: 5})
	successes, errs = runConcurrent(t, wide, 5, 5, 0)
	if successes != 5 {
		t.Fatalf("width 5: want 5 successes, got %d (errs=%v)", successes, errs)
	}
	if n := len(wide.Observer.ConwipWaits); n != 5 {
		t.Fatalf("width 5: want 5 ConwipWait observations, got %d", n)
	}
	if n := wide.Gateway.SessionCount(); n != 0 {
		t.Fatalf("leases leaked after successful steps: %d", n)
	}
}

// A cancelled plan context must reach the model through the gateway and abort
// the stream rather than hang inside the semaphore.
func TestE2E_CancelPropagatesThroughGateway(t *testing.T) {
	h := harness.New(harness.Config{})
	h.Streamer.Gate = make(chan struct{}) // never released
	h.AddResponse("never delivered")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := h.ExecutePlan(ctx)
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	for h.Streamer.Entered() == 0 {
		select {
		case <-deadline:
			t.Fatal("step never reached the model")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled through the gateway, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("plan did not abort after cancel")
	}
	if n := h.Gateway.SessionCount(); n != 0 {
		t.Fatalf("lease leaked after cancel: %d", n)
	}
}
