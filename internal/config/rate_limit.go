package config

import "fmt"

// RateLimitConfig is the per-principal token-bucket limit on the kernel's gRPC
// planes (server.rate_limit; T-20261101-rate-limit-the-operator-and-agent-grpc-planes,
// ADR-0047 / ADR-0130). Read by app.go, which constructs the limiter and puts
// its interceptors in the server chain AFTER authentication, so every bucket is
// keyed by the principal the credential established — never by a header the
// caller chose freely.
//
// Three classes, because they have different callers:
//   - Agent: the Orchestrator agent plane (and any non-human principal) — one
//     bucket per agent id.
//   - Operator: the OperatorConsole and premium operator planes — one bucket
//     per human operator identity.
//   - Anonymous: calls that reach the server with no principal (Login, health,
//     agent calls under insecure_localhost with no x-agent-id) — one bucket per
//     peer host.
//
// Defaults are deliberately GENEROUS (200 req/s sustained, burst 400, per
// principal, every class): this is a storm guard against a runaway or hostile
// client, not a quota. The loopback benchmark harness and a fleet of local
// agents must never notice it — the fastest measured legitimate caller (bulk
// ingest from the bench harness) is well under that per principal. A stream
// costs one token when it OPENS; messages on an open stream are not metered.
type RateLimitConfig struct {
	// Enabled switches the limiter on. Default true.
	Enabled bool `json:"enabled"`
	// Agent, Operator and Anonymous are the per-class bucket shapes.
	Agent     RateLimitClass `json:"agent"`
	Operator  RateLimitClass `json:"operator"`
	Anonymous RateLimitClass `json:"anonymous"`
	// IdleBucketTTLSeconds is how long an untouched bucket is kept before it is
	// swept, so a stream of distinct anonymous peers cannot grow the map without
	// bound. Default 600.
	IdleBucketTTLSeconds int `json:"idle_bucket_ttl_seconds"`
}

// RateLimitClass is one token bucket shape: RequestsPerSecond refill, Burst
// capacity. RequestsPerSecond = 0 leaves that class unlimited (an explicit
// opt-out, not a default).
type RateLimitClass struct {
	RequestsPerSecond float64 `json:"requests_per_second"`
	Burst             int     `json:"burst"`
}

// Default rate-limit values. See RateLimitConfig for why they are generous.
const (
	DefaultRateLimitRPS            = 200
	DefaultRateLimitBurst          = 400
	DefaultRateLimitIdleTTLSeconds = 600
)

// DefaultRateLimitConfig is the server.rate_limit block DefaultConfig carries.
func DefaultRateLimitConfig() RateLimitConfig {
	c := RateLimitClass{RequestsPerSecond: DefaultRateLimitRPS, Burst: DefaultRateLimitBurst}
	return RateLimitConfig{
		Enabled:              true,
		Agent:                c,
		Operator:             c,
		Anonymous:            c,
		IdleBucketTTLSeconds: DefaultRateLimitIdleTTLSeconds,
	}
}

// validate reports configuration errors. A limited class needs a burst of at
// least 1, or every request in it is refused — a typo that would read as an
// outage rather than a config error.
func (c RateLimitConfig) validate() []string {
	if !c.Enabled {
		return nil
	}
	var errs []string
	for name, cl := range map[string]RateLimitClass{"agent": c.Agent, "operator": c.Operator, "anonymous": c.Anonymous} {
		if cl.RequestsPerSecond < 0 {
			errs = append(errs, fmt.Sprintf("server.rate_limit.%s.requests_per_second must be >= 0 (0 = unlimited), got %v",
				name, cl.RequestsPerSecond))
		}
		if cl.RequestsPerSecond > 0 && cl.Burst < 1 {
			errs = append(errs, fmt.Sprintf("server.rate_limit.%s.burst must be >= 1 when requests_per_second > 0, got %d",
				name, cl.Burst))
		}
	}
	if c.IdleBucketTTLSeconds < 0 {
		errs = append(errs, fmt.Sprintf("server.rate_limit.idle_bucket_ttl_seconds must be >= 0, got %d", c.IdleBucketTTLSeconds))
	}
	return errs
}
