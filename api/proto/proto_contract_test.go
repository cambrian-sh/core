package proto_test

import (
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"

	"google.golang.org/protobuf/proto"
)

// ── Cycle 1: ProposalRequest.ConfidenceHint ──────────────────────────────────

func TestProposalRequest_ConfidenceHint_RoundTrip(t *testing.T) {
	original := &pb.ProposalRequest{
		TaskId:         "task-001",
		Description:    "sql analysis",
		ConfidenceHint: 0.85,
	}
	b, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.ProposalRequest
	if err := proto.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ConfidenceHint != float32(0.85) {
		t.Errorf("want ConfidenceHint=0.85, got %f", decoded.ConfidenceHint)
	}
}

func TestProposalRequest_ZeroConfidenceHint_IsDefaultFloat(t *testing.T) {
	req := &pb.ProposalRequest{TaskId: "task-002"}
	if req.ConfidenceHint != 0.0 {
		t.Errorf("want zero default ConfidenceHint, got %f", req.ConfidenceHint)
	}
}

// ── Cycle 2: VerifyRequest / VerifyResponse ───────────────────────────────────

func TestVerifyRequest_AllFields_RoundTrip(t *testing.T) {
	original := &pb.VerifyRequest{
		TaskId:        "task-003",
		OriginalQuery: "SELECT * FROM orders",
		WinnerOutput:  "3 rows returned",
		WinnerAgentId: "sql-agent",
		BidConfidence: 0.9,
	}
	b, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.VerifyRequest
	if err := proto.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.TaskId != "task-003" {
		t.Errorf("TaskId: want task-003, got %q", decoded.TaskId)
	}
	if decoded.OriginalQuery != "SELECT * FROM orders" {
		t.Errorf("OriginalQuery: want SELECT..., got %q", decoded.OriginalQuery)
	}
	if decoded.WinnerOutput != "3 rows returned" {
		t.Errorf("WinnerOutput mismatch")
	}
	if decoded.WinnerAgentId != "sql-agent" {
		t.Errorf("WinnerAgentId: want sql-agent, got %q", decoded.WinnerAgentId)
	}
	if decoded.BidConfidence != float32(0.9) {
		t.Errorf("BidConfidence: want 0.9, got %f", decoded.BidConfidence)
	}
}

func TestVerifyResponse_QualityScoreAndCritique_RoundTrip(t *testing.T) {
	original := &pb.VerifyResponse{
		QualityScore: 0.95,
		Critique:     "Output is correct and well-structured",
	}
	b, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.VerifyResponse
	if err := proto.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.QualityScore != float32(0.95) {
		t.Errorf("QualityScore: want 0.95, got %f", decoded.QualityScore)
	}
	if decoded.Critique != "Output is correct and well-structured" {
		t.Errorf("Critique mismatch: %q", decoded.Critique)
	}
}

// ── Cycle 3: MemoryRequest / MemoryResult / MemoryResponse ───────────────────

func TestMemoryRequest_RoundTrip(t *testing.T) {
	original := &pb.MemoryRequest{
		Query: "What is the project budget?",
		TopK:  5,
	}
	b, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.MemoryRequest
	if err := proto.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Query != "What is the project budget?" {
		t.Errorf("Query mismatch")
	}
	if decoded.TopK != 5 {
		t.Errorf("TopK: want 5, got %d", decoded.TopK)
	}
}

func TestMemoryResponse_MultipleResults_RoundTrip(t *testing.T) {
	original := &pb.MemoryResponse{
		Results: []*pb.MemoryResult{
			{Text: "Budget is 500k", Score: 0.92, Metadata: `{"source":"meeting-notes"}`},
			{Text: "Budget approved Q1", Score: 0.87, Metadata: `{"source":"email"}`},
		},
	}
	b, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.MemoryResponse
	if err := proto.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Results) != 2 {
		t.Fatalf("want 2 results, got %d", len(decoded.Results))
	}
	if decoded.Results[0].Text != "Budget is 500k" {
		t.Errorf("Results[0].Text mismatch")
	}
	if decoded.Results[0].Score != float32(0.92) {
		t.Errorf("Results[0].Score: want 0.92, got %f", decoded.Results[0].Score)
	}
	if decoded.Results[1].Metadata != `{"source":"email"}` {
		t.Errorf("Results[1].Metadata mismatch")
	}
}

func TestMemoryResponse_EmptyResults_IsValid(t *testing.T) {
	resp := &pb.MemoryResponse{}
	if len(resp.Results) != 0 {
		t.Errorf("want empty results, got %d", len(resp.Results))
	}
}
