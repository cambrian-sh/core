package domain

import (
	"context"
	"strings"
)

// CheckpointRequest is the input to the H1 coherence gate (ADR-0013): a step's
// output assessed against its original intent.
type CheckpointRequest struct {
	StepQuery  string
	StepOutput string
	Question   string
}

// CheckpointVerdict is the structured result of a coherence validation. Coherent
// drives the gate; Assessment is the validator's brief reason (for telemetry).
type CheckpointVerdict struct {
	Coherent   bool
	Assessment string
}

// CheckpointValidator assesses whether a step's output satisfies its intent
// (ADR-0013 H1 gate). It is a dedicated validator with a structured verdict
// contract — not the synthesis ThoughtFn parsed by a substring search. An
// incoherent verdict triggers replanning before dependent steps dispatch.
type CheckpointValidator interface {
	Validate(ctx context.Context, req CheckpointRequest) (CheckpointVerdict, error)
}

// ParseCheckpointVerdict deterministically extracts a verdict from a validator's
// raw response. It reads the explicit "VERDICT:" line only, so the model echoing
// the step output (which may itself contain the word "incoherent") cannot flip
// the result. A missing or unparseable verdict is treated as coherent
// (fail-open) — a parse miss must never fabricate a replan.
func ParseCheckpointVerdict(raw string) CheckpointVerdict {
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)
		if !strings.HasPrefix(upper, "VERDICT:") {
			continue
		}
		// "INCOHERENT" contains "COHERENT", so test it first.
		if strings.Contains(upper, "INCOHERENT") {
			return CheckpointVerdict{Coherent: false, Assessment: trimmed}
		}
		if strings.Contains(upper, "COHERENT") {
			return CheckpointVerdict{Coherent: true, Assessment: trimmed}
		}
	}
	return CheckpointVerdict{Coherent: true, Assessment: strings.TrimSpace(raw)}
}
