package app

import (
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/ratelimit"
)

// rateLimitConfig maps server.rate_limit onto the limiter's construction input
// (T-20261101-rate-limit-the-operator-and-agent-grpc-planes). Kept in app so
// internal/ratelimit does not import internal/config — the same direction as
// every other config-to-component mapping at the composition root.
func rateLimitConfig(c config.RateLimitConfig) ratelimit.Config {
	cls := func(x config.RateLimitClass) ratelimit.Limit {
		return ratelimit.Limit{RPS: x.RequestsPerSecond, Burst: x.Burst}
	}
	return ratelimit.Config{
		Enabled:   c.Enabled,
		Agent:     cls(c.Agent),
		Operator:  cls(c.Operator),
		Anonymous: cls(c.Anonymous),
		IdleTTL:   time.Duration(c.IdleBucketTTLSeconds) * time.Second,
	}
}
