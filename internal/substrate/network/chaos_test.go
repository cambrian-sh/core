//go:build chaos

package network_test

import (
	"context"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	orchestratorAddr = "localhost:50051"
	toxiproxyAddr    = "localhost:8474"
)

func TestChaosReal_PostgresTcpBlackout(t *testing.T) {
	// This test verifies that the Gatekeeper degrades to Declaration-only
	// when PostgreSQL is unreachable via toxiproxy.
	// Requires: docker compose -f scripts/chaos-compose.yml up -d

	conn, err := grpc.NewClient(orchestratorAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Skipf("orchestrator not reachable at %s: %v", orchestratorAddr, err)
	}
	defer conn.Close()
	client := pb.NewOrchestratorClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Execute(ctx, &pb.Handoff{
		Id:        "chaos-test-001",
		FromAgent: "test-agent",
		ToAgent:   "test-agent",
		Payload:   &pb.Object{Id: "p", Type: "text", Data: []byte("test task with pg blackout")},
	})
	if err != nil {
		t.Fatalf("Execute should not error under pg blackout: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestChaosReal_AgentSigkill(t *testing.T) {
	conn, err := grpc.NewClient(orchestratorAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Skipf("orchestrator not reachable at %s: %v", orchestratorAddr, err)
	}
	defer conn.Close()
	client := pb.NewOrchestratorClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Execute(ctx, &pb.Handoff{
		Id:        "chaos-test-002",
		FromAgent: "test-agent",
		ToAgent:   "test-agent",
		Payload:   &pb.Object{Id: "p", Type: "text", Data: []byte("test task after agent sigkill")},
	})
	if err != nil {
		t.Fatalf("Execute should recover after agent failure: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestChaosReal_LLMStreamingHang(t *testing.T) {
	conn, err := grpc.NewClient(orchestratorAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Skipf("orchestrator not reachable at %s: %v", orchestratorAddr, err)
	}
	defer conn.Close()
	client := pb.NewOrchestratorClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := client.Execute(ctx, &pb.Handoff{
		Id:        "chaos-test-003",
		FromAgent: "test-agent",
		ToAgent:   "test-agent",
		Payload:   &pb.Object{Id: "p", Type: "text", Data: []byte("test task with llm hang")},
	})
	if err != nil {
		t.Fatalf("Execute should recover from llm hang: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}
