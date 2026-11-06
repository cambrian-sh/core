package operator

import (
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// ADR-0139 amendment 2026-11-06: the feed's token lane carries the step-output
// stream — every producer, with its kind, lease, sequence and source — while the
// historical fields keep their meaning (session_id has always carried the lease).
func TestToOperatorEvent_StepOutputChunk(t *testing.T) {
	ev := domain.TokenChunkFromStepOutput(domain.StepOutputChunk{
		LeaseID: "L1", SessionID: "task-7", StepIndex: 3, AgentID: "writer",
		Seq: 9, Kind: domain.StepOutputProgress, Data: "half way", Final: true,
		Source: domain.StepOutputFromAgent,
	})
	out := toOperatorEvent(domain.SequencedEvent{Seq: 0, Event: ev})
	tok, ok := out.GetPayload().(*pb.OperatorEvent_Token)
	if !ok {
		t.Fatalf("payload = %T", out.GetPayload())
	}
	got := tok.Token
	if got.GetSessionId() != "L1" || got.GetLeaseId() != "L1" || got.GetTaskSessionId() != "task-7" ||
		got.GetStepIndex() != 3 || got.GetAgentId() != "writer" || got.GetText() != "half way" ||
		got.GetKind() != "progress" || got.GetChunkSeq() != 9 || !got.GetFinal() || got.GetSource() != "agent" {
		t.Fatalf("TokenChunkOp = %+v", got)
	}
	if out.GetSeq() != 0 {
		t.Errorf("the lane is live-only: seq = %d", out.GetSeq())
	}
}
