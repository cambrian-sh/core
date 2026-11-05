package kernel

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/yield"
)

// ADR-0041 D5 / ADR-0100 D10: the Selector every caller dispatches through must
// be the yield consumer around the Dispatcher. Core b0072cc dropped the old
// consumer silently; this pins the production wiring so it cannot happen again.
func TestMetabolismStack_SelectorResolvesYields(t *testing.T) {
	meta := NewMetabolismStack(&AgentRepoDecorator{}, nil, nil, nil, nil, config.DefaultConfig(), nil, nil)
	if _, ok := meta.Selector.(*yield.Resolver); !ok {
		t.Fatalf("MetabolismStack.Selector = %T, want *yield.Resolver wrapping the Dispatcher", meta.Selector)
	}
	if meta.Dispatcher == nil {
		t.Fatal("the concrete Dispatcher must still be exposed for EventBus wiring")
	}
}
