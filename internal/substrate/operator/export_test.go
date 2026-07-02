package operator

import (
	"context"
	"time"
)

// ContextWithPrincipal injects an authenticated principal for tests that call
// command handlers directly (bypassing the interceptor). Test-only.
func ContextWithPrincipal(ctx context.Context, principal string, role Role) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, principalInfo{principal: principal, role: role})
}

// SetClock overrides the spool's clock. Compiled only in tests (export_test.go),
// so it does not widen the production API. Lets age-cap eviction be tested
// deterministically without sleeping.
func (s *Spool) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}
