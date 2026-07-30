package telemetry

import (
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

type recordingWriter struct {
	events []domain.TaskEvent
	err    error
}

func (r *recordingWriter) WriteTaskEvent(e domain.TaskEvent) error {
	r.events = append(r.events, e)
	return r.err
}

func fixedNow(t *TokenSeries, at time.Time) { t.now = func() time.Time { return at } }

func TestTokenSeries_AccumulatesIntoHourlyBuckets(t *testing.T) {
	now := time.Date(2026, 7, 29, 15, 30, 0, 0, time.UTC)
	ts := NewTokenSeries()
	fixedNow(ts, now)
	w := ts.Wrap(nil)

	// Two events in the same hour, one in the previous.
	_ = w.WriteTaskEvent(domain.TaskEvent{PromptTokens: 100, CompletionTokens: 20, Timestamp: now})
	_ = w.WriteTaskEvent(domain.TaskEvent{PromptTokens: 50, CompletionTokens: 10, Timestamp: now.Add(-5 * time.Minute)})
	_ = w.WriteTaskEvent(domain.TaskEvent{PromptTokens: 7, CompletionTokens: 3, Timestamp: now.Add(-time.Hour)})

	series := ts.TokenSeries(3)
	if len(series) != 3 {
		t.Fatalf("got %d points, want 3", len(series))
	}
	// Oldest first: [-2h] [-1h] [current].
	if series[2].InputTokens != 150 || series[2].OutputTokens != 30 || series[2].Calls != 2 {
		t.Fatalf("current hour = %+v, want 150/30/2", series[2])
	}
	if series[1].InputTokens != 7 || series[1].Calls != 1 {
		t.Fatalf("previous hour = %+v, want 7 in / 1 call", series[1])
	}
}

// An idle hour must come back as a ZERO point, not be omitted. A sparkline built
// from a sparse series compresses idle time and draws a quiet night as
// continuous activity — the gap is the information.
func TestTokenSeries_IdleHoursAreZeroPointsNotGaps(t *testing.T) {
	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	ts := NewTokenSeries()
	fixedNow(ts, now)
	w := ts.Wrap(nil)

	_ = w.WriteTaskEvent(domain.TaskEvent{PromptTokens: 10, Timestamp: now})

	series := ts.TokenSeries(5)
	if len(series) != 5 {
		t.Fatalf("got %d points, want 5 contiguous hours", len(series))
	}
	for i := 0; i < 4; i++ {
		if series[i].Calls != 0 || series[i].InputTokens != 0 {
			t.Fatalf("point %d should be an idle zero: %+v", i, series[i])
		}
		// Contiguous and one hour apart, so a chart can plot on the timestamps.
		if i > 0 && !series[i].HourStart.Equal(series[i-1].HourStart.Add(time.Hour)) {
			t.Fatalf("points are not contiguous: %v then %v", series[i-1].HourStart, series[i].HourStart)
		}
	}
}

// A step that never reached a model must not inflate the call count — otherwise
// the series measures steps rather than spend.
func TestTokenSeries_ZeroTokenEventIsNotACall(t *testing.T) {
	now := time.Now()
	ts := NewTokenSeries()
	w := ts.Wrap(nil)

	_ = w.WriteTaskEvent(domain.TaskEvent{Timestamp: now}) // cache hit / thought step

	for _, p := range ts.TokenSeries(2) {
		if p.Calls != 0 {
			t.Fatalf("a zero-token event was counted as a call: %+v", p)
		}
	}
}

// The tap must forward, and must not swallow the wrapped writer's error — the
// series is an observer, not a replacement.
func TestTokenSeries_ForwardsToTheWrappedWriter(t *testing.T) {
	inner := &recordingWriter{}
	ts := NewTokenSeries()
	w := ts.Wrap(inner)

	if err := w.WriteTaskEvent(domain.TaskEvent{PromptTokens: 5, Timestamp: time.Now()}); err != nil {
		t.Fatalf("WriteTaskEvent: %v", err)
	}
	if len(inner.events) != 1 {
		t.Fatalf("the wrapped writer received %d events, want 1", len(inner.events))
	}

	inner.err = errors.New("store down")
	if err := w.WriteTaskEvent(domain.TaskEvent{PromptTokens: 5, Timestamp: time.Now()}); err == nil {
		t.Fatal("the wrapped writer's error was swallowed")
	}
	// …and the usage was still recorded: the tokens were spent regardless of
	// whether the durable write succeeded.
	var total int64
	for _, p := range ts.TokenSeries(2) {
		total += p.InputTokens
	}
	if total != 10 {
		t.Fatalf("recorded %d input tokens, want 10 — a failed write still spent them", total)
	}
}

// One accumulator must be shared across wrappers: the kernel resolves its event
// writer per execution, so a fresh accumulator each time would reset the series
// on every plan.
func TestTokenSeries_AccumulatorIsSharedAcrossWrappers(t *testing.T) {
	ts := NewTokenSeries()
	now := time.Now()

	_ = ts.Wrap(nil).WriteTaskEvent(domain.TaskEvent{PromptTokens: 3, Timestamp: now})
	_ = ts.Wrap(nil).WriteTaskEvent(domain.TaskEvent{PromptTokens: 4, Timestamp: now})

	var total int64
	for _, p := range ts.TokenSeries(2) {
		total += p.InputTokens
	}
	if total != 7 {
		t.Fatalf("total = %d, want 7 — each Wrap must share one accumulator", total)
	}
}

func TestTokenSeries_EvictsBeyondRetention(t *testing.T) {
	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	ts := NewTokenSeries()
	fixedNow(ts, now)
	w := ts.Wrap(nil)

	// Well outside the 48h window.
	_ = w.WriteTaskEvent(domain.TaskEvent{PromptTokens: 99, Timestamp: now.Add(-100 * time.Hour)})
	_ = w.WriteTaskEvent(domain.TaskEvent{PromptTokens: 1, Timestamp: now})

	var total int64
	for _, p := range ts.TokenSeries(DefaultTokenRetentionHours) {
		total += p.InputTokens
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1 — the ancient bucket should have been evicted", total)
	}
}

// ReadTaskEvent must pass through, or decorating strips the read-back the
// verification worker needs to update an event in place.
func TestTokenSeries_ReadTaskEventPassesThrough(t *testing.T) {
	ts := NewTokenSeries()

	// A plain writer with no read-back: the wrapper must not pretend to have one.
	got, err := ts.Wrap(&recordingWriter{}).ReadTaskEvent("t1")
	if err != nil || got != nil {
		t.Fatalf("got %+v, %v; want nil, nil for a writer with no read-back", got, err)
	}
}
