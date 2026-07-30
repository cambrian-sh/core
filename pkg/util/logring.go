package util

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// The kernel's in-process log retention.
//
// Logs went to stdout and nowhere else, so the only way to read what a running
// kernel had done was to have been attached to it at the time. Nothing could ask
// "what happened an hour ago" — not the operator console, not a support tool,
// not the process itself.
//
// This keeps a BOUNDED window in memory. Deliberately not a file and not a
// database: the kernel already writes to stdout, where a supervisor or a log
// shipper can do durable retention properly. What was missing is the ability to
// ANSWER A QUESTION about recent history from inside the process, and a ring is
// the cheapest honest form of that.
//
// Two consequences the caller must carry, rather than discover:
//
//   - It does not survive a restart. Same trade-off as the access-decision
//     journal, and it is why `Stats` reports the boot id and the oldest record —
//     a window with nothing behind it must never be presented as "nothing
//     happened".
//   - It is bounded, so it forgets. `Stats.Dropped` counts what has been
//     overwritten so a reader can say so out loud instead of silently showing a
//     truncated story.

const (
	// DefaultLogRingCapacity is roughly an hour of a busy kernel, and a few tens
	// of megabytes at the value caps below.
	DefaultLogRingCapacity = 50_000

	// maxMessageBytes and maxValueBytes keep a count-bounded ring also bounded in
	// BYTES. Without them one log line carrying a large payload makes the memory
	// ceiling a fiction.
	maxMessageBytes = 8 << 10
	maxValueBytes   = 4 << 10
)

// LogRecord is one retained line.
//
// Attributes stay TYPED and separate from the message. Pre-formatting them into
// a string is what forces every consumer back to regex, and it is the one thing
// that would make this ring not worth keeping.
type LogRecord struct {
	Seq   uint64     `json:"seq"`
	At    time.Time  `json:"at"`
	Level slog.Level `json:"level"`
	// Component is the subsystem: the `logger` attribute when the line came from
	// an agent, otherwise the message's own prefix ("ADR-0074", "SEC-01").
	Component string         `json:"component"`
	Message   string         `json:"message"`
	Attrs     map[string]any `json:"attrs,omitempty"`
	BootID    string         `json:"boot_id"`
	// Truncated marks a record whose message or an attribute was clipped to keep
	// the ring's memory ceiling real. Said out loud so a reader never mistakes a
	// clipped value for the whole one.
	Truncated bool `json:"truncated,omitempty"`
}

// LogRingStats describes the window, so a reader can state its limits.
type LogRingStats struct {
	BootID   string `json:"boot_id"`
	Capacity int    `json:"capacity"`
	Count    int    `json:"count"`
	// Dropped counts records overwritten since boot. Non-zero means the window
	// no longer reaches back to process start.
	Dropped uint64    `json:"dropped"`
	Oldest  time.Time `json:"oldest,omitempty"`
	Newest  time.Time `json:"newest,omitempty"`
	// LastSeq is the highest sequence issued — the resume cursor.
	LastSeq uint64 `json:"last_seq"`
}

// defaultRing is the window installed by the last InitLogger call.
//
// A package-level accessor mirroring slog.Default(): there is exactly one logger
// per process, so there is exactly one retention window, and threading it from
// where the logger is built to where a read surface is built would otherwise
// mean plumbing it through every constructor in between.
var (
	defaultRingMu sync.RWMutex
	defaultRing   *LogRing
)

// SetDefaultLogRing records the process-wide window. Called by InitLogger.
func SetDefaultLogRing(r *LogRing) {
	defaultRingMu.Lock()
	defer defaultRingMu.Unlock()
	defaultRing = r
}

// DefaultLogRing returns the process-wide window, or nil when no logger has been
// initialised — a nil ring means "no retention", never an empty one.
func DefaultLogRing() *LogRing {
	defaultRingMu.RLock()
	defer defaultRingMu.RUnlock()
	return defaultRing
}

// LogRing is a fixed-capacity, concurrency-safe window over recent log records.
type LogRing struct {
	mu      sync.RWMutex
	buf     []LogRecord
	next    int // write cursor
	count   int
	seq     uint64
	dropped uint64
	bootID  string
}

// NewLogRing builds a ring. A capacity of zero or less uses the default.
func NewLogRing(capacity int) *LogRing {
	if capacity <= 0 {
		capacity = DefaultLogRingCapacity
	}
	return &LogRing{buf: make([]LogRecord, capacity), bootID: newBootID()}
}

func newBootID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// A boot id only has to be distinct from the neighbouring boots, so a
		// clock fallback is sufficient and must not take the process down.
		return time.Now().UTC().Format("20060102150405.000")
	}
	return hex.EncodeToString(b)
}

// BootID identifies this process run. It changes on every start, which is what
// lets a reader tell "before the restart" from "after".
func (r *LogRing) BootID() string { return r.bootID }

// Append stores a record, assigning its sequence. Overwrites the oldest entry
// when full and counts that as a drop.
func (r *LogRing) Append(rec LogRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seq++
	rec.Seq = r.seq
	rec.BootID = r.bootID

	if r.count == len(r.buf) {
		r.dropped++
	} else {
		r.count++
	}
	r.buf[r.next] = rec
	r.next = (r.next + 1) % len(r.buf)
}

// Since returns records with a sequence strictly greater than afterSeq, oldest
// first, capped at limit (0 = no cap).
//
// This is the resume primitive: a reader that disconnects reconnects with the
// last sequence it saw. If that sequence has already been overwritten the reader
// gets what remains — and `Stats.Dropped` is how it learns there was a gap,
// rather than silently believing it saw everything.
func (r *LogRing) Since(afterSeq uint64, limit int) []LogRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]LogRecord, 0, min(r.count, orAll(limit, r.count)))
	// Walk oldest → newest.
	start := (r.next - r.count + len(r.buf)) % len(r.buf)
	for i := 0; i < r.count; i++ {
		rec := r.buf[(start+i)%len(r.buf)]
		if rec.Seq <= afterSeq {
			continue
		}
		out = append(out, rec)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// Stats reports the window's shape.
func (r *LogRing) Stats() LogRingStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	st := LogRingStats{
		BootID:   r.bootID,
		Capacity: len(r.buf),
		Count:    r.count,
		Dropped:  r.dropped,
		LastSeq:  r.seq,
	}
	if r.count > 0 {
		start := (r.next - r.count + len(r.buf)) % len(r.buf)
		st.Oldest = r.buf[start].At
		st.Newest = r.buf[(r.next-1+len(r.buf))%len(r.buf)].At
	}
	return st
}

func orAll(limit, count int) int {
	if limit <= 0 {
		return count
	}
	return limit
}

// ── the handler ─────────────────────────────────────────────────────────────

// ringHandler tees every record into the ring and passes it to the real handler.
//
// A handler decorator rather than a call-site change: it captures everything
// already being logged, including the agent output relayed through forwardPipe,
// without a single existing log statement moving.
type ringHandler struct {
	inner slog.Handler
	ring  *LogRing
	// attrs and groups accumulate through WithAttrs/WithGroup so a record logged
	// on a derived logger reaches the ring with the same attributes the real
	// handler will see. Dropping them here is the classic decorator bug: the
	// stdout line is complete and the retained copy quietly is not.
	attrs  []slog.Attr
	groups []string
	// floor is the ring's own level, independent of the inner handler's. The ring
	// should be able to retain debug detail a terminal is not printing.
	floor slog.Level
}

// NewLogRingHandler wraps inner so every record it sees is also retained.
func NewLogRingHandler(inner slog.Handler, ring *LogRing, floor slog.Level) slog.Handler {
	return &ringHandler{inner: inner, ring: ring, floor: floor}
}

// Enabled is true when EITHER the ring or the real handler wants the record, so
// the ring can retain below the printing threshold. Handle re-checks the inner
// handler before forwarding, so this never makes stdout noisier.
func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.floor || h.inner.Enabled(ctx, l)
}

func (h *ringHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= h.floor {
		h.ring.Append(h.build(r))
	}
	if !h.inner.Enabled(ctx, r.Level) {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func (h *ringHandler) build(r slog.Record) LogRecord {
	attrs := make(map[string]any, r.NumAttrs()+len(h.attrs))
	truncated := false

	put := func(key string, v slog.Value) {
		val, clipped := clipValue(v)
		truncated = truncated || clipped
		attrs[key] = val
	}
	// Already qualified when they were captured — a group only applies to
	// attributes added AFTER it was opened, so qualifying them here would
	// retroactively move attrs into a group they were never in.
	for _, a := range h.attrs {
		put(a.Key, a.Value)
	}
	// The record's own attrs belong to whatever group is open now.
	prefix := ""
	if len(h.groups) > 0 {
		prefix = strings.Join(h.groups, ".") + "."
	}
	r.Attrs(func(a slog.Attr) bool {
		put(prefix+a.Key, a.Value)
		return true
	})

	msg := r.Message
	if len(msg) > maxMessageBytes {
		msg = msg[:maxMessageBytes]
		truncated = true
	}

	at := r.Time
	if at.IsZero() {
		at = time.Now()
	}
	return LogRecord{
		At:        at,
		Level:     r.Level,
		Component: componentOf(msg, attrs),
		Message:   msg,
		Attrs:     attrs,
		Truncated: truncated,
	}
}

// clipValue resolves an attribute and bounds it, so a count-bounded ring is also
// bounded in bytes.
func clipValue(v slog.Value) (any, bool) {
	v = v.Resolve()
	if v.Kind() == slog.KindString {
		s := v.String()
		if len(s) > maxValueBytes {
			return s[:maxValueBytes], true
		}
		return s, false
	}
	// Anything else keeps its type — the whole point of retaining attrs is that a
	// reader can compare numbers as numbers.
	return v.Any(), false
}

// componentOf names the subsystem a record belongs to.
//
// The `logger` attribute wins: agent lines carry it, and it is the accurate
// name. Otherwise the message's own prefix is used — this codebase writes
// "ADR-0074: …", "SEC-01: …", "contract 0077: …" consistently, and that prefix
// is exactly the grouping an operator wants.
func componentOf(msg string, attrs map[string]any) string {
	if s, ok := attrs["logger"].(string); ok && s != "" {
		return s
	}
	if i := strings.IndexByte(msg, ':'); i > 0 && i <= 32 {
		if prefix := strings.TrimSpace(msg[:i]); looksLikeComponent(prefix) {
			return prefix
		}
	}
	return "kernel"
}

// looksLikeComponent separates a subsystem label from the first word of a
// sentence that merely contains a colon.
//
// This codebase labels lines as "ADR-0074:", "SEC-01:", "contract 0077:",
// "telegram:" — either carrying a digit, or entirely lower-case. Ordinary prose
// that happens to start with a colon-terminated word is capitalised and has no
// digit: "Warning: the file could not be read". Treating that as a component
// would invent a subsystem called Warning and scatter real lines across it.
func looksLikeComponent(prefix string) bool {
	if prefix == "" || len(strings.Fields(prefix)) > 3 {
		return false
	}
	if strings.ContainsAny(prefix, "0123456789") {
		return true
	}
	return prefix == strings.ToLower(prefix)
}

func (h *ringHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	next := *h
	next.inner = h.inner.WithAttrs(as)
	// Qualify with the group path in force RIGHT NOW, then store. This is what
	// makes `log.With("a", 1).WithGroup("g").Info(..., "b", 2)` retain `a` and
	// `g.b`, which is what the printed line says too.
	prefix := ""
	if len(h.groups) > 0 {
		prefix = strings.Join(h.groups, ".") + "."
	}
	qualified := make([]slog.Attr, 0, len(as))
	for _, a := range as {
		qualified = append(qualified, slog.Attr{Key: prefix + a.Key, Value: a.Value})
	}
	next.attrs = append(append([]slog.Attr(nil), h.attrs...), qualified...)
	return &next
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.inner = h.inner.WithGroup(name)
	next.groups = append(append([]string(nil), h.groups...), name)
	return &next
}
