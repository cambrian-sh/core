package domain

import (
	"context"

	"fmt"
	"strings"
	"time"
)

// Event time in memory (ADR-0134).
//
// This file is the ONE definition of the event-time vocabulary the memory lane
// speaks. The proto planes carry these values as strings (cambrian.proto
// IngestMemoryRequest / MemoryResult; operator.proto IngestMemoryOpRequest /
// MemoryOp / MemoryCitation, contract 0099) and their comments repeat the list;
// TestEventTimeVocabularyMatchesContract keeps the two in step. Owner decision
// ADR-0134 Q9: defined once here, used by memory now; mapping the evidence lane's
// clock labels (ADR-0105) onto it is a follow-up.
//
// Two clocks, never confused:
//   - event time  — when the content happened (valid time). This file.
//   - ingest time — when Cambrian recorded it (documents/chunks created_at).
//
// `as_of` is deliberately NOT a name used here: it is reserved for transaction
// time ("what did our records hold at T"), as on the premium substrate plane.

// TimeSource says where an event time came from. A defaulted time is never
// presented as a real one: TimeSourceIngestDefault means "nothing was known, this
// IS the ingest time".
type TimeSource string

const (
	// TimeSourceExplicit: the caller stated the time (occurred_at / part times).
	TimeSourceExplicit TimeSource = "explicit"
	// TimeSourceSourceMetadata: read from the source itself by a declared rule
	// (ADR-0134 D3; the ingress studio's TimeSource spec), never sniffed.
	TimeSourceSourceMetadata TimeSource = "source_metadata"
	// TimeSourceInferred: derived by a declared rule.
	TimeSourceInferred TimeSource = "inferred"
	// TimeSourceIngestDefault: no time was known; the event time equals the
	// ingest time. A stored row with no time_source reads as this.
	TimeSourceIngestDefault TimeSource = "ingest_default"
	// TimeSourceClampedFuture: the caller stated a time later than ingest + the
	// configured tolerance; the event time was clamped to the ingest time and the
	// claimed time kept beside it (ADR-0134 Q4).
	TimeSourceClampedFuture TimeSource = "clamped_future"
)

// TimeSources lists the vocabulary in contract order.
var TimeSources = []TimeSource{
	TimeSourceExplicit, TimeSourceSourceMetadata, TimeSourceInferred,
	TimeSourceIngestDefault, TimeSourceClampedFuture,
}

// Valid reports whether s is in the vocabulary.
func (s TimeSource) Valid() bool {
	for _, v := range TimeSources {
		if s == v {
			return true
		}
	}
	return false
}

// TimePrecision says how precise an event time is, so a date-only source is
// never shown as an exact instant (ADR-0134 Q3).
type TimePrecision string

const (
	TimePrecisionYear   TimePrecision = "year"
	TimePrecisionMonth  TimePrecision = "month"
	TimePrecisionDay    TimePrecision = "day"
	TimePrecisionHour   TimePrecision = "hour"
	TimePrecisionMinute TimePrecision = "minute"
	TimePrecisionSecond TimePrecision = "second"
)

// TimePrecisions lists the vocabulary from coarsest to finest.
var TimePrecisions = []TimePrecision{
	TimePrecisionYear, TimePrecisionMonth, TimePrecisionDay,
	TimePrecisionHour, TimePrecisionMinute, TimePrecisionSecond,
}

// Valid reports whether p is in the vocabulary.
func (p TimePrecision) Valid() bool {
	for _, v := range TimePrecisions {
		if p == v {
			return true
		}
	}
	return false
}

// DocumentPart is one timed part of a multi-part document — a chat session's
// turn (ADR-0134 D1/Q5). The part's text lives inside ExternalDocument.Body at
// [Offset, Offset+Length) (byte offsets), so the chunker can map any chunk back to
// the parts it covers: a chunk's event time is the earliest OccurredAt among
// them, its span end the latest. Part boundaries never force chunk boundaries.
type DocumentPart struct {
	Offset int
	Length int
	// OccurredAt is the part's event time in UTC; zero = this part has no time.
	OccurredAt time.Time
}

// MemoryPartInput is one part as a caller supplied it (wire → domain), before the
// body is assembled.
type MemoryPartInput struct {
	Text       string
	OccurredAt *time.Time // nil = no time for this part
}

// PartSeparator joins part texts into the document body. A blank line keeps each
// part a paragraph of its own for the paragraph-based chunkers.
const PartSeparator = "\n\n"

// DefaultEventTimeFutureTolerance is the default for
// execution.ingestion.event_time_future_tolerance_ms (ADR-0134 Q4).
const DefaultEventTimeFutureTolerance = 5 * time.Minute

// IngestEventTime is the resolved event time of one ingest. It travels on
// domain.ExternalDocument (Timestamp = EventTime) into the ingestion pipeline.
type IngestEventTime struct {
	// EventTime is the document's event time (UTC). For a multi-part document it
	// is the earliest part time unless the caller stated occurred_at.
	EventTime time.Time
	// EventTimeEnd is the latest part time; zero for a single instant.
	EventTimeEnd time.Time
	Source       TimeSource
	Precision    TimePrecision
	// ClaimedTime is the caller's stated time when it was clamped
	// (Source == TimeSourceClampedFuture); zero otherwise.
	ClaimedTime time.Time
	// Body is the assembled body for a multi-part ingest ("" when no parts).
	Body  string
	Parts []DocumentPart
}

// ResolveIngestEventTime validates and resolves the event-time fields of one
// memory ingest — the single implementation both ingest planes call (agent
// IngestMemory, operator IngestMemory), so the UTC / precision / clamp rules
// cannot drift between them (ADR-0134 Q3/Q4/Q5).
//
//   - occurredAt nil and no timed part: EventTime = now, Source = ingest_default.
//   - a stated time later than now + tolerance: EventTime = now, ClaimedTime =
//     the stated time, Source = clamped_future. Part times are clamped the same
//     way, so a future-dated turn cannot place a chunk ahead of the ingest.
//   - precision "" defaults to "second"; an unknown value is an error.
//
// All times are returned in UTC. Callers convert wire Timestamps (always absolute
// UTC instants, so "naive" input cannot reach here) and validate them first.
func ResolveIngestEventTime(occurredAt *time.Time, parts []MemoryPartInput, precision string, now time.Time, tolerance time.Duration) (IngestEventTime, error) {
	now = now.UTC()
	if tolerance < 0 {
		tolerance = 0
	}
	out := IngestEventTime{Precision: TimePrecisionSecond}
	if precision != "" {
		p := TimePrecision(strings.ToLower(strings.TrimSpace(precision)))
		if !p.Valid() {
			return IngestEventTime{}, fmt.Errorf("time_precision %q is not one of %v", precision, TimePrecisions)
		}
		out.Precision = p
	}
	limit := now.Add(tolerance)
	// claimed is the first stated time that lay beyond the tolerance (occurred_at
	// is examined first, so it wins over a part time).
	var claimed time.Time
	clamp := func(t time.Time) time.Time {
		t = t.UTC()
		if t.After(limit) {
			if claimed.IsZero() {
				claimed = t
			}
			return now
		}
		return t
	}

	var stated time.Time
	if occurredAt != nil {
		stated = clamp(*occurredAt)
	}

	var earliest, latest time.Time
	if len(parts) > 0 {
		var b strings.Builder
		out.Parts = make([]DocumentPart, 0, len(parts))
		for i, p := range parts {
			if i > 0 {
				b.WriteString(PartSeparator)
			}
			dp := DocumentPart{Offset: b.Len(), Length: len(p.Text)}
			b.WriteString(p.Text)
			if p.OccurredAt != nil {
				dp.OccurredAt = clamp(*p.OccurredAt)
				if earliest.IsZero() || dp.OccurredAt.Before(earliest) {
					earliest = dp.OccurredAt
				}
				if dp.OccurredAt.After(latest) {
					latest = dp.OccurredAt
				}
			}
			out.Parts = append(out.Parts, dp)
		}
		out.Body = b.String()
	}

	switch {
	case occurredAt != nil:
		out.EventTime = stated
	case !earliest.IsZero():
		out.EventTime = earliest
	default:
		out.EventTime = now
		out.Source = TimeSourceIngestDefault
		return out, nil
	}
	out.Source = TimeSourceExplicit
	if latest.After(out.EventTime) {
		out.EventTimeEnd = latest
	}
	if !claimed.IsZero() {
		out.Source = TimeSourceClampedFuture
		out.ClaimedTime = claimed
	}
	return out, nil
}

// EventTimeQuery is the event-time part of a memory query (ADR-0134 D4, contract
// 0099). It rides the CONTEXT from the transport handlers to QueryService, which
// copies the window onto SearchOptions (Since / Until) for the SQL push-down, and
// re-asserts it in Go where candidates from every lane converge (Admits). The
// context is the channel because the memory read port (MemorySearcher) has no
// options argument and the agentic loop issues many searches per question; this is
// not safety-relevant state (AGENTS.md §2.3 concerns enforcement), so a missing value
// only means "no window".
type EventTimeQuery struct {
	// ReferenceTime is the asker's "now"; zero = the server clock. It is not a
	// filter. Its reader is the Stage-A recency term (internal/memory
	// applyStageABlend): a candidate dated after the asker's now gets no recency
	// credit and does not stretch the recency range. It also travels to the
	// retrieval decision record (ADR-0103), so a receipt says which "now" an answer
	// was computed against.
	ReferenceTime time.Time
	// EventAfter (inclusive) / EventBefore (exclusive) bound the event-time
	// window; a hit is kept when its span [event_time, event_time_end] overlaps
	// it. Zero = open.
	EventAfter  time.Time
	EventBefore time.Time
}

// HasWindow reports whether the query restricts event time at all.
func (q EventTimeQuery) HasWindow() bool {
	return !q.EventAfter.IsZero() || !q.EventBefore.IsZero()
}

// IsZero reports whether the query carries nothing.
func (q EventTimeQuery) IsZero() bool {
	return q.ReferenceTime.IsZero() && !q.HasWindow()
}

// Validate rejects an empty or inverted window.
func (q EventTimeQuery) Validate() error {
	if !q.EventAfter.IsZero() && !q.EventBefore.IsZero() && !q.EventAfter.Before(q.EventBefore) {
		return fmt.Errorf("event_after (%s) must be before event_before (%s)",
			q.EventAfter.UTC().Format(time.RFC3339), q.EventBefore.UTC().Format(time.RFC3339))
	}
	return nil
}

// Admits reports whether a stored row lies in the window: its span (StoredSpan, the
// mirror of the SQL span) overlaps [EventAfter, EventBefore) — span end at or after
// EventAfter, span start before EventBefore. A query with no window admits every row.
//
// This is the in-memory form of the predicate the pgvector adapter pushes into SQL
// beside the read scope. It is re-applied where candidates from every lane converge
// (dense, lexical, and the by-id enrichment lanes that never saw SearchOptions), for
// the same reason the session boundary is (BRAIN-01).
func (q EventTimeQuery) Admits(d Document) bool {
	if !q.HasWindow() {
		return true
	}
	start, end := StoredSpan(d)
	if !q.EventAfter.IsZero() && end.Before(q.EventAfter) {
		return false
	}
	if !q.EventBefore.IsZero() && !start.Before(q.EventBefore) {
		return false
	}
	return true
}

type eventTimeQueryKey struct{}

// WithEventTimeQuery carries q on ctx (a zero q is not stored).
func WithEventTimeQuery(ctx context.Context, q EventTimeQuery) context.Context {
	if q.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, eventTimeQueryKey{}, q)
}

// EventTimeQueryFromContext returns the query carried on ctx, if any.
func EventTimeQueryFromContext(ctx context.Context) (EventTimeQuery, bool) {
	q, ok := ctx.Value(eventTimeQueryKey{}).(EventTimeQuery)
	return q, ok
}

// EventStamp is the event time a stored row carries (ADR-0134 D2): the typed
// event_time / event_time_end / time_source / time_precision / claimed_time columns
// of chunks and documents (migration 0019). It is embedded in Document and
// SourceDocument, so one value travels from the ingest path to the adapter.
//
// The zero value means "this writer states no event time": the adapter then writes
// NULLs on insert and LEAVES a stored stamp alone on update (ADR-0134 Q6). A row with
// no stamp reads as its ingest clock (HitEventTime).
type EventStamp struct {
	// EventTime is when the content happened (UTC). Zero = not stated.
	EventTime time.Time
	// EventTimeEnd is the span end (latest part time) of a multi-part row; zero for
	// a single instant.
	EventTimeEnd time.Time
	// TimeSource says where EventTime came from; "" with a non-zero EventTime is
	// stored as NULL and reads as ingest_default.
	TimeSource TimeSource
	// TimePrecision is how exact EventTime is; "" reads as second.
	TimePrecision TimePrecision
	// ClaimedTime is a caller's stated time that was clamped (clamped_future).
	ClaimedTime time.Time
}

// IsZero reports whether the stamp states nothing.
func (s EventStamp) IsZero() bool { return s.EventTime.IsZero() }

// StampOf returns the document-level stamp of an ingest (the document's own event
// time and span). Chunks take ChunkStamps instead, which narrows it per chunk.
func (d ExternalDocument) StampOf() EventStamp {
	return EventStamp{
		EventTime:     d.Timestamp.UTC(),
		EventTimeEnd:  utcOrZero(d.EventTimeEnd),
		TimeSource:    d.TimeSource,
		TimePrecision: d.TimePrecision,
		ClaimedTime:   utcOrZero(d.ClaimedTime),
	}
}

// ChunkStamps assigns each chunk of a document its own event time (ADR-0134 Q5):
// the earliest time among the timed parts the chunk covers, and, when it covers parts
// at different times, the latest as its span end. Chunk boundaries are never moved.
//
// spans[i] is chunk i's byte range in doc.Body, or ok=false when the chunk's text could
// not be located there (a structure parser that rewrote the text). A chunk that is not
// located, or that covers no timed part, takes the DOCUMENT's stamp — its time and its
// whole span — which is the honest statement "somewhere within this document's range":
// it never invents a narrower time than the evidence supports, and a window filter
// over the span still finds it.
//
// A document with no parts gives every chunk the document stamp.
func ChunkStamps(doc ExternalDocument, spans []ChunkSpan) []EventStamp {
	base := doc.StampOf()
	out := make([]EventStamp, len(spans))
	for i, sp := range spans {
		out[i] = base
		if len(doc.Parts) == 0 || !sp.OK {
			continue
		}
		var earliest, latest time.Time
		for _, p := range doc.Parts {
			if p.OccurredAt.IsZero() {
				continue
			}
			// Half-open overlap of [sp.Start, sp.End) and [p.Offset, p.Offset+p.Length).
			if sp.Start < p.Offset+p.Length && p.Offset < sp.End {
				t := p.OccurredAt.UTC()
				if earliest.IsZero() || t.Before(earliest) {
					earliest = t
				}
				if t.After(latest) {
					latest = t
				}
			}
		}
		if earliest.IsZero() {
			continue
		}
		out[i].EventTime = earliest
		out[i].EventTimeEnd = time.Time{}
		if latest.After(earliest) {
			out[i].EventTimeEnd = latest
		}
		// The claimed (clamped) time belongs to the document's statement; a chunk whose
		// parts were all within tolerance carries none of it. Source stays the
		// document's: explicit, or clamped_future when any stated time was clamped.
	}
	return out
}

// ChunkSpan is a chunk's byte range in its document's body; OK=false = not located.
type ChunkSpan struct {
	Start, End int
	OK         bool
}

// LocateChunkSpans finds each chunk body in body, in order. Chunkers emit chunks in
// document order as (trimmed) substrings of the body, so the search for chunk i starts
// where chunk i-1 started (chunks may overlap); a chunk not found from there is searched
// from the beginning once (a chunker that reorders).
//
// A chunk that is not a verbatim substring — the structure path joins the leaves it
// merges with a single newline where the body had a blank line — is located by its
// first and last non-empty lines instead: the span runs from the first line's start to
// the last line's end. Otherwise the chunk is reported not located (ChunkStamps then
// gives it the whole document span). An empty chunk is never located.
func LocateChunkSpans(body string, chunkBodies []string) []ChunkSpan {
	out := make([]ChunkSpan, len(chunkBodies))
	cursor := 0
	find := func(needle string, from int) int {
		if j := strings.Index(body[from:], needle); j >= 0 {
			return from + j
		}
		if j := strings.Index(body, needle); j >= 0 {
			return j
		}
		return -1
	}
	for i, cb := range chunkBodies {
		if strings.TrimSpace(cb) == "" {
			continue
		}
		if at := find(cb, cursor); at >= 0 {
			out[i] = ChunkSpan{Start: at, End: at + len(cb), OK: true}
			cursor = at
			continue
		}
		first, last := edgeLines(cb)
		start := find(first, cursor)
		if start < 0 {
			continue
		}
		end := -1
		if j := strings.Index(body[start:], last); j >= 0 {
			end = start + j + len(last)
		}
		if end < 0 {
			continue
		}
		out[i] = ChunkSpan{Start: start, End: end, OK: true}
		cursor = start
	}
	return out
}

// edgeLines returns the first and last non-empty trimmed lines of s.
func edgeLines(s string) (first, last string) {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		if first == "" {
			first = l
		}
		last = l
	}
	return first, last
}

func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

// MetaEventTimestamp is the metadata key rows written before the typed columns
// (migration 0019) carried their event time under: chunker output (option_c's
// "timestamp") and agent-authored memories (internal/memory/agent.go). HitEventTime
// still reads it for such rows; the ingest path no longer writes it.
const MetaEventTimestamp = "timestamp"

// HitTime is the event time a retrieval hit reports on the wire.
type HitTime struct {
	EventTime    time.Time
	EventTimeEnd time.Time
	Source       TimeSource
	Precision    TimePrecision
}

// End returns the end of the hit's span: EventTimeEnd, or EventTime for an instant.
func (h HitTime) End() time.Time {
	if h.EventTimeEnd.After(h.EventTime) {
		return h.EventTimeEnd
	}
	return h.EventTime
}

// HitEventTime derives a hit's event time from a stored document — the ONE reader
// every plane's hit mapping, the Stage-A recency term and the event-window mirror
// share, so "what time is this chunk" has one answer. Order:
//
//  1. the typed columns (EventStamp, migration 0019);
//  2. a legacy metadata "timestamp" (rows written before 0019; agent memories);
//  3. created_at, the ingest clock, labelled ingest_default.
//
// Step 2 exists only for display and the recency term, so ranking on a store written
// before 0019 does not change. The event WINDOW does not use it: it reads StoredSpan,
// the exact mirror of the SQL span (a metadata string cannot be range-indexed, and a
// Go filter that admitted rows the SQL had excluded would make the window depend on
// which lane found a row). A legacy row's metadata time becomes windowable when it is
// re-ingested with a stated time. All times are returned in UTC.
func HitEventTime(d Document) HitTime {
	h := HitTime{Source: TimeSourceIngestDefault, Precision: TimePrecisionSecond}
	switch {
	case !d.EventTime.IsZero():
		h.EventTime = d.EventTime.UTC()
		h.EventTimeEnd = utcOrZero(d.EventTimeEnd)
		if d.TimeSource.Valid() {
			h.Source = d.TimeSource
		}
		if d.TimePrecision.Valid() {
			h.Precision = d.TimePrecision
		}
		return h
	case d.Metadata != nil:
		if v, ok := d.Metadata[MetaEventTimestamp].(string); ok && v != "" {
			h.EventTime = utcOrZero(parseEventTime(v))
		}
	}
	if h.EventTime.IsZero() {
		h.EventTime = utcOrZero(d.CreatedAt)
	}
	return h
}

// StoredSpan is the event-time span the SQL window reads for a row — the in-memory
// mirror of migrate.EventStartExpr / EventEndExpr: the typed columns, else created_at.
// Used wherever the window is re-asserted in Go, so a row is in or out of a window
// regardless of which retrieval lane found it.
func StoredSpan(d Document) (start, end time.Time) {
	if !d.EventTime.IsZero() {
		start = d.EventTime.UTC()
		end = start
		if d.EventTimeEnd.After(d.EventTime) {
			end = d.EventTimeEnd.UTC()
		}
		return start, end
	}
	start = utcOrZero(d.CreatedAt)
	return start, start
}

func parseEventTime(v string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}
