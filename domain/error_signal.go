package domain

import (
	"encoding/json"
	"strings"
)

// ErrorKind classifies the type of error signal detected in a step output.
type ErrorKind string

const (
	ErrorKindBlocked     ErrorKind = "blocked"
	ErrorKindNonZeroExit ErrorKind = "non_zero_exit"
	ErrorKindStderr      ErrorKind = "stderr"
	ErrorKindException   ErrorKind = "exception"
	ErrorKindGeneric     ErrorKind = "generic"
	// ErrorKindRuntime marks a transport/OS-level infrastructure failure narration
	// — e.g. an agent answer that reports a tool was "unavailable" by quoting the
	// underlying gRPC/OS error. Such text is operational telemetry about the system,
	// never durable knowledge about the task subject, so it is routed to the
	// decaying negative_edge lane (remembered as a DATED failure, not a timeless
	// fact). ADR-0025 (failure-laundering gap).
	ErrorKindRuntime ErrorKind = "runtime"
)

// exceptionSignals are Python/shell exception substrings indicating a failed execution.
var exceptionSignals = []string{"SyntaxError", "ModuleNotFoundError", "Traceback"}

// runtimeFailureSignals are transport/OS-level substrings that mark an
// infrastructure/tool failure being narrated as if it were content. They catch the
// case the raw-output checks miss: an agent final_answer that *paraphrases* a tool
// failure ("the web_search tool is currently unavailable …") while quoting the
// underlying error. Kept deliberately tight to system/runtime signatures that do
// not appear in legitimate domain knowledge, to avoid filtering real facts.
var runtimeFailureSignals = []string{
	"DEADLINE_EXCEEDED",
	"_InactiveRpcError",
	"StatusCode.",
	"No such file or directory",
	"[Errno",
	"cambrian-tool-", // the kernel's per-call tool jail tempdir — never real knowledge
}

// IsErrorOutput detects deterministic error signals in step output text.
// Returns (true, kind) when the text represents a failure; (false, "") for clean output.
// Pure function — no external dependencies. ADR-0025.
func IsErrorOutput(text string) (bool, ErrorKind) {
	switch {
	case strings.HasPrefix(text, "BLOCKED:"):
		return true, ErrorKindBlocked
	case strings.HasPrefix(text, "FAILURE:"):
		return true, ErrorKindGeneric
	case strings.HasPrefix(text, "ERROR:"):
		return true, ErrorKindGeneric
	}
	for _, sig := range exceptionSignals {
		if strings.Contains(text, sig) {
			return true, ErrorKindException
		}
	}
	for _, sig := range runtimeFailureSignals {
		if strings.Contains(text, sig) {
			return true, ErrorKindRuntime
		}
	}
	if kind, ok := checkJSONErrorPayload(text); ok {
		return true, kind
	}
	return false, ""
}

// checkJSONErrorPayload parses JSON tool-agent output and checks exit_code / stderr fields.
func checkJSONErrorPayload(text string) (ErrorKind, bool) {
	var payload struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return "", false
	}
	if payload.ExitCode != 0 {
		return ErrorKindNonZeroExit, true
	}
	if payload.Stderr != "" {
		return ErrorKindStderr, true
	}
	return "", false
}
