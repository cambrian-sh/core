package auctioneer

import (
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// stubCalibrator returns a fixed calibrated score per agent (test double for
// *calibration.Model).
type stubCalibrator map[string]float64

func (s stubCalibrator) Calibrate(agentID string, conf float64) float64 {
	if v, ok := s[agentID]; ok {
		return v
	}
	return conf
}

// ROUTE-05 / ADR-0068: with a calibrator wired, the winner is the highest CALIBRATED
// confidence — even though a different agent has the highest raw self-report.
func TestExecute_CalibratedSelection_FlipsWinner(t *testing.T) {
	defs := []struct {
		id   string
		conf float64
	}{
		{"overconfident", 0.95}, // highest raw bid, but low verified quality
		{"solid", 0.80},         // lower raw bid, high verified quality
	}
	agents := make(map[string]domain.AgentDefinition)
	manifests := make(map[string]*domain.AgentManifest)
	mocks := make(map[string]*mockAgentClient)
	agentPtrs := map[string]*domain.AgentDefinition{}
	for _, d := range defs {
		agents[d.id] = domain.AgentDefinition{ID: d.id}
		manifests[d.id] = &domain.AgentManifest{Tools: []string{d.id}, SupportedFormats: []string{d.id}}
		mocks[d.id] = &mockAgentClient{
			proposal: &pb.ProposalResponse{Confidence: float32(d.conf), EstimatedLatencyMs: 50},
			execute:  &pb.Handoff{Payload: &pb.Object{Data: []byte("r-" + d.id)}},
		}
		a := agents[d.id]
		agentPtrs[d.id] = &a
	}

	cfg := config.ExecutionConfig{MinAuctionConfidence: 0.3, MaxRecursionDepth: 3, GatekeeperMaxCandidates: 3}
	auc := New(&mockDialer{agents: agentPtrs}, &testGatekeeper{agents: agents, manifests: manifests}, cfg)
	for _, d := range defs {
		auc.RegisterAgentClient(d.id, &pbClientWrapper{m: mocks[d.id]}, nil)
	}

	task := &domain.AuctionTask{ID: "t-cal", Description: "calibration flip"}
	handoff := &domain.Handoff{Payload: &domain.Payload{Data: []byte("in")}}

	// Baseline (no calibrator): raw confidence wins → overconfident.
	res, err := auc.Execute(t.Context(), task, handoff)
	if err != nil {
		t.Fatalf("baseline Execute: %v", err)
	}
	if got := string(res.Handoff.Payload.Data); got != "r-overconfident" {
		t.Fatalf("baseline winner should be the highest raw bid (overconfident), got %q", got)
	}

	// With calibration: overconfident's 0.95 maps to 0.30, solid's 0.80 maps to 0.85.
	auc.Calibrator = stubCalibrator{"overconfident": 0.30, "solid": 0.85}
	res, err = auc.Execute(t.Context(), task, handoff)
	if err != nil {
		t.Fatalf("calibrated Execute: %v", err)
	}
	if got := string(res.Handoff.Payload.Data); got != "r-solid" {
		t.Fatalf("calibrated winner should flip to solid, got %q", got)
	}
}
