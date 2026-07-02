package network

import (
	"context"
	"fmt"

	"github.com/cambrian-sh/cambrian-runtime/internal/centralexec"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// auctioneerYieldCaller adapts the Auctioneer to centralexec.YieldCaller.
type auctioneerYieldCaller struct{ a domain.Auctioneer }

func (c auctioneerYieldCaller) CallAgent(ctx context.Context, agentID string, h *domain.Handoff) (*domain.Handoff, error) {
	return c.a.CallAgent(ctx, agentID, h, "")
}

// selectorYieldBinder adapts the live ResourceSelector to centralexec.YieldBinder
// — sub-goal binding stays an inference decision in the selection layer
// (Zero-Hardcode), reusing whatever selector the kernel is configured with.
type selectorYieldBinder struct{ sel domain.ResourceSelector }

func (b selectorYieldBinder) Bind(ctx context.Context, intent string) (string, error) {
	sel, err := b.sel.Select(ctx, domain.Intent{ID: "subgoal", Description: intent}, nil)
	if err != nil {
		return "", err
	}
	if sel.ResourceID == "" {
		return "", fmt.Errorf("no resource bound for sub-goal intent %q", intent)
	}
	return sel.ResourceID, nil
}

// NewYieldDriver builds the ADR-0037 D10–D15 YieldDriver from the live selector +
// auctioneer + embedder (ADR-0041 follow-up: the previously-unwired half). Returns
// nil when the selector or auctioneer is absent (yield then stays inert, as before).
func NewYieldDriver(sel domain.ResourceSelector, a domain.Auctioneer, emb domain.Embedder,
	narrowingMargin float64, maxDepth int) *centralexec.YieldDriver {
	if sel == nil || a == nil {
		return nil
	}
	return &centralexec.YieldDriver{
		Coordinator: centralexec.NewYieldCoordinator(narrowingMargin),
		Binder:      selectorYieldBinder{sel: sel},
		Caller:      auctioneerYieldCaller{a: a},
		Embedder:    emb,
		MaxDepth:    maxDepth,
	}
}
