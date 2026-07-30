package operator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// SetTokenSeriesReader wires the hourly token accumulator. nil ⇒ GetTokenSeries
// returns Unimplemented, which a console renders as "no history is projected"
// rather than drawing a flat line that reads as zero spend.
func (s *Service) SetTokenSeriesReader(r domain.TokenSeriesReader) { s.tokenSeries = r }

// HasTokenSeries reports whether the series is available.
func (s *Service) HasTokenSeries() bool { return s.tokenSeries != nil }

// GetTokenSeries returns recent hourly token usage. Read RPC.
func (s *Service) GetTokenSeries(_ context.Context, req *pb.GetTokenSeriesOpRequest) (*pb.GetTokenSeriesOpResponse, error) {
	if s.tokenSeries == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel does not project token history")
	}
	buckets := s.tokenSeries.TokenSeries(int(req.GetHours()))
	out := make([]*pb.TokenSeriesPointOp, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, &pb.TokenSeriesPointOp{
			HourStartUnixMs: b.HourStart.UnixMilli(),
			InputTokens:     b.InputTokens,
			OutputTokens:    b.OutputTokens,
			Calls:           b.Calls,
		})
	}
	return &pb.GetTokenSeriesOpResponse{
		Points:         out,
		RetentionHours: int32(len(buckets)),
	}, nil
}
