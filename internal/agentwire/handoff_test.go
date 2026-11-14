package agentwire

import (
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Every Handoff field the agent boundary carries survives the round trip, so
// both agent call paths (agentplane, agentmgr) carry the same fields.
func TestHandoffRoundTrip(t *testing.T) {
	in := &domain.Handoff{
		ID: "h1", FromAgent: "kernel", ToAgent: "worker", Confidence: 0.7,
		Uncertainties: []string{"u"},
		Context:       map[string]string{"_lane": "single_agent"},
		Payload:       &domain.Payload{ID: "p", Type: "task", Data: []byte("do it"), Metadata: map[string]string{"k": "v"}},
		WorkingMemory: []domain.ContextRef{{CID: "c1", Type: "doc", Labels: []string{"l"}, Activation: 0.5, Snippet: "s", Precision: 0.9}},
	}
	out := FromProto(ToProto(in))
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip lost data:\n in  %+v\n out %+v", in, out)
	}
	if ToProto(nil) != nil || FromProto(nil) != nil {
		t.Fatal("nil must map to nil")
	}
}
