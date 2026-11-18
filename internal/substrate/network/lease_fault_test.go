package network

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// ENG-2124: the gateway records the class of a lease's last failed model call, so the
// chat lane can tell the person why a turn failed whatever the worker reported.

func TestClassifyProviderFault_MirrorsGatewayStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want domain.ProviderFault
	}{
		{"nil", nil, domain.ProviderFaultNone},
		{"401", &llm.ProviderError{Provider: "openai", Status: 401, Message: "x", Auth: true}, domain.ProviderFaultAuth},
		{"streamed auth", fmt.Errorf("openai: %w: Failed to authenticate", domain.ErrProviderAuth), domain.ProviderFaultAuth},
		{"429", &llm.ProviderError{Provider: "openai", Status: 429, Message: "rate"}, domain.ProviderFaultRateLimited},
		{"overflow conwip", errors.New("gateway_overflow"), domain.ProviderFaultRateLimited},
		{"500", &llm.ProviderError{Provider: "openai", Status: 500, Message: "boom"}, domain.ProviderFaultUnavailable},
		{"reset", fmt.Errorf("openai: %w: reset", llm.ErrProviderUnreachable), domain.ProviderFaultUnavailable},
		{"no healthy model", llm.ErrNoHealthyModel, domain.ProviderFaultUnavailable},
		{"spend ceiling", fmt.Errorf("x: %w", llm.ErrSpendCeiling), domain.ProviderFaultSpendLimit},
		{"context overflow", &llm.ProviderError{Provider: "openai", Status: 400, Message: "x", Overflow: true}, domain.ProviderFaultContextOverflow},
		// Not provider failures: a capability answer, a cancelled turn, a plain 400.
		{"tools unsupported", fmt.Errorf("%w: m", ErrToolCallingUnsupported), domain.ProviderFaultNone},
		{"cancelled", context.Canceled, domain.ProviderFaultNone},
		{"400", &llm.ProviderError{Provider: "openai", Status: 400, Message: "bad"}, domain.ProviderFaultNone},
	}
	for _, c := range cases {
		if got := classifyProviderFault(c.err); got != c.want {
			t.Errorf("%s: fault = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLeaseFault_SetByAFailureClearedByASuccessKeptOverACancel(t *testing.T) {
	t.Parallel()
	_, gw := newLeaseServer(t)
	lease, err := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := gw.LeaseFault(lease); got != domain.ProviderFaultNone {
		t.Fatalf("fresh lease fault = %q", got)
	}
	gw.recordCallOutcome(lease, &llm.ProviderError{Provider: "openai", Status: 429, Message: "rate"})
	if got := gw.LeaseFault(lease); got != domain.ProviderFaultRateLimited {
		t.Fatalf("after a 429 = %q", got)
	}
	gw.recordCallOutcome(lease, context.Canceled)
	if got := gw.LeaseFault(lease); got != domain.ProviderFaultRateLimited {
		t.Fatalf("a cancelled call must not erase the record, got %q", got)
	}
	gw.recordCallOutcome(lease, nil)
	if got := gw.LeaseFault(lease); got != domain.ProviderFaultNone {
		t.Fatalf("a successful call must clear the record, got %q", got)
	}
	if got := gw.LeaseFault("unknown"); got != domain.ProviderFaultNone {
		t.Fatalf("unknown lease = %q", got)
	}
}
