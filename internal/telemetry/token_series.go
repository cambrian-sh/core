package telemetry

import (
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// TokenSeries accumulates token usage into hourly buckets, backing the operator
// console's spend sparkline (contract 0075).
//
// Tokens only — never money. A completion response carries token counts and
// never a price, so any currency figure here would be one the kernel invented
// from a rate nobody reconciled. Configured rates are published as a catalogue
// elsewhere and are deliberately not multiplied into anything.
//
// It is a DECORATOR over domain.TaskEventWriter rather than a new emission site.
// Every path that records a step's usage already goes through WriteTaskEvent —
// the executor, the interview worker, the verification worker — so wrapping the
// one chokepoint catches all of them and cannot drift from what the kernel
// actually recorded. A parallel emission site would have to be added at each
// caller and the first one anybody forgot would silently under-report.
type TokenSeries struct {
	mu      sync.Mutex
	buckets map[int64]*domain.TokenBucket
	// retain bounds how many hours are kept, so a long-lived kernel does not
	// accumulate a bucket per hour forever.
	retain int
	now    func() time.Time
}

// DefaultTokenRetentionHours is how much history the sparkline keeps. 48 hours
// covers "yesterday at this time", which is the comparison an operator actually
// makes; longer belongs in a metrics system, not in kernel memory.
const DefaultTokenRetentionHours = 48

// NewTokenSeries builds an empty accumulator. Wrap() attaches it to a writer.
func NewTokenSeries() *TokenSeries {
	return &TokenSeries{
		buckets: map[int64]*domain.TokenBucket{},
		retain:  DefaultTokenRetentionHours,
		now:     time.Now,
	}
}

// Wrap returns a writer that records usage and forwards to inner.
//
// The accumulator is SHARED across every wrapper — a kernel resolves its event
// writer per execution, so returning a fresh accumulator each time would reset
// the series on every plan.
func (t *TokenSeries) Wrap(inner domain.TaskEventWriter) domain.TaskEventReadWriter {
	return &tokenSeriesWriter{series: t, inner: inner}
}

// tokenSeriesWriter is one attachment of the shared accumulator.
type tokenSeriesWriter struct {
	series *TokenSeries
	inner  domain.TaskEventWriter
}

func (w *tokenSeriesWriter) WriteTaskEvent(e domain.TaskEvent) error {
	// Recorded first and unconditionally: a sparkline must never be able to fail
	// a step, and a write that errors still spent the tokens.
	w.series.record(e)
	if w.inner == nil {
		return nil
	}
	return w.inner.WriteTaskEvent(e)
}

// ReadTaskEvent forwards when the wrapped writer supports it, so decorating does
// not strip the read-back the verification worker needs.
func (w *tokenSeriesWriter) ReadTaskEvent(taskID string) (*domain.TaskEvent, error) {
	if rw, ok := w.inner.(domain.TaskEventReadWriter); ok {
		return rw.ReadTaskEvent(taskID)
	}
	return nil, nil
}

func (t *TokenSeries) record(e domain.TaskEvent) {
	if e.PromptTokens == 0 && e.CompletionTokens == 0 {
		// Nothing to add. Counting the CALL anyway would inflate the call count
		// with steps that never reached a model (cache hits, thought steps).
		return
	}
	at := e.Timestamp
	if at.IsZero() {
		at = t.now()
	}
	hour := at.Truncate(time.Hour)

	t.mu.Lock()
	defer t.mu.Unlock()
	key := hour.Unix()
	b, ok := t.buckets[key]
	if !ok {
		b = &domain.TokenBucket{HourStart: hour}
		t.buckets[key] = b
	}
	b.InputTokens += int64(e.PromptTokens)
	b.OutputTokens += int64(e.CompletionTokens)
	b.Calls++
	t.evictLocked()
}

// evictLocked drops buckets older than the retention window.
func (t *TokenSeries) evictLocked() {
	cutoff := t.now().Add(-time.Duration(t.retain) * time.Hour).Unix()
	for k := range t.buckets {
		if k < cutoff {
			delete(t.buckets, k)
		}
	}
}

// TokenSeries returns the last `hours` hourly buckets, oldest first.
//
// Hours with no usage are returned as ZERO buckets rather than omitted. A
// sparkline drawn from a sparse series silently compresses idle time and makes a
// quiet night look like continuous activity — the gap IS the information.
func (t *TokenSeries) TokenSeries(hours int) []domain.TokenBucket {
	if hours <= 0 || hours > t.retain {
		hours = t.retain
	}
	end := t.now().Truncate(time.Hour)

	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]domain.TokenBucket, 0, hours)
	for i := hours - 1; i >= 0; i-- {
		hour := end.Add(-time.Duration(i) * time.Hour)
		if b, ok := t.buckets[hour.Unix()]; ok {
			out = append(out, *b)
			continue
		}
		out = append(out, domain.TokenBucket{HourStart: hour})
	}
	return out
}
