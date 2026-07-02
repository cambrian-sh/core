package domain

import (
	"errors"
	"fmt"
	"testing"
)

// Cycle 1: SemanticCheckpointError.Error() returns the expected formatted string.
func TestSemanticCheckpointError_ErrorString(t *testing.T) {
	err := &SemanticCheckpointError{
		StepIndex:  3,
		Assessment: "output is off-topic",
	}
	want := "semantic checkpoint failed at step 3: output is off-topic"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// Cycle 2: errors.As correctly unwraps a wrapped SemanticCheckpointError and
// preserves all three fields on the extracted value.
func TestSemanticCheckpointError_ErrorsAs_UnwrapsAndPreservesFields(t *testing.T) {
	original := &SemanticCheckpointError{
		StepIndex:      5,
		Assessment:     "wrong format REPLAN_SIGNAL",
		OriginalResult: "some bad output",
	}
	wrapped := fmt.Errorf("coordinator: %w", original)

	var target *SemanticCheckpointError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As did not match SemanticCheckpointError")
	}
	if target.StepIndex != 5 {
		t.Errorf("StepIndex: want 5, got %d", target.StepIndex)
	}
	if target.Assessment != "wrong format REPLAN_SIGNAL" {
		t.Errorf("Assessment: want %q, got %q", "wrong format REPLAN_SIGNAL", target.Assessment)
	}
	if target.OriginalResult != "some bad output" {
		t.Errorf("OriginalResult: want %q, got %q", "some bad output", target.OriginalResult)
	}
}
