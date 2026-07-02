package domain

import "context"

// DecisionType is the result of classifying an input through the Router.
type DecisionType string

const (
	DecisionChat          DecisionType = "chat"
	DecisionPlan          DecisionType = "plan"
	DecisionIngest        DecisionType = "ingest"
	DecisionWatch         DecisionType = "watch"
	DecisionClarification DecisionType = "clarification"
)

// knownDecisions is the set of valid DecisionType values for Layer 0 validation.
var knownDecisions = map[DecisionType]bool{
	DecisionChat:          true,
	DecisionPlan:          true,
	DecisionIngest:        true,
	DecisionWatch:         true,
	DecisionClarification: true,
}

// IsKnownDecision returns true if d is a valid DecisionType constant.
func IsKnownDecision(d DecisionType) bool { return knownDecisions[d] }

// RouterInput is the canonical envelope for any external stimulus delivered to
// the InputRouter. Transport adapters (gRPC, HTTP, filesystem) translate their
// native formats into RouterInput before calling Router.Resolve.
type RouterInput struct {
	// Body is the raw input text, not enriched or truncated by the adapter.
	Body string

	// SourceType identifies the transport: "grpc", "filesystem", "http", "webhook".
	SourceType string

	// ContentType is the MIME type of Body (e.g. "text/plain", "application/pdf").
	// Used by the INGEST branch to determine how to process binary content.
	ContentType string

	// Metadata carries key-value pairs from the transport layer.
	// Notable keys:
	//   "_ingest_tags" — comma-separated tag override for INGEST (default: "ingested_from_chat")
	Metadata map[string]string

	// Intent is an optional pre-classification hint from the Company Gateway.
	// When non-empty and a valid DecisionType constant, the Router returns this
	// decision immediately (Layer 0) without running Layers 1-3.
	// Unknown or empty values fall through to Layer 1.
	Intent DecisionType
}

// ClarificationOption is a single selectable choice inside a DecisionClarification response.
type ClarificationOption struct {
	// Label is the human-readable description shown to the user.
	Label string
	// Decision is the DecisionType this option resolves to.
	Decision DecisionType
	// Recommended is true for the highest-confidence Layer 3 candidate.
	Recommended bool
}

// ChatParams carries parameters for the CHAT decision.
type ChatParams struct{}

// PlanParams carries parameters for the PLAN decision.
type PlanParams struct{}

// IngestParams carries parameters for the INGEST decision.
type IngestParams struct {
	// Tags overrides the default ["ingested_from_chat"] tag for ingested documents.
	Tags []string
}

// WatchParams carries parameters for the WATCH decision.
type WatchParams struct{}

// RouterDecision is the output of InputRouter.Resolve.
type RouterDecision struct {
	Type DecisionType

	// ClarificationQuestion is the question posed to the user.
	// Non-empty only when Type == DecisionClarification.
	ClarificationQuestion string

	// ClarificationOptions lists the choices presented to the user.
	// Non-empty only when Type == DecisionClarification.
	ClarificationOptions []ClarificationOption

	ChatParams   *ChatParams
	PlanParams   *PlanParams
	IngestParams *IngestParams
	WatchParams  *WatchParams
}

// InputRouter is the universal entry point for all external input.
// It classifies a RouterInput into a RouterDecision using four ordered layers:
//
//	Layer 0 — Gateway pre-classification (RouterInput.Intent)
//	Layer 1 — Slash-prefix commands (/watch, /plan, /ingest)
//	Layer 2 — Word-boundary keyword heuristics
//	Layer 3 — LLM classification with structured output
//
// Implementations must be safe for concurrent use from multiple goroutines.
type InputRouter interface {
	Resolve(ctx context.Context, input RouterInput) (*RouterDecision, error)
}
