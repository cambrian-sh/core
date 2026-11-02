package domain

import (
	"context"
	"sync"
	"time"
)

// StageClock accumulates named durations and counts for ONE unit of work (one
// ingested document) across layers that do not otherwise share a return path:
// the ingestion manager times its own stages, while the structure dispatcher and
// the vector-store adapter add sub-stages (sidecar processing time, projection
// writes) that only they can see. It rides the context so no port signature
// changes for a diagnostic (T-20261102-ingestion-logs-no-per-stage-timing-so-the-1-s).
//
// Every method is nil-safe: code that records into a context without a clock
// pays one ctx.Value lookup and nothing else. Safe for concurrent use.
type StageClock struct {
	mu     sync.Mutex
	order  []string
	ms     map[string]time.Duration
	counts map[string]int
}

type stageClockKey struct{}

// WithStageClock returns ctx carrying c.
func WithStageClock(ctx context.Context, c *StageClock) context.Context {
	return context.WithValue(ctx, stageClockKey{}, c)
}

// StageClockFrom returns the clock on ctx, or nil.
func StageClockFrom(ctx context.Context) *StageClock {
	c, _ := ctx.Value(stageClockKey{}).(*StageClock)
	return c
}

// NewStageClock returns an empty clock.
func NewStageClock() *StageClock {
	return &StageClock{ms: map[string]time.Duration{}, counts: map[string]int{}}
}

// Add accumulates d under stage. Repeated stages sum.
func (c *StageClock) Add(stage string, d time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if _, ok := c.ms[stage]; !ok {
		c.order = append(c.order, stage)
	}
	c.ms[stage] += d
	c.mu.Unlock()
}

// Count accumulates n under name.
func (c *StageClock) Count(name string, n int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.counts[name] += n
	c.mu.Unlock()
}

// Since is Add(stage, time.Since(start)), for `defer`-free call sites.
func (c *StageClock) Since(stage string, start time.Time) {
	c.Add(stage, time.Since(start))
}

// StageTiming is one recorded stage.
type StageTiming struct {
	Stage    string
	Duration time.Duration
}

// Stages returns the recorded stages in first-recorded order.
func (c *StageClock) Stages() []StageTiming {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]StageTiming, 0, len(c.order))
	for _, s := range c.order {
		out = append(out, StageTiming{Stage: s, Duration: c.ms[s]})
	}
	return out
}

// Counts returns a copy of the recorded counts.
func (c *StageClock) Counts() map[string]int {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}
