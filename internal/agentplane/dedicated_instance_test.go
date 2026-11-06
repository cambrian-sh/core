package agentplane

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// echoAgent is a real AgentService over bufconn.
type echoAgent struct {
	pb.UnimplementedAgentServiceServer
}

func (echoAgent) Execute(_ context.Context, h *pb.Handoff) (*pb.Handoff, error) {
	return &pb.Handoff{Payload: &pb.Object{Type: "text", Data: []byte("child:" + string(h.GetPayload().GetData()))}}, nil
}

// dedicatedDialer boots "instances" that are one bufconn server, and records
// dedicated boots and evictions.
type dedicatedDialer struct {
	countingDialer
	lis     *bufconn.Listener
	boots   atomic.Int32
	evicted atomic.Value
}

func (d *dedicatedDialer) BootDedicatedInstance(_ context.Context, def *domain.AgentDefinition) (*domain.Instance, error) {
	d.boots.Add(1)
	return &domain.Instance{ID: "inst-dedicated", AgentID: def.ID, SocketPath: "bufnet"}, nil
}
func (d *dedicatedDialer) EvictInstance(id string) { d.evicted.Store(id) }
func (d *dedicatedDialer) DialAgent(string) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return d.lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// ADR-0139 D12: a call marked WithDedicatedInstance runs on a freshly booted
// instance that is evicted afterwards, and never touches the shared client cache
// (replacing it would close the busy parent's connection mid-call).
func TestCallAgent_DedicatedInstance_BootsUsesAndEvicts(t *testing.T) {
	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer()
	pb.RegisterAgentServiceServer(srv, echoAgent{})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	d := &dedicatedDialer{lis: lis}
	a := New(d)
	ctx := domain.WithDedicatedInstance(context.Background())
	resp, err := a.CallAgent(ctx, "worker", &domain.Handoff{Payload: &domain.Payload{Data: []byte("hi")}}, "")
	if err != nil {
		t.Fatalf("CallAgent: %v", err)
	}
	if string(resp.Payload.Data) != "child:hi" {
		t.Errorf("response %q", resp.Payload.Data)
	}
	if d.boots.Load() != 1 || d.evicted.Load() != "inst-dedicated" {
		t.Errorf("boots %d, evicted %v", d.boots.Load(), d.evicted.Load())
	}
	if len(a.agentClients) != 0 || d.dials.Load() != 0 {
		t.Errorf("the dedicated call must not use or populate the shared cache: %d clients", len(a.agentClients))
	}
}
