package llm

import (
	"context"
	"sync"
)

// Usage capture (ADR-0131 D2).
//
// domain.Generator.Generate returns only text, and every acquired generator is
// a stack of decorators (health, tracing, concurrency, metering). Threading a
// usage return value through all of them would change the port and every
// decorator for one cross-cutting fact. Instead the caller that wants the usage
// — the SpendMeter's decorator, the gateway's tool path — opens a capture cell
// in ctx, and the provider adapter at the bottom of the stack reports what its
// response said into it. Decorators forward ctx already, so nothing between the
// two needs to know.

type usageCaptureKey struct{}

// UsageCapture receives the provider usage of the call made under its context.
type UsageCapture struct {
	mu  sync.Mutex
	u   TokenUsage
	set bool
}

// WithUsageCapture returns a child context carrying a fresh capture cell.
func WithUsageCapture(ctx context.Context) (context.Context, *UsageCapture) {
	c := &UsageCapture{}
	return context.WithValue(ctx, usageCaptureKey{}, c), c
}

// Usage returns the reported usage and whether the provider reported any.
func (c *UsageCapture) Usage() (TokenUsage, bool) {
	if c == nil {
		return TokenUsage{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.u, c.set
}

// reportUsage is called by a provider adapter after it parsed a response. A
// zero usage (no usage block in the response) is not a report: the caller must
// fall back to the flagged estimate rather than record a free call.
func reportUsage(ctx context.Context, u TokenUsage) {
	if u.IsZero() {
		return
	}
	c, ok := ctx.Value(usageCaptureKey{}).(*UsageCapture)
	if !ok || c == nil {
		return
	}
	c.mu.Lock()
	c.u, c.set = u, true
	c.mu.Unlock()
}
