package domain

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPlanActionIDs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		result string
		want   []string
	}{
		{"structured top level", `{"key":"OPS-11","issue":{"key":"OPS-11"},"write_id":"W-7"}`, []string{"key=OPS-11", "write_id=W-7"}},
		{"mcp content array", `[{"type":"text","text":"{\"message_id\":\"M-9\",\"status\":\"sent\"}"}]`, []string{"message_id=M-9"}},
		{"nested only", `{"issue":{"key":"OPS-3","summary":"x"},"ok":true}`, []string{"issue.key=OPS-3"}},
		{"numeric id", `{"id":42}`, []string{"id=42"}},
		{"wrapper", `{"result":{"id":"evt-1"}}`, []string{"id=evt-1"}},
		{"no ids", `{"ok":true}`, nil},
		{"text content", `[{"type":"text","text":"Issue created"}]`, nil},
		{"not json", `created`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlanActionIDs([]byte(tt.result)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRenderPlanActions(t *testing.T) {
	t.Parallel()
	if RenderPlanActions(nil) != "" {
		t.Fatal("no actions ⇒ empty")
	}
	got := RenderPlanActions([]PlanAction{
		{Step: 0, Tool: "mcp:w/issues_create", Status: PlanActionDone, IDs: []string{"key=OPS-11"}, Args: map[string]string{"summary": "INC-1040\nline two", "project": "OPS"}},
		{Step: 1, Tool: "mcp:w/mail_send", Status: PlanActionRefused, Detail: "approval declined"},
	})
	want := PlanActionsHeader +
		"\n1. step 0 · mcp:w/issues_create · done · ids: key=OPS-11 · args: project=OPS; summary=INC-1040 line two" +
		"\n2. step 1 · mcp:w/mail_send · refused (approval declined)"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// RedactArgs cuts long values by runes: a byte cut split a multi-byte rune and left
// invalid UTF-8, which a proto string field refuses to marshal.
func TestRedactArgs_CutsOnRuneBoundary(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("ğ", 200)
	got := RedactArgs(map[string]any{"body": long})["body"]
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 161 || !strings.HasSuffix(got, "…") {
		t.Errorf("got %d runes, valid=%v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
}
