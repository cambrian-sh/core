package auctioneer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// countingDialer reports how many times DialAgent actually ran, and returns a
// lazy gRPC conn (IDLE until an RPC, so no server is needed).
type countingDialer struct{ dials atomic.Int32 }

func (d *countingDialer) GetOrBootInstance(_ context.Context, def *domain.AgentDefinition, _ string) *domain.Instance {
	return &domain.Instance{SocketPath: "/tmp/" + def.ID + ".sock"}
}
func (d *countingDialer) DialAgent(_ string) (*grpc.ClientConn, error) {
	d.dials.Add(1)
	return grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()))
}
func (d *countingDialer) GetAgentByName(_ context.Context, name string) (*domain.AgentDefinition, error) {
	return &domain.AgentDefinition{ID: name}, nil
}
func (d *countingDialer) GetManifest(_ context.Context, _ string) (*domain.AgentManifest, error) {
	return nil, nil
}

// Concurrent cache-misses for the same agent must single-flight to ONE dial.
// Pre-fix, each goroutine dialed its own conn and RegisterAgentClient closed the
// previous one mid-RPC ("the client connection is closing", surfaced by the EFE
// path). Mutation-proof: revert getOrDialClient to dial-per-call and dials > 1.
func TestGetOrDialClient_SingleFlightsConcurrentMisses(t *testing.T) {
	d := &countingDialer{}
	a := &Auctioneer{
		agentClients: map[string]pb.AgentServiceClient{},
		agentConns:   map[string]*grpc.ClientConn{},
		Manager:      d,
	}
	agent := domain.AgentDefinition{ID: "analyst_agent"}

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if _, err := a.getOrDialClient(context.Background(), agent, ""); err != nil {
				t.Errorf("getOrDialClient: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := d.dials.Load(); got != 1 {
		t.Fatalf("DialAgent ran %d times for concurrent misses; want 1 (single-flight ⇒ no replace-close race)", got)
	}
	if len(a.agentConns) != 1 {
		t.Errorf("agentConns has %d entries; want exactly 1 (no conn replaced/closed)", len(a.agentConns))
	}
}

// A warm cache hit never dials: the fast path stays lock-cheap.
func TestGetOrDialClient_CacheHitDoesNotDial(t *testing.T) {
	d := &countingDialer{}
	a := &Auctioneer{
		agentClients: map[string]pb.AgentServiceClient{},
		agentConns:   map[string]*grpc.ClientConn{},
		Manager:      d,
	}
	agent := domain.AgentDefinition{ID: "analyst_agent"}

	if _, err := a.getOrDialClient(context.Background(), agent, ""); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := a.getOrDialClient(context.Background(), agent, ""); err != nil {
			t.Fatalf("cached call: %v", err)
		}
	}
	if got := d.dials.Load(); got != 1 {
		t.Fatalf("DialAgent ran %d times; want 1 (subsequent calls hit the cache)", got)
	}
}
