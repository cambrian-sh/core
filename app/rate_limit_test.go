package app

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/ratelimit"
)

// The shipped defaults reach the limiter intact and are generous: the
// loopback bench harness and local agents must never be throttled by them.
func TestRateLimitConfig_DefaultsMapAndAreGenerous(t *testing.T) {
	got := rateLimitConfig(config.DefaultConfig().Server.RateLimit)
	if !got.Enabled {
		t.Fatal("rate limit must default on")
	}
	for name, l := range map[string]ratelimit.Limit{"agent": got.Agent, "operator": got.Operator, "anonymous": got.Anonymous} {
		if l.RPS < 100 || l.Burst < 200 {
			t.Errorf("%s default %+v is not generous enough for the bench harness", name, l)
		}
	}
	if got.IdleTTL != 600*time.Second {
		t.Errorf("idle TTL = %v", got.IdleTTL)
	}
	// A whole burst from one principal passes with the defaults.
	lim := ratelimit.New(got, nil)
	for i := 0; i < config.DefaultRateLimitBurst; i++ {
		if !lim.Allow(ratelimit.ClassAgent, "bench") {
			t.Fatalf("default limiter refused request %d of one burst", i)
		}
	}
}
