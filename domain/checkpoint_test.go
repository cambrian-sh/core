package domain

import "testing"

// The checkpoint verdict is parsed deterministically from an explicit VERDICT:
// line — robust to the validator echoing the step output, and fail-open so a
// parse miss never fabricates a replan. Regression for the H1 gate that
// previously grepped free-form text for REPLAN_SIGNAL and rubber-stamped.
func TestParseCheckpointVerdict(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantCoherent bool
	}{
		{"explicit coherent", "VERDICT: COHERENT\nThe complexity analysis is correct.", true},
		{"explicit incoherent", "VERDICT: INCOHERENT\nThe base case is missing.", false},
		{"lowercase verdict", "verdict: incoherent — wrong recurrence", false},
		{
			name:         "echoed prose mentioning 'incoherent' does not flip a coherent verdict",
			raw:          "The user worried the output was incoherent, but it is fine.\nVERDICT: COHERENT",
			wantCoherent: true,
		},
		{"no verdict line is fail-open (coherent)", "I think the answer looks reasonable.", true},
		{"empty is fail-open (coherent)", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := ParseCheckpointVerdict(tt.raw)
			if v.Coherent != tt.wantCoherent {
				t.Errorf("Coherent = %v, want %v (raw=%q)", v.Coherent, tt.wantCoherent, tt.raw)
			}
		})
	}
}
