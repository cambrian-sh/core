package operator

import (
	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// toOperatorEvent maps a proto-free domain.SequencedEvent to the wire envelope.
// This is the sole domain→proto translation point in the operator plane
// (ADR-0047): domain stays free of any proto import. An unrecognized
// event type yields an envelope with seq/ts but no payload (forward-compatible).
func toOperatorEvent(se domain.SequencedEvent) *pb.OperatorEvent {
	out := &pb.OperatorEvent{
		Seq: se.Seq,
		Ts:  timestamppb.New(se.At),
	}

	switch e := se.Event.(type) {
	case domain.AuctionEventPayload:
		bids := make([]*pb.BidEntryOp, len(e.Bids))
		for i, b := range e.Bids {
			bids[i] = &pb.BidEntryOp{
				AgentId:    b.AgentID,
				Confidence: b.Confidence,
				Rationale:  b.Rationale,
				LatencyMs:  b.LatencyMs,
			}
		}
		out.Payload = &pb.OperatorEvent_Auction{Auction: &pb.AuctionEventOp{
			TaskId:   e.TaskID,
			TaskDesc: e.TaskDesc,
			Status:   e.Status,
			WinnerId: e.WinnerID,
			Bids:     bids,
			ErrorMsg: e.ErrorMsg,
		}}

	case domain.AgentReadyEvent:
		out.Payload = &pb.OperatorEvent_AgentReady{AgentReady: &pb.AgentReadyOp{
			AgentId:      e.AgentID,
			SourceHash:   e.SourceHash,
			TrustScore:   e.TrustScore,
			Capabilities: e.Capabilities,
			InterviewMs:  e.InterviewMs,
		}}

	case domain.SessionDormantEvent:
		out.SessionId = e.SessionID
		out.Payload = &pb.OperatorEvent_SessionDormant{SessionDormant: &pb.SessionDormantOp{
			SessionId:  e.SessionID,
			TtlSeconds: int64(e.TTLDuration.Seconds()),
		}}

	case domain.SessionCompletedEvent:
		out.SessionId = e.SessionID
		out.Payload = &pb.OperatorEvent_SessionCompleted{SessionCompleted: &pb.SessionCompletedOp{
			SessionId:       e.SessionID,
			DocumentsMerged: int32(e.DocumentsMerged),
		}}

	case domain.MemoryPressureEvent:
		out.Payload = &pb.OperatorEvent_MemoryPressure{MemoryPressure: &pb.MemoryPressureOp{
			TotalDocuments: int32(e.TotalDocuments),
			IndexSizeBytes: e.IndexSizeBytes,
			Trigger:        e.Trigger,
		}}

	case domain.DaemonCrashedEvent:
		out.Payload = &pb.OperatorEvent_DaemonCrashed{DaemonCrashed: &pb.DaemonCrashedOp{
			AgentId:  e.AgentID,
			StreamId: e.StreamID,
		}}

	case domain.WatchTriggeredEvent:
		out.SessionId = e.StreamID
		out.Payload = &pb.OperatorEvent_WatchTriggered{WatchTriggered: &pb.WatchTriggeredOp{
			WatchConfigId: e.WatchConfigID,
			StreamId:      e.StreamID,
			ActionTarget:  e.ActionTarget,
		}}

	case domain.MemoryWrittenEvent:
		out.SessionId = e.SessionID
		out.Payload = &pb.OperatorEvent_MemoryWritten{MemoryWritten: &pb.MemoryWrittenOp{
			DocId:     e.DocID,
			DocType:   e.DocType,
			SessionId: e.SessionID,
			Source:    e.Source,
			Summary:   e.Summary,
		}}

	case domain.HITLRaisedEvent:
		out.SessionId = e.SessionID
		out.Payload = &pb.OperatorEvent_HitlRaised{HitlRaised: &pb.HITLRaisedOp{
			InterventionId: e.InterventionID,
			SessionId:      e.SessionID,
			AgentId:        e.AgentID,
			Description:    e.Description,
			IsDestructive:  e.IsDestructive,
		}}

	case domain.VerifierRoundEvent:
		out.Payload = &pb.OperatorEvent_VerifierRound{VerifierRound: &pb.VerifierRoundOp{
			TaskId:        e.TaskID,
			WinnerAgent:   e.WinnerAgent,
			QualityScore:  e.QualityScore,
			BidConfidence: e.BidConf,
			Critique:      e.Critique,
		}}

	case domain.LLMHealthEvent:
		out.Payload = &pb.OperatorEvent_LlmHealth{LlmHealth: &pb.LLMHealthOp{
			ModelId: e.ModelID,
			State:   e.State,
			Reason:  e.Reason,
		}}

	case domain.PlanStateChanged:
		out.SessionId = e.SessionID
		out.Payload = &pb.OperatorEvent_PlanState{PlanState: &pb.PlanStateOp{
			SessionId:   e.SessionID,
			PlanId:      e.PlanID,
			ActiveStep:  int32(e.ActiveStep),
			Status:      e.Status,
			ActiveAgent: e.ActiveAgent,
			CostSoFar:   e.CostSoFar,
			Terminal:    e.Terminal,
		}}

	case domain.AuditEvent:
		out.Payload = &pb.OperatorEvent_Audit{Audit: auditEntryToOp(e.Entry)}

	case domain.TokenChunkEvent:
		out.SessionId = e.SessionID
		out.Payload = &pb.OperatorEvent_Token{Token: &pb.TokenChunkOp{
			SessionId: e.SessionID,
			StepIndex: int32(e.StepIndex),
			Text:      e.Text,
		}}
	}

	return out
}

// auditEntryToOp maps a domain.AuditEntry to its wire form. Shared by the feed
// (AuditEvent) and the QueryAudit read RPC.
func auditEntryToOp(a domain.AuditEntry) *pb.AuditOp {
	return &pb.AuditOp{
		Id:         a.ID,
		CommandId:  a.CommandID,
		Actor:      a.Actor,
		Role:       a.Role,
		ActionType: a.ActionType,
		TargetType: a.TargetType,
		TargetId:   a.TargetID,
		Before:     a.Before,
		After:      a.After,
		Reason:     a.Reason,
		Result:     a.Result,
	}
}
