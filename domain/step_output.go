package domain

// The live step-output stream (ADR-0139 amendment 2026-11-06; it supersedes
// ADR-0047 D12's token-level tier).
//
// One stream per step, keyed by the step's lease, decoupled from "the model
// tokens the gateway sees". Three producers feed it:
//
//   - the gateway (StepOutputFromGateway): text deltas and the incrementally
//     extracted final-answer argument of a managed model call, on both the text
//     lane (GenerateViaModelStream) and the native tool lane (GenerateWithTools,
//     which streams from the provider internally while staying unary for the SDK);
//   - the agent itself (StepOutputFromAgent): an external agent that calls its own
//     model pushes its output over the agent-plane EmitStepOutput RPC;
//   - A2A streaming (deferred: the kernel speaks no A2A 1.0 task protocol yet).
//
// Its outward channels are the operator feed (TokenChunkEvent, live-only) and the
// MCP endpoint's notifications/progress. Everything here is best-effort and
// live-only: the step's FINAL output is what the step-level events guarantee.

// StepOutputKind classifies one chunk.
type StepOutputKind string

const (
	// StepOutputText is a fragment of the step's output text.
	StepOutputText StepOutputKind = "text"
	// StepOutputProgress is a progress note ("reading 3 of 7 files").
	StepOutputProgress StepOutputKind = "progress"
	// StepOutputStatus is a stream status note: a restart after a model failover,
	// a truncation at the per-step cap.
	StepOutputStatus StepOutputKind = "status"
)

// Valid reports whether k is one of the declared kinds.
func (k StepOutputKind) Valid() bool {
	switch k {
	case StepOutputText, StepOutputProgress, StepOutputStatus:
		return true
	}
	return false
}

// StepOutputSource names which producer emitted a chunk.
type StepOutputSource string

const (
	// StepOutputFromGateway: forwarded by the kernel from a managed model call.
	StepOutputFromGateway StepOutputSource = "gateway"
	// StepOutputFromAgent: pushed by the agent (EmitStepOutput).
	StepOutputFromAgent StepOutputSource = "agent"
)

// StepOutputChunk is one chunk of a step's live output, as the stream delivers
// it to its channels. Seq is the KERNEL's per-lease sequence (1, 2, ...) across
// both producers — an agent's own seq is used only to drop its duplicates.
type StepOutputChunk struct {
	LeaseID   LeaseID
	SessionID SessionID // the task session the lease was issued under; "" when none
	StepIndex int
	AgentID   string
	Seq       uint64
	Kind      StepOutputKind
	Data      string
	// Final marks the producer's last chunk for the step (agent-declared).
	Final  bool
	Source StepOutputSource
}
