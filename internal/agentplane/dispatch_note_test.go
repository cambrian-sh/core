package agentplane

import (
	"context"
	"net"
	"sync"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// notingDialer boots one bufconn "instance" per agent and records every
// NoteDispatch (the agentmgr.AgentManager surface, ADR-0157 D5.2).
type notingDialer struct {
	countingDialer
	lis   *bufconn.Listener
	mu    sync.Mutex
	notes []string
}

func (d *notingDialer) GetOrBootInstance(_ context.Context, def *domain.AgentDefinition, _ string) (*domain.Instance, error) {
	return &domain.Instance{ID: "inst-" + def.ID, AgentID: def.ID, SocketPath: "bufnet"}, nil
}
func (d *notingDialer) DialAgent(string) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return d.lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
}
func (d *notingDialer) BootDedicatedInstance(_ context.Context, def *domain.AgentDefinition) (*domain.Instance, error) {
	return &domain.Instance{ID: "inst-dedicated", AgentID: def.ID, SocketPath: "bufnet"}, nil
}
func (d *notingDialer) EvictInstance(string) {}
func (d *notingDialer) NoteDispatch(agentID, instanceID string, h *domain.Handoff) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.notes = append(d.notes, agentID+"|"+instanceID+"|"+h.Context["_session_token_id"])
}

// T-20261118-agent-artifacts-from-planner-lane-steps-carry: every dispatch
// through the Transport — cached client or dedicated instance — tells the
// manager which instance works under the handoff's lease, before the call.
func TestCallAgent_NotesTheInstanceAndLeaseOfEveryDispatch(t *testing.T) {
	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer()
	pb.RegisterAgentServiceServer(srv, echoAgent{})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	d := &notingDialer{lis: lis}
	a := New(d)
	h := func(lease string) *domain.Handoff {
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("hi")}, Context: map[string]string{"_session_token_id": lease}}
	}
	for _, lease := range []string{"lease-1", "lease-2"} { // the second reuses the cached client
		if _, err := a.CallAgent(context.Background(), "worker", h(lease), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.CallAgent(domain.WithDedicatedInstance(context.Background()), "worker", h("lease-3"), ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"worker|inst-worker|lease-1", "worker|inst-worker|lease-2", "worker|inst-dedicated|lease-3"}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.notes) != len(want) {
		t.Fatalf("notes = %v, want %v", d.notes, want)
	}
	for i := range want {
		if d.notes[i] != want[i] {
			t.Fatalf("notes = %v, want %v", d.notes, want)
		}
	}
}
