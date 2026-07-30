package util

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func ringLogger(t *testing.T, capacity int, floor slog.Level) (*slog.Logger, *LogRing) {
	t.Helper()
	ring := NewLogRing(capacity)
	inner := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(NewLogRingHandler(inner, ring, floor)), ring
}

func TestLogRing_RetainsTypedAttributes(t *testing.T) {
	log, ring := ringLogger(t, 10, slog.LevelDebug)
	log.Info("planner_step_generated", "index", 3, "is_thought", true, "agent", "planner")

	recs := ring.Since(0, 0)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	// Typed, and separate from the message. Flattening these into a string is
	// what forces every consumer back to regex.
	if recs[0].Attrs["index"] != int64(3) {
		t.Fatalf("index lost its type: %#v", recs[0].Attrs["index"])
	}
	if recs[0].Attrs["is_thought"] != true {
		t.Fatalf("bool lost: %#v", recs[0].Attrs["is_thought"])
	}
	if recs[0].Message != "planner_step_generated" {
		t.Fatalf("message mangled: %q", recs[0].Message)
	}
}

// The classic decorator bug: the printed line carries the attributes and the
// retained copy quietly does not.
func TestLogRing_KeepsWithAttrsAndWithGroup(t *testing.T) {
	log, ring := ringLogger(t, 10, slog.LevelDebug)
	log.With("agent_id", "retriever-2").WithGroup("http").Info("request rejected", "status", 504)

	rec := ring.Since(0, 0)[0]
	if rec.Attrs["agent_id"] != "retriever-2" {
		t.Fatalf("WithAttrs lost: %#v", rec.Attrs)
	}
	// Grouped keys are qualified, so two groups cannot collide on `status`.
	if rec.Attrs["http.status"] != int64(504) {
		t.Fatalf("WithGroup not applied: %#v", rec.Attrs)
	}
}

// Overwriting is expected; doing it silently is not. A reader has to be able to
// say the window no longer reaches back.
func TestLogRing_CountsWhatItDropped(t *testing.T) {
	log, ring := ringLogger(t, 4, slog.LevelDebug)
	for i := 0; i < 10; i++ {
		log.Info("line")
	}

	st := ring.Stats()
	if st.Count != 4 {
		t.Fatalf("want 4 retained, got %d", st.Count)
	}
	if st.Dropped != 6 {
		t.Fatalf("want 6 dropped, got %d", st.Dropped)
	}
	if st.LastSeq != 10 {
		t.Fatalf("sequence must keep counting past the capacity, got %d", st.LastSeq)
	}
	// What remains is the NEWEST, in order.
	recs := ring.Since(0, 0)
	if recs[0].Seq != 7 || recs[3].Seq != 10 {
		t.Fatalf("wrong window retained: %d..%d", recs[0].Seq, recs[3].Seq)
	}
}

// The resume primitive: reconnect with the last sequence seen.
func TestLogRing_SinceIsTheResumeCursor(t *testing.T) {
	log, ring := ringLogger(t, 100, slog.LevelDebug)
	for i := 0; i < 5; i++ {
		log.Info("line")
	}

	got := ring.Since(3, 0)
	if len(got) != 2 || got[0].Seq != 4 || got[1].Seq != 5 {
		t.Fatalf("want seq 4 and 5, got %+v", got)
	}
	if len(ring.Since(5, 0)) != 0 {
		t.Fatal("a caller already at the head should get nothing")
	}
	if n := len(ring.Since(0, 2)); n != 2 {
		t.Fatalf("limit ignored: got %d", n)
	}
}

// The ring keeps detail the terminal is not printing — that is the point of a
// separate floor.
func TestLogRing_RetainsBelowTheHandlersLevel(t *testing.T) {
	log, ring := ringLogger(t, 10, slog.LevelDebug) // inner handler is Info
	log.Debug("offset advanced", "offset", 12)

	if got := len(ring.Since(0, 0)); got != 1 {
		t.Fatalf("debug record not retained: %d", got)
	}
}

// A count-bounded ring must also be bounded in bytes, or one large payload makes
// the memory ceiling a fiction.
func TestLogRing_ClipsOversizedValuesAndSaysSo(t *testing.T) {
	log, ring := ringLogger(t, 10, slog.LevelDebug)
	log.Info("ingest", "body", strings.Repeat("x", maxValueBytes*3))

	rec := ring.Since(0, 0)[0]
	if len(rec.Attrs["body"].(string)) != maxValueBytes {
		t.Fatalf("value not clipped: %d", len(rec.Attrs["body"].(string)))
	}
	if !rec.Truncated {
		t.Fatal("clipping was silent; a reader would mistake it for the whole value")
	}
}

func TestLogRing_ComponentFromLoggerThenPrefix(t *testing.T) {
	log, ring := ringLogger(t, 10, slog.LevelDebug)
	// An agent line: `logger` is the accurate name and wins.
	log.Info("polling from update_id 0", "logger", "cambrian.daemon")
	// A kernel line: the message's own prefix is the grouping operators use.
	log.Info("ADR-0074: plugin registered", "plugin", "authz")
	// Prose containing a colon is NOT a component.
	log.Info("Warning: the file could not be read and will be skipped")

	recs := ring.Since(0, 0)
	if recs[0].Component != "cambrian.daemon" {
		t.Fatalf("logger should win: %q", recs[0].Component)
	}
	if recs[1].Component != "ADR-0074" {
		t.Fatalf("prefix not used: %q", recs[1].Component)
	}
	if recs[2].Component != "kernel" {
		t.Fatalf("prose misread as a component: %q", recs[2].Component)
	}
}

// A boot id is what lets a reader tell "before the restart" from "after".
func TestLogRing_StampsABootID(t *testing.T) {
	log, ring := ringLogger(t, 10, slog.LevelDebug)
	log.Info("up")

	if ring.BootID() == "" {
		t.Fatal("no boot id")
	}
	if ring.Since(0, 0)[0].BootID != ring.BootID() {
		t.Fatal("record not stamped with the boot id")
	}
	if NewLogRing(4).BootID() == ring.BootID() {
		t.Fatal("two rings share a boot id; a restart would be invisible")
	}
}

// slog handlers are called concurrently. A ring that corrupts under load would
// be worse than no ring.
func TestLogRing_IsConcurrencySafe(t *testing.T) {
	log, ring := ringLogger(t, 512, slog.LevelDebug)

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				log.Info("concurrent", "worker", w)
			}
		}(w)
	}
	// Read while writing.
	go func() {
		for i := 0; i < 200; i++ {
			_ = ring.Stats()
			_ = ring.Since(0, 16)
		}
	}()
	wg.Wait()

	if st := ring.Stats(); st.LastSeq != 1600 {
		t.Fatalf("lost or double-counted records: seq=%d", st.LastSeq)
	}
}

// The decorator must not change what stdout sees.
func TestLogRing_DoesNotChangeWhatTheHandlerPrints(t *testing.T) {
	var buf strings.Builder
	ring := NewLogRing(10)
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	log := slog.New(NewLogRingHandler(inner, ring, slog.LevelDebug))

	log.Debug("not printed")
	log.Info("printed")

	if strings.Contains(buf.String(), "not printed") {
		t.Fatal("the ring made stdout noisier than the handler's own level")
	}
	if !strings.Contains(buf.String(), "printed") {
		t.Fatal("the ring swallowed a record the handler wanted")
	}
	if got := len(ring.Since(0, 0)); got != 2 {
		t.Fatalf("ring should hold both, got %d", got)
	}
}

func TestLogRing_EmptyWindowIsHonest(t *testing.T) {
	ring := NewLogRing(8)
	st := ring.Stats()
	if st.Count != 0 || st.Dropped != 0 || st.LastSeq != 0 {
		t.Fatalf("fresh ring reports content: %+v", st)
	}
	if !st.Oldest.IsZero() {
		t.Fatal("an empty window must not claim a start time")
	}
	if len(ring.Since(0, 0)) != 0 {
		t.Fatal("empty ring returned records")
	}
	_ = context.Background()
}
