package agentplane

import (
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/metabolism/yield"
)

// The yield wire contract (ADR-0041 D5) crosses this boundary: the Python SDK's
// yield_subgoal() + runtime._handoff_from_result put the sub-goal on
// Handoff.metadata and stamp payload.type "yield_subgoal". This pins that the
// kernel side of the conversion still presents it as a yield to the consumer in
// internal/metabolism/yield, and that the resume keys reach the agent as
// metadata (where react.py's Seed C reads them).
func TestYieldWireContract_SDKShapedHandoff(t *testing.T) {
	sdk := &pb.Handoff{
		Id:        "h1",
		FromAgent: "analyst",
		Payload:   &pb.Object{Type: "yield_subgoal"},
		Metadata: map[string]string{
			"_yield":                    "true",
			"_yield_intent":             "summarise the risk factors section",
			"_yield_capability_hint":    "summarisation",
			"_yield_continuation_state": "Y29udA==",
		},
	}
	d := protoToDomainHandoff(sdk)
	if !yield.IsYield(d) {
		t.Fatalf("SDK yield not recognised after conversion: %+v", d)
	}
	if d.Context[yield.KeyIntent] != "summarise the risk factors section" ||
		d.Context[yield.KeyCapabilityHint] != "summarisation" {
		t.Fatalf("sub-goal fields lost in conversion: %v", d.Context)
	}

	d.Context = map[string]string{yield.KeyResult: "RISKS", yield.KeyResumedIntent: "x"}
	back := domainToProtoHandoff(d)
	if back.Metadata["_yield_result"] != "RISKS" || back.Metadata["_yield_resumed_intent"] != "x" {
		t.Fatalf("resume keys must travel as metadata: %v", back.Metadata)
	}
}
