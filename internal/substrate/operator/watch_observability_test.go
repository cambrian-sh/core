package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

type fakeMetricsReader struct{ ms []domain.WatchMetrics }

func (f *fakeMetricsReader) WatchMetrics() []domain.WatchMetrics { return f.ms }

type fakeBacktester struct{ verdicts []domain.WatchBacktestVerdict }

func (f *fakeBacktester) Backtest(context.Context, domain.WatchConfig, uint64) ([]domain.WatchBacktestVerdict, error) {
	return f.verdicts, nil
}

// REACT-05 / ADR-0071: unwired (OSS) ⇒ both observability RPCs are Unimplemented.
func TestWatchObservability_UnimplementedWhenUnwired(t *testing.T) {
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	if _, err := svc.GetWatchMetrics(context.Background(), &pb.GetWatchMetricsOpRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("GetWatchMetrics should be Unimplemented, got %v", err)
	}
	if _, err := svc.BacktestWatch(context.Background(), &pb.BacktestWatchOpRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("BacktestWatch should be Unimplemented, got %v", err)
	}
}

// Wired: metrics + verdicts are mapped to the proto shape.
func TestWatchObservability_MapsMetricsAndVerdicts(t *testing.T) {
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetWatchObservability(
		&fakeMetricsReader{ms: []domain.WatchMetrics{{
			WatchID: "w1", SignalsSeen: 10, ConditionFired: 4, ConditionSuppressed: 6,
			DryRunWouldFire: 2, ConditionEvalCount: 10, ConditionLatencyMsTot: 500,
		}}},
		&fakeBacktester{verdicts: []domain.WatchBacktestVerdict{
			{Seq: 1, StreamID: "s1", RawText: "a", WouldFire: true},
			{Seq: 2, StreamID: "s1", RawText: "b", WouldFire: false},
		}},
	)

	mr, err := svc.GetWatchMetrics(context.Background(), &pb.GetWatchMetricsOpRequest{})
	if err != nil {
		t.Fatalf("GetWatchMetrics: %v", err)
	}
	if len(mr.GetMetrics()) != 1 || mr.GetMetrics()[0].GetWatchId() != "w1" ||
		mr.GetMetrics()[0].GetConditionFired() != 4 || mr.GetMetrics()[0].GetMeanConditionLatencyMs() != 50 {
		t.Fatalf("unexpected metrics mapping: %+v", mr.GetMetrics())
	}

	br, err := svc.BacktestWatch(context.Background(), &pb.BacktestWatchOpRequest{Config: &pb.WatchConfigOp{Id: "cand"}})
	if err != nil {
		t.Fatalf("BacktestWatch: %v", err)
	}
	if len(br.GetVerdicts()) != 2 || !br.GetVerdicts()[0].GetWouldFire() || br.GetVerdicts()[1].GetWouldFire() {
		t.Fatalf("unexpected verdict mapping: %+v", br.GetVerdicts())
	}
}
