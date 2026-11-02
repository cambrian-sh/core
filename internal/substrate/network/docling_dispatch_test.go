package network

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
)

type doclingCaptureCaller struct{ sent []byte }

func (c *doclingCaptureCaller) CallAgent(_ context.Context, _ string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	c.sent = h.Payload.Data
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(`{"backend":"text","ok":true,"nodes":[{"id":"chk:n0","kind":"paragraph","text":"| a | 1 |","meta":{"junk_reason":"low_letter_ratio"}}]}`)}}, nil
}

// junk_mode must reach the sidecar, and the junk_reason tag must come back, or the
// kernel-side filter (memory.applyStructureJunkFilter) can neither keep chat
// content nor audit a drop (T-20261102-ingestion-structure-junk-filter-silently-drops).
func TestDoclingDispatcher_JunkModeRoundTrip(t *testing.T) {
	c := &doclingCaptureCaller{}
	sd, err := (&DoclingDispatcher{Caller: c}).Parse(context.Background(), memory.StructureParseRequest{
		DocID: "d", Text: "| a | 1 |", JunkMode: "mark",
	})
	if err != nil || sd == nil {
		t.Fatalf("Parse: %v %v", sd, err)
	}
	var req map[string]any
	if err := json.Unmarshal(c.sent, &req); err != nil {
		t.Fatalf("request JSON: %v", err)
	}
	if req["junk_mode"] != "mark" {
		t.Fatalf("junk_mode not sent: %s", c.sent)
	}
	if got, _ := sd.Nodes[0].Meta["junk_reason"].(string); got != "low_letter_ratio" {
		t.Fatalf("junk_reason not decoded: %+v", sd.Nodes[0].Meta)
	}
}
