package network

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
)

// QueryMemory's INFO line names the caller and fingerprints the query; the
// person's text itself is DEBUG only
// (T-20261103-kernel-logs-print-raw-query-text-and-leaf).
//
// Not parallel: it swaps the default logger.
func TestQueryMemory_InfoLogCarriesNoQueryText(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := makeQueryMemoryServer(&mockMemorySearcher{})
	if _, err := srv.QueryMemory(ctxWithAgentID("agent-a"), &pb.MemoryRequest{Query: "Mehmet Yılmaz salary 2026", TopK: 3}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "QueryMemory called") || !strings.Contains(out, "query_sha256=") {
		t.Fatalf("no fingerprinted QueryMemory line:\n%s", out)
	}
	if strings.Contains(out, "Mehmet") || strings.Contains(out, "salary") {
		t.Fatalf("an INFO line carries the query text:\n%s", out)
	}
}
