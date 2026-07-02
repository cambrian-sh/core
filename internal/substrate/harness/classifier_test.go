package harness

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
)

// --- Cycle 1: Rule 1 — DeadlineExceeded → SystemError (tracer bullet) ---

func TestClassify_DeadlineExceeded_IsSystemError(t *testing.T) {
	result := Classify(errors.New("context deadline exceeded"), codes.DeadlineExceeded, "")
	if result != SystemError {
		t.Errorf("expected SystemError for DeadlineExceeded, got %v", result)
	}
}

// --- Cycle 2: Rule 1 — Unavailable → SystemError ---

func TestClassify_Unavailable_IsSystemError(t *testing.T) {
	result := Classify(errors.New("service unavailable"), codes.Unavailable, "")
	if result != SystemError {
		t.Errorf("expected SystemError for Unavailable, got %v", result)
	}
}

// --- Cycle 3: Rule 4 — default → LogicError ---

func TestClassify_OtherGRPCCode_IsLogicError(t *testing.T) {
	result := Classify(errors.New("agent returned bad output"), codes.Internal, "")
	if result != LogicError {
		t.Errorf("expected LogicError for codes.Internal with no special signal, got %v", result)
	}
}

// --- Cycle 4: Rule 2 — NO_WINNER in error message → SystemError ---

func TestClassify_NoWinnerMessage_IsSystemError(t *testing.T) {
	result := Classify(errors.New("auction failed: NO_WINNER for step 3"), codes.Internal, "")
	if result != SystemError {
		t.Errorf("expected SystemError for NO_WINNER error message, got %v", result)
	}
}

// --- Cycle 5: Rule 3 — agent self-report via agentErrorType → SystemError ---

func TestClassify_AgentSelfReportSystem_IsSystemError(t *testing.T) {
	result := Classify(errors.New("disk full"), codes.Internal, "system")
	if result != SystemError {
		t.Errorf("expected SystemError for agent self-report _error_type=system, got %v", result)
	}
}

// --- Cycle 6: Rule priority — Rule 1 wins over Rule 4 ---

func TestClassify_Rule1BeatsRule4_DeadlineWithNoSignal(t *testing.T) {
	// Rule 4 would apply (no NO_WINNER, no agent self-report), but Rule 1 fires first.
	result := Classify(errors.New("some timeout"), codes.DeadlineExceeded, "")
	if result != SystemError {
		t.Errorf("expected Rule 1 (DeadlineExceeded) to take priority over Rule 4 default, got %v", result)
	}
}

// --- Cycle 7: Rule priority — Rule 3 wins over Rule 4 ---

func TestClassify_Rule3BeatsRule4_AgentSelfReportWithInternalCode(t *testing.T) {
	// codes.Internal would normally be Rule 4 (LogicError), but Rule 3 overrides it.
	result := Classify(errors.New("network unreachable"), codes.Internal, "system")
	if result != SystemError {
		t.Errorf("expected Rule 3 (agent self-report) to override codes.Internal default, got %v", result)
	}
}

// --- Cycle 8: Rule 2 beats Rule 4 — NO_WINNER on codes.Internal ---

func TestClassify_Rule2BeatsRule4_NoWinnerWithInternalCode(t *testing.T) {
	result := Classify(errors.New("NO_WINNER: no candidates passed declaration filter"), codes.Internal, "")
	if result != SystemError {
		t.Errorf("expected Rule 2 (NO_WINNER message) to override codes.Internal default, got %v", result)
	}
}

// --- Cycle 9: agentErrorType values other than "system" fall through to Rule 4 ---

func TestClassify_NonSystemAgentErrorType_IsLogicError(t *testing.T) {
	result := Classify(errors.New("bad output"), codes.Internal, "logic")
	if result != LogicError {
		t.Errorf("expected LogicError when agentErrorType is not 'system', got %v", result)
	}
}
