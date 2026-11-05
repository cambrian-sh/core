package harness

import (
	"strings"

	"google.golang.org/grpc/codes"
)

// FaultClass distinguishes the source of a step execution failure.
// SystemError indicates infrastructure failure (no Merit impact).
// LogicError indicates agent fault (Verifier scores final output).
type FaultClass int

const (
	LogicError  FaultClass = iota
	SystemError FaultClass = iota
)

// Classify applies four rules in priority order to determine fault ownership.
//
// Rule 1: gRPC DeadlineExceeded or Unavailable, or a context deadline
//
//	anywhere in the chain                 → SystemError (infrastructure)
//
// Rule 2: error message contains "NO_WINNER"   → SystemError (no eligible agent)
// Rule 3: agentErrorType == "system"           → SystemError (agent self-report)
// Rule 4: all other cases                      → LogicError  (agent fault)
func Classify(err error, code codes.Code, agentErrorType string) FaultClass {
	// A context deadline that never became a gRPC status (the dispatcher's
	// "step timed out" wraps context.DeadlineExceeded) is the same infrastructure
	// fault: without this it read as codes.Unknown and was blamed on the agent.
	if code == codes.DeadlineExceeded || code == codes.Unavailable || isTimeout(err, code) {
		return SystemError
	}
	if err != nil && strings.Contains(err.Error(), "NO_WINNER") {
		return SystemError
	}
	if agentErrorType == "system" {
		return SystemError
	}
	return LogicError
}
