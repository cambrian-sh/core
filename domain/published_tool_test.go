package domain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The name grammar is the endpoint's public contract (ADR-0126 D7). It is checked
// at registration, so every rejection here is a boot error rather than a 400 the
// first external client discovers.
func TestValidPublishedToolName(t *testing.T) {
	maxLen := "a" + strings.Repeat("b", 47)  // 48 chars — the limit
	tooLong := "a" + strings.Repeat("b", 48) // 49 chars — one over

	cases := []struct {
		name string
		want bool
	}{
		{"ask_memory", true},
		{"a", true},
		{"search_memory_2", true},
		{maxLen, true},
		{tooLong, false},
		{"Ask_Memory", false},   // upper case
		{"memory.query", false}, // dotted — the ADR-0097 D7 lesson
		{"2fast", false},        // leading digit
		{"_leading", false},     // leading underscore
		{"", false},
		{"ask-memory", false},   // hyphen
		{"ask memory", false},   // space
		{"mcp:cambrian", false}, // namespaced
		{"ask_memory\n", false}, // trailing newline (the anchors are ^…$, not \A…\z)
	}
	for _, c := range cases {
		if got := ValidPublishedToolName(c.name); got != c.want {
			t.Errorf("ValidPublishedToolName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// echoHandler is a minimal PublishedToolHandler: it proves the port is satisfiable
// with nothing but the standard library, which is the whole point of keeping the
// MCP SDK out of domain/.
type echoHandler struct{ ref string }

var _ PublishedToolHandler = echoHandler{}

func (h echoHandler) Invoke(_ context.Context, args json.RawMessage) (PublishedToolResult, error) {
	return PublishedToolResult{Structured: map[string]string{"echo": string(args)}, Text: string(args), ReceiptRef: h.ref}, nil
}

// A result carries both renderings plus the receipt handle; an empty ReceiptRef is
// the OSS default (no receipt lane), not a failure.
func TestPublishedToolResultShape(t *testing.T) {
	res, err := echoHandler{}.Invoke(context.Background(), json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Text != `{"q":"x"}` {
		t.Errorf("Text = %q, want the raw arguments echoed", res.Text)
	}
	if res.Structured == nil {
		t.Error("Structured must survive to the renderer")
	}
	if res.ReceiptRef != "" {
		t.Errorf("ReceiptRef = %q, want empty when no receipt lane is installed", res.ReceiptRef)
	}
}

// The surface is a plain ordered snapshot: declarations, handlers, and the plugin
// that owns each — enough for a renderer to list and dispatch without reaching
// back into the composition root.
func TestPublishedToolSurfaceCarriesOwnerAndHandler(t *testing.T) {
	surface := PublishedToolSurface{
		{Owner: "core", Tool: PublishedTool{Name: "ask_memory", Effects: []ToolEffect{EffectRead}, ReadOnly: true}, Handler: echoHandler{ref: "r1"}},
	}
	if surface[0].Owner != "core" || surface[0].Tool.Name != "ask_memory" {
		t.Fatalf("entry lost its attribution: %+v", surface[0])
	}
	res, err := surface[0].Handler.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil || res.ReceiptRef != "r1" {
		t.Fatalf("handler not reachable from the snapshot: %+v, %v", res, err)
	}
}
