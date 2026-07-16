package agentmgr

import (
	"testing"
	"time"
)

// newTestPolicy builds a policy with a fixed clock and no-jitter (jitter returns the
// ceiling) so backoff is exactly the capped exponential value.
func newTestPolicy(maxAttempts int, window, base, max time.Duration, now *time.Time) *DaemonRestartPolicy {
	p := NewDaemonRestartPolicy(maxAttempts, window, base, max)
	p.now = func() time.Time { return *now }
	p.jitter = func(m time.Duration) time.Duration { return m } // deterministic: full ceiling
	return p
}

func TestRestartPolicy_ExponentialBackoffCapped(t *testing.T) {
	now := time.Now()
	p := newTestPolicy(10, time.Hour, time.Second, 8*time.Second, &now)

	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		delay, quar := p.Register("s")
		if quar {
			t.Fatalf("attempt %d: unexpected quarantine", i)
		}
		if delay != w {
			t.Fatalf("attempt %d: backoff = %v, want %v", i, delay, w)
		}
	}
}

func TestRestartPolicy_FlapQuarantine(t *testing.T) {
	now := time.Now()
	p := newTestPolicy(3, time.Hour, time.Second, time.Minute, &now)
	for i := 0; i < 3; i++ {
		if _, quar := p.Register("s"); quar {
			t.Fatalf("attempt %d should restart, not quarantine", i)
		}
	}
	// 4th attempt within the window → quarantine.
	if _, quar := p.Register("s"); !quar {
		t.Fatal("exceeding MaxAttempts in the window must quarantine")
	}
}

func TestRestartPolicy_WindowRollsOver(t *testing.T) {
	now := time.Now()
	p := newTestPolicy(2, time.Minute, time.Second, time.Minute, &now)
	p.Register("s")
	p.Register("s")
	if _, quar := p.Register("s"); !quar {
		t.Fatal("precondition: 3rd within window quarantines")
	}
	// Advance past the window — attempts prune, exploration resumes.
	now = now.Add(2 * time.Minute)
	if _, quar := p.Register("s"); quar {
		t.Fatal("after the window rolls over, restart should be allowed again")
	}
}

func TestRestartPolicy_DisabledAlwaysQuarantines(t *testing.T) {
	now := time.Now()
	p := newTestPolicy(0, time.Hour, time.Second, time.Minute, &now)
	if _, quar := p.Register("s"); !quar {
		t.Fatal("MaxAttempts=0 must quarantine (auto-restart disabled)")
	}
	var nilP *DaemonRestartPolicy
	if _, quar := nilP.Register("s"); !quar {
		t.Fatal("nil policy must quarantine (no restart)")
	}
}

func TestRestartPolicy_ResetClears(t *testing.T) {
	now := time.Now()
	p := newTestPolicy(2, time.Hour, time.Second, time.Minute, &now)
	p.Register("s")
	p.Register("s")
	p.Reset("s")
	if _, quar := p.Register("s"); quar {
		t.Fatal("after Reset the stream should restart fresh")
	}
}
