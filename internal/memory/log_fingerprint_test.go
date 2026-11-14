package memory

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A person's recall query never reaches an INFO line as text
// (T-20261103-kernel-logs-print-raw-query-text-and-leaf): the SLOW pass-timing
// line carries its length and fingerprint; the raw text is DEBUG only.
//
// Not parallel: it swaps the default logger.
func TestPassTimer_SlowLineCarriesNoQueryText(t *testing.T) {
	const secret = "what is the diagnosis of Ayşe Kaya"
	for _, tc := range []struct {
		name        string
		level       slog.Level
		wantPrinted string
	}{
		{"info", slog.LevelInfo, "memory pass timing (SLOW)"},
		{"debug handler", slog.LevelDebug, "memory pass timing (SLOW)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tc.level})))
			t.Cleanup(func() { slog.SetDefault(prev) })
			pt := &passTimer{started: time.Now().Add(-2 * slowPassThreshold), last: time.Now()}
			pt.log(context.Background(), secret)
			out := buf.String()
			if !strings.Contains(out, tc.wantPrinted) {
				t.Fatalf("no SLOW line:\n%s", out)
			}
			if strings.Contains(out, "Ayşe") || strings.Contains(out, "diagnosis") {
				t.Fatalf("the INFO SLOW line carries the query text:\n%s", out)
			}
			if !strings.Contains(out, "query_sha256=") || !strings.Contains(out, "query_len=34") {
				t.Fatalf("the SLOW line lacks the fingerprint:\n%s", out)
			}
		})
	}
}

func TestTextLogAttrs_FingerprintIsStableAndShort(t *testing.T) {
	a := TextLogAttrs("query", "hello")
	b := TextLogAttrs("query", "hello")
	if len(a) != 4 || a[0] != "query_len" || a[1] != 5 || a[2] != "query_sha256" || a[3] != b[3] || len(a[3].(string)) != 12 {
		t.Fatalf("attrs = %v", a)
	}
	if a[3] == TextLogAttrs("query", "hello!")[3] {
		t.Fatal("different text, same fingerprint")
	}
}
