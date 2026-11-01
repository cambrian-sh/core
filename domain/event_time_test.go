package domain_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func tp(t time.Time) *time.Time { return &t }

// No stated time: the event time IS the ingest clock and says so (ADR-0134 D2:
// a defaulted time is never presented as a real one).
func TestResolveIngestEventTime_DefaultsToIngestClock(t *testing.T) {
	t.Parallel()
	et, err := domain.ResolveIngestEventTime(nil, nil, "", t0, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !et.EventTime.Equal(t0) || et.Source != domain.TimeSourceIngestDefault || et.Precision != domain.TimePrecisionSecond {
		t.Fatalf("got %+v", et)
	}
	if !et.EventTimeEnd.IsZero() || !et.ClaimedTime.IsZero() {
		t.Fatalf("default must carry no span or claimed time: %+v", et)
	}
}

func TestResolveIngestEventTime_ExplicitIsUTCWithPrecision(t *testing.T) {
	t.Parallel()
	ist := time.FixedZone("IST", 5*3600+1800)
	stated := time.Date(2023, 5, 20, 7, 51, 0, 0, ist)
	et, err := domain.ResolveIngestEventTime(tp(stated), nil, "Day", t0, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !et.EventTime.Equal(stated) || et.EventTime.Location() != time.UTC {
		t.Fatalf("event time = %v, want %v in UTC", et.EventTime, stated)
	}
	if et.Source != domain.TimeSourceExplicit || et.Precision != domain.TimePrecisionDay {
		t.Fatalf("got %+v", et)
	}
}

func TestResolveIngestEventTime_RejectsUnknownPrecision(t *testing.T) {
	t.Parallel()
	if _, err := domain.ResolveIngestEventTime(tp(t0), nil, "fortnight", t0, 0); err == nil {
		t.Fatal("unknown precision must be an error")
	}
}

// ADR-0134 Q4: a future time beyond the tolerance is stored, clamped to the ingest
// time, and labelled; within the tolerance it is taken as stated.
func TestResolveIngestEventTime_ClampsFutureBeyondTolerance(t *testing.T) {
	t.Parallel()
	within := t0.Add(4 * time.Minute)
	et, _ := domain.ResolveIngestEventTime(tp(within), nil, "", t0, 5*time.Minute)
	if !et.EventTime.Equal(within) || et.Source != domain.TimeSourceExplicit {
		t.Fatalf("within tolerance: %+v", et)
	}
	far := t0.Add(48 * time.Hour)
	et, _ = domain.ResolveIngestEventTime(tp(far), nil, "", t0, 5*time.Minute)
	if !et.EventTime.Equal(t0) || !et.ClaimedTime.Equal(far) || et.Source != domain.TimeSourceClampedFuture {
		t.Fatalf("beyond tolerance: %+v", et)
	}
}

// ADR-0134 Q5: parts join into the body, keep their offsets, and the document's
// time is the earliest part time with the latest as the span end.
func TestResolveIngestEventTime_PartsEarliestAndSpan(t *testing.T) {
	t.Parallel()
	a, b := t0.Add(-3*time.Hour), t0.Add(-1*time.Hour)
	parts := []domain.MemoryPartInput{
		{Text: "user: hi", OccurredAt: tp(b)},
		{Text: "assistant: hello"},
		{Text: "user: bye", OccurredAt: tp(a)},
	}
	et, err := domain.ResolveIngestEventTime(nil, parts, "minute", t0, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := "user: hi\n\nassistant: hello\n\nuser: bye"; et.Body != want {
		t.Fatalf("body = %q", et.Body)
	}
	if !et.EventTime.Equal(a) || !et.EventTimeEnd.Equal(b) || et.Source != domain.TimeSourceExplicit {
		t.Fatalf("got %+v", et)
	}
	for i, p := range et.Parts {
		if got := et.Body[p.Offset : p.Offset+p.Length]; got != parts[i].Text {
			t.Fatalf("part %d offsets point at %q", i, got)
		}
	}
	if !et.Parts[1].OccurredAt.IsZero() {
		t.Fatal("an untimed part must stay untimed")
	}
}

// occurred_at wins over the part times for the document's event time; the parts
// still supply the span end.
func TestResolveIngestEventTime_OccurredAtWinsOverParts(t *testing.T) {
	t.Parallel()
	stated := t0.Add(-24 * time.Hour)
	parts := []domain.MemoryPartInput{{Text: "x", OccurredAt: tp(t0.Add(-2 * time.Hour))}}
	et, _ := domain.ResolveIngestEventTime(tp(stated), parts, "", t0, time.Minute)
	if !et.EventTime.Equal(stated) || !et.EventTimeEnd.Equal(t0.Add(-2*time.Hour)) {
		t.Fatalf("got %+v", et)
	}
}

func TestResolveIngestEventTime_ClampsFuturePart(t *testing.T) {
	t.Parallel()
	far := t0.Add(time.Hour)
	parts := []domain.MemoryPartInput{{Text: "x", OccurredAt: tp(far)}}
	et, _ := domain.ResolveIngestEventTime(nil, parts, "", t0, time.Minute)
	if et.Source != domain.TimeSourceClampedFuture || !et.EventTime.Equal(t0) || !et.ClaimedTime.Equal(far) {
		t.Fatalf("got %+v", et)
	}
}

func TestEventTimeQuery_ValidateAndAdmits(t *testing.T) {
	t.Parallel()
	if err := (domain.EventTimeQuery{ReferenceTime: t0}).Validate(); err != nil {
		t.Fatalf("reference time alone must be accepted: %v", err)
	}
	if err := (domain.EventTimeQuery{EventAfter: t0, EventBefore: t0}).Validate(); err == nil {
		t.Fatal("an empty window is the caller's error")
	}
	if err := (domain.EventTimeQuery{EventAfter: t0}).Validate(); err != nil {
		t.Fatalf("an open window is valid: %v", err)
	}
	// Span overlap with [after, before): after inclusive, before exclusive.
	after, before := t0, t0.Add(24*time.Hour)
	w := domain.EventTimeQuery{EventAfter: after, EventBefore: before}
	at := func(start, end time.Time) domain.Document {
		return domain.Document{EventStamp: domain.EventStamp{EventTime: start, EventTimeEnd: end}}
	}
	cases := []struct {
		name string
		d    domain.Document
		want bool
	}{
		{"instant at after (inclusive)", at(after, time.Time{}), true},
		{"instant at before (exclusive)", at(before, time.Time{}), false},
		{"instant just before after", at(after.Add(-time.Second), time.Time{}), false},
		{"span ending at after overlaps", at(after.Add(-time.Hour), after), true},
		{"span straddling the window", at(after.Add(-time.Hour), before.Add(time.Hour)), true},
		{"span entirely before", at(after.Add(-2*time.Hour), after.Add(-time.Hour)), false},
		{"unstamped row is windowed by created_at", domain.Document{CreatedAt: after.Add(time.Hour)}, true},
		{"legacy metadata time is not windowable", domain.Document{CreatedAt: before.Add(time.Hour),
			Metadata: map[string]any{"timestamp": after.Add(time.Hour).Format(time.RFC3339)}}, false},
	}
	for _, c := range cases {
		if got := w.Admits(c.d); got != c.want {
			t.Errorf("%s: Admits = %v, want %v", c.name, got, c.want)
		}
	}
	if !(domain.EventTimeQuery{}).Admits(at(time.Time{}, time.Time{})) {
		t.Error("no window admits everything")
	}
}

func TestEventTimeQuery_RidesTheContext(t *testing.T) {
	t.Parallel()
	ctx := domain.WithEventTimeQuery(context.Background(), domain.EventTimeQuery{})
	if _, ok := domain.EventTimeQueryFromContext(ctx); ok {
		t.Fatal("a zero query must not be stored")
	}
	ctx = domain.WithEventTimeQuery(context.Background(), domain.EventTimeQuery{ReferenceTime: t0})
	if q, ok := domain.EventTimeQueryFromContext(ctx); !ok || !q.ReferenceTime.Equal(t0) {
		t.Fatalf("got %+v %v", q, ok)
	}
}

func TestHitEventTime(t *testing.T) {
	t.Parallel()
	// Legacy row: a timestamp but no source is the ingest clock.
	h := domain.HitEventTime(domain.Document{Metadata: map[string]any{"timestamp": "2023-05-20T07:51:00Z"}})
	if h.Source != domain.TimeSourceIngestDefault || h.EventTime.Year() != 2023 {
		t.Fatalf("legacy: %+v", h)
	}
	// Stamped row: the typed columns (migration 0019) win over a metadata copy.
	h = domain.HitEventTime(domain.Document{
		Metadata: map[string]any{"timestamp": "2026-01-01T00:00:00Z"},
		EventStamp: domain.EventStamp{
			EventTime:    time.Date(2023, 5, 20, 7, 51, 0, 0, time.UTC),
			EventTimeEnd: time.Date(2023, 5, 20, 9, 0, 0, 0, time.UTC),
			TimeSource:   domain.TimeSourceExplicit, TimePrecision: domain.TimePrecisionMinute,
		},
	})
	if h.Source != domain.TimeSourceExplicit || h.Precision != domain.TimePrecisionMinute ||
		h.EventTime.Year() != 2023 || h.EventTimeEnd.Hour() != 9 {
		t.Fatalf("stamped: %+v", h)
	}
	// A stamped time with no source (a writer outside the resolver) reads as the
	// ingest default; an unknown precision reads as second.
	h = domain.HitEventTime(domain.Document{EventStamp: domain.EventStamp{EventTime: t0, TimePrecision: "fortnight"}})
	if h.Source != domain.TimeSourceIngestDefault || h.Precision != domain.TimePrecisionSecond {
		t.Fatalf("unsourced stamp: %+v", h)
	}
	// No time at all: created_at, labelled ingest_default; a metadata source key is
	// not a column and is ignored.
	h = domain.HitEventTime(domain.Document{CreatedAt: t0, Metadata: map[string]any{"time_source": "explicit"}})
	if !h.EventTime.Equal(t0) || h.Source != domain.TimeSourceIngestDefault {
		t.Fatalf("created_at fallback: %+v", h)
	}
}

// StoredSpan is the SQL span's mirror: columns, else created_at — never metadata.
func TestStoredSpan(t *testing.T) {
	t.Parallel()
	end := t0.Add(time.Hour)
	s, e := domain.StoredSpan(domain.Document{CreatedAt: t0.Add(48 * time.Hour), EventStamp: domain.EventStamp{EventTime: t0, EventTimeEnd: end}})
	if !s.Equal(t0) || !e.Equal(end) {
		t.Fatalf("stamped span = %v..%v", s, e)
	}
	legacy := domain.Document{CreatedAt: t0, Metadata: map[string]any{"timestamp": "2019-01-01T00:00:00Z"}}
	if s, e := domain.StoredSpan(legacy); !s.Equal(t0) || !e.Equal(t0) {
		t.Fatalf("legacy span must be created_at, got %v..%v", s, e)
	}
}

// ADR-0134 Q5: a chunk takes the earliest time of the parts it covers and the
// latest as its span end; an unlocated chunk takes the whole document span.
func TestChunkStamps(t *testing.T) {
	t.Parallel()
	a, b, c := t0, t0.Add(time.Hour), t0.Add(2*time.Hour)
	et, err := domain.ResolveIngestEventTime(nil, []domain.MemoryPartInput{
		{Text: "turn one", OccurredAt: &a}, {Text: "turn two", OccurredAt: &b}, {Text: "turn three", OccurredAt: &c},
	}, "minute", t0.Add(24*time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var doc domain.ExternalDocument
	doc.Body = et.Body
	doc.ApplyEventTime(et)
	// chunk 0 = part 1; chunk 1 spans parts 2 and 3; chunk 2 is not in the body.
	spans := domain.LocateChunkSpans(doc.Body, []string{"turn one", "turn two" + domain.PartSeparator + "turn three", "rewritten by a parser"})
	if !spans[0].OK || !spans[1].OK || spans[2].OK {
		t.Fatalf("spans = %+v", spans)
	}
	st := domain.ChunkStamps(doc, spans)
	if !st[0].EventTime.Equal(a) || !st[0].EventTimeEnd.IsZero() {
		t.Fatalf("chunk 0 = %+v", st[0])
	}
	if !st[1].EventTime.Equal(b) || !st[1].EventTimeEnd.Equal(c) {
		t.Fatalf("chunk 1 = %+v", st[1])
	}
	if !st[2].EventTime.Equal(a) || !st[2].EventTimeEnd.Equal(c) {
		t.Fatalf("unlocated chunk must carry the document span, got %+v", st[2])
	}
	for i, s := range st {
		if s.TimeSource != domain.TimeSourceExplicit || s.TimePrecision != domain.TimePrecisionMinute {
			t.Fatalf("chunk %d source/precision = %+v", i, s)
		}
	}
	// No parts: every chunk carries the document stamp.
	single := domain.ExternalDocument{Body: "x" + domain.PartSeparator + "y", Timestamp: a, TimeSource: domain.TimeSourceExplicit}
	for _, s := range domain.ChunkStamps(single, domain.LocateChunkSpans(single.Body, []string{"x", "y"})) {
		if !s.EventTime.Equal(a) {
			t.Fatalf("single-part chunk = %+v", s)
		}
	}
}

// The vocabulary is defined once (here) and repeated in the proto comments for
// client authors. This keeps the two from drifting apart.
func TestEventTimeVocabularyMatchesContract(t *testing.T) {
	t.Parallel()
	for _, f := range []string{"../api/proto/cambrian.proto", "../api/proto/operator.proto"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		for _, s := range domain.TimeSources {
			if !strings.Contains(src, `"`+string(s)+`"`) {
				t.Errorf("%s does not document time_source %q", f, s)
			}
		}
		for _, p := range domain.TimePrecisions {
			if !strings.Contains(src, `"`+string(p)+`"`) {
				t.Errorf("%s does not document time_precision %q", f, p)
			}
		}
	}
}
