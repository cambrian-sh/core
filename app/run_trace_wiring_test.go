package app

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

func TestWireRunTrace(t *testing.T) {
	if wireRunTrace(config.TraceConfig{}) != nil {
		t.Fatal("off must wire no tracer")
	}
	dir := filepath.Join(t.TempDir(), "traces")
	tr := wireRunTrace(config.TraceConfig{Enabled: true, Dir: dir})
	if tr == nil {
		t.Fatal("on must wire a tracer")
	}
	t.Cleanup(func() { _ = tr.(io.Closer).Close() }) // Windows cannot remove an open file
	tr.Span(t.Context(), domain.TraceSpan{Name: "chat m"})
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("trace files = %d, want 1", len(files))
	}
}
