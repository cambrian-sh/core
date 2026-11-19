package runtrace

import (
	"os"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2102: the kernel closes the writer at shutdown; a span arriving after
// that is dropped (no write to a closed file, no "tracing stopped" warning),
// a second Close is a no-op, and the file can be removed once closed.
func TestWriter_SpanAfterCloseIsDropped(t *testing.T) {
	w, err := Open(t.TempDir(), "cambrian-kernel", 0)
	if err != nil {
		t.Fatal(err)
	}
	w.Span(t.Context(), domain.TraceSpan{Name: "before"})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	w.Span(t.Context(), domain.TraceSpan{Name: "after"})
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	lines := readSpans(t, w.Path())
	if len(lines) != 1 || lines[0]["name"] != "before" {
		t.Fatalf("lines = %v, want only the span before Close", lines)
	}
	if err := os.Remove(w.Path()); err != nil {
		t.Fatalf("a closed trace file must be removable: %v", err)
	}
}
