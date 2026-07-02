package llm

import (
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for deterministic cooldown tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker(threshold int, cooldown time.Duration) (*CircuitBreaker, *fakeClock) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	return newCircuitBreakerWithClock(threshold, cooldown, clk.Now), clk
}

func TestCircuitBreaker_UnknownIDHealthy(t *testing.T) {
	b, _ := newTestBreaker(3, time.Minute)
	if !b.Healthy("never-seen") {
		t.Fatal("unknown id should be optimistically healthy")
	}
}

func TestCircuitBreaker_TripsAtThreshold(t *testing.T) {
	b, _ := newTestBreaker(3, time.Minute)
	b.Record("x", false)
	b.Record("x", false)
	if !b.Healthy("x") {
		t.Fatal("should still be healthy below threshold (2/3 failures)")
	}
	b.Record("x", false) // 3rd consecutive failure trips OPEN
	if b.Healthy("x") {
		t.Fatal("should be OPEN at threshold (3/3 failures)")
	}
}

func TestCircuitBreaker_OpenBlocksUntilCooldown(t *testing.T) {
	b, clk := newTestBreaker(1, time.Minute)
	b.Record("x", false) // threshold=1 => immediately OPEN
	if b.Healthy("x") {
		t.Fatal("should be OPEN immediately after tripping")
	}
	clk.advance(59 * time.Second)
	if b.Healthy("x") {
		t.Fatal("should remain OPEN before cooldown elapses")
	}
	clk.advance(time.Second) // now == cooldown
	if !b.Healthy("x") {
		t.Fatal("should be half-open (permittable) once cooldown elapses")
	}
}

func TestCircuitBreaker_HalfOpenRestoresOnSuccess(t *testing.T) {
	b, clk := newTestBreaker(1, time.Minute)
	b.Record("x", false)
	clk.advance(time.Minute)
	if !b.Healthy("x") {
		t.Fatal("precondition: half-open after cooldown")
	}
	b.Record("x", true) // probe succeeds => restore
	if !b.Healthy("x") {
		t.Fatal("should be healthy after a successful half-open probe")
	}
	_ = clk
}

func TestCircuitBreaker_HalfOpenReopensOnFailure(t *testing.T) {
	b, clk := newTestBreaker(1, time.Minute)
	b.Record("x", false)
	clk.advance(time.Minute)
	if !b.Healthy("x") {
		t.Fatal("precondition: half-open after cooldown")
	}
	b.Record("x", false) // probe fails => re-open, cooldown resets from now
	if b.Healthy("x") {
		t.Fatal("should re-open after a failed half-open probe")
	}
	clk.advance(59 * time.Second)
	if b.Healthy("x") {
		t.Fatal("re-opened cooldown should not have elapsed yet")
	}
	clk.advance(time.Second)
	if !b.Healthy("x") {
		t.Fatal("should be half-open again after the reset cooldown")
	}
}

func TestCircuitBreaker_SuccessResetsConsecutiveCounter(t *testing.T) {
	b, _ := newTestBreaker(3, time.Minute)
	b.Record("x", false)
	b.Record("x", false)
	b.Record("x", true) // reset
	b.Record("x", false)
	b.Record("x", false)
	if !b.Healthy("x") {
		t.Fatal("counter should have reset on success; 2/3 must not trip")
	}
}
