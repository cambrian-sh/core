package app

import (
	"context"
	"sync/atomic"

	"github.com/cambrian-sh/core/domain"
)

// deadLetterRetrierHolder is the late-bound dead-letter retrier
// (KernelServices.RegisterDeadLetterRetrier, ENG-1654). The operator service is
// built before plugins Build, so it holds this holder; the premium reactive
// plugin's registration lands in it. Until then (and forever on an OSS kernel) a
// retry answers domain.ErrDeadLetterRetryUnavailable, which the operator plane
// reports as Unimplemented, and no entry is offered as retryable.
type deadLetterRetrierHolder struct {
	v atomic.Pointer[domain.WatchDeadLetterRetrier]
}

func (h *deadLetterRetrierHolder) set(r domain.WatchDeadLetterRetrier) {
	if r == nil {
		return
	}
	h.v.Store(&r)
}

// RetryDeadLetter implements domain.WatchDeadLetterRetrier.
func (h *deadLetterRetrierHolder) RetryDeadLetter(ctx context.Context, id string) error {
	if p := h.v.Load(); p != nil {
		return (*p).RetryDeadLetter(ctx, id)
	}
	return domain.ErrDeadLetterRetryUnavailable
}

// RetryAvailable reports whether a retrier is registered (the operator plane marks
// entries retryable only then).
func (h *deadLetterRetrierHolder) RetryAvailable() bool { return h.v.Load() != nil }

var _ domain.WatchDeadLetterRetrier = (*deadLetterRetrierHolder)(nil)
