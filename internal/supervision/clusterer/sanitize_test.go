package clusterer

import "testing"

// A cluster label must never be a model/serving error blob or multi-line junk —
// it is injected verbatim into the Planner prompt. sanitizeClusterName accepts a
// clean short label and rejects everything else (caller substitutes a fallback).
func TestSanitizeClusterName(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string // "" means rejected
	}{
		{"clean label", "code_generation", "code_generation"},
		{"trims whitespace", "  data_analysis \n", "data_analysis"},
		{"takes first line", "summarization\n(reasoning)", "summarization"},
		{"rejects error JSON blob", `{"error": "invalid_outputschema_format", "message": "...Received: 'string'."}`, ""},
		{"rejects any braces", "label {x}", ""},
		{"rejects the word error", "naming error occurred", ""},
		{"rejects over-long", "this is a far too long capability label that is clearly not 1 to 3 words", ""},
		{"rejects empty", "   ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeClusterName(tt.raw); got != tt.want {
				t.Errorf("sanitizeClusterName(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
