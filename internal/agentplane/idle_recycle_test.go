package agentplane

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// recyclingDialer hands out a fresh instance id per boot, refuses Execute on
// the retiring ids, and reports a recycle at the end of every Execute on a
// stale id (the agentmgr.AgentManager idleRecycler surface, ADR-0157 D7.10.2).
type recyclingDialer struct {
	countingDialer
	lis      *bufconn.Listener
	mu       sync.Mutex
	booted   int
	retiring map[string]bool
	stale    map[string]bool
	began    []string
	ended    []string
}

func (d *recyclingDialer) GetOrBootInstance(_ context.Context, def *domain.AgentDefinition, _ string) (*domain.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.booted++
	return &domain.Instance{ID: fmt.Sprintf("inst-%d", d.booted), AgentID: def.ID, SocketPath: "bufnet"}, nil
}
func (d *recyclingDialer) DialAgent(string) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return d.lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
}
func (d *recyclingDialer) ExecuteBegan(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.retiring[id] {
		return false
	}
	d.began = append(d.began, id)
	return true
}
func (d *recyclingDialer) ExecuteEnded(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ended = append(d.ended, id)
	return d.stale[id]
}

// The Transport brackets every Execute; a recycled instance's cached
// connection is dropped so the next call boots a new instance; a retiring
// instance is never dispatched to (the Transport dials again).
func TestCallAgent_CountsExecutesAndDropsARecycledInstance(t *testing.T) {
	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer()
	pb.RegisterAgentServiceServer(srv, echoAgent{})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	d := &recyclingDialer{lis: lis, retiring: map[string]bool{}, stale: map[string]bool{"inst-1": true}}
	a := New(d)
	h := &domain.Handoff{Payload: &domain.Payload{Data: []byte("hi")}}
	ctx := context.Background()
	if _, err := a.CallAgent(ctx, "worker", h, ""); err != nil {
		t.Fatal(err)
	}
	// inst-1 was stale: recycled at the end of that Execute ⇒ the next call
	// boots inst-2 instead of reusing the dropped connection.
	if _, err := a.CallAgent(ctx, "worker", h, ""); err != nil {
		t.Fatal(err)
	}
	// inst-2 is retiring before the next call starts: the Transport must dial
	// again (inst-3) rather than dispatch to it.
	d.mu.Lock()
	d.retiring["inst-2"] = true
	d.mu.Unlock()
	if _, err := a.CallAgent(ctx, "worker", h, ""); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if got, want := fmt.Sprint(d.began), "[inst-1 inst-2 inst-3]"; got != want {
		t.Fatalf("began = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(d.ended), "[inst-1 inst-2 inst-3]"; got != want {
		t.Fatalf("ended = %s, want %s (every admitted Execute is ended once)", got, want)
	}
	if d.booted != 3 {
		t.Fatalf("boots = %d, want 3", d.booted)
	}
}
