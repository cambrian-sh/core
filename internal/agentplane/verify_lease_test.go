package agentplane

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// verifyMDClient records the outgoing metadata of its VerifyOutput call.
type verifyMDClient struct {
	capturingAgentClient
	md metadata.MD
}

func (c *verifyMDClient) VerifyOutput(ctx context.Context, _ *pb.VerifyRequest, _ ...grpc.CallOption) (*pb.VerifyResponse, error) {
	c.md, _ = metadata.FromOutgoingContext(ctx)
	return &pb.VerifyResponse{QualityScore: 0.7}, nil
}

// The verifier's lease reaches the agent as the LeaseHeader metadata, because
// pb.VerifyRequest has no lease field (ADR-0131;
// T-20261101-verification-mints-no-lease-so-verifier-llm); no lease ⇒ no header.
func TestVerifyOutput_SendsLeaseAsMetadata(t *testing.T) {
	c := &verifyMDClient{}
	a := buildTestAuctioneer("verifier", c)
	agent := domain.AgentDefinition{ID: "verifier"}

	if _, err := a.VerifyOutput(context.Background(), agent, domain.VerifyRequest{TaskID: "t", LeaseID: "lease-v"}); err != nil {
		t.Fatalf("VerifyOutput: %v", err)
	}
	if got := c.md.Get(domain.LeaseHeader); len(got) != 1 || got[0] != "lease-v" {
		t.Fatalf("%s metadata = %v, want [lease-v]", domain.LeaseHeader, got)
	}

	if _, err := a.VerifyOutput(context.Background(), agent, domain.VerifyRequest{TaskID: "t"}); err != nil {
		t.Fatalf("VerifyOutput: %v", err)
	}
	if got := c.md.Get(domain.LeaseHeader); len(got) != 0 {
		t.Fatalf("no lease must send no %s header, got %v", domain.LeaseHeader, got)
	}
}
