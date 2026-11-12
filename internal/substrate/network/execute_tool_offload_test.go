package network

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// An offloaded result reaches the agent as its summary beside result_cid — never a
// bare id (T-20261112-worker-resolve-cid-inline-has-no-size-cap-a) — while an
// inline result is passed through byte for byte.
func TestWireResultJSON_OffloadedCarriesSummary(t *testing.T) {
	s := domain.NewOffloadSummary("led_x", "mcp:docs/read", []byte(strings.Repeat("abc ", 5000)), 50, 16000)
	got := wireResultJSON(domain.ToolCallResponse{ResultCID: "led_x", Offloaded: &s})
	if !strings.HasPrefix(got, `{"_offloaded":{"ref":"led_x"`) || !strings.Contains(got, `"size_bytes":20000`) {
		t.Fatalf("wire = %s", got)
	}
	if got := wireResultJSON(domain.ToolCallResponse{ResultJSON: []byte(`{"a":1}`), ResultCID: "led_y"}); got != `{"a":1}` {
		t.Fatalf("inline = %s", got)
	}
}
