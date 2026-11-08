package network_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/identity"
	"github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// recordingHub is the real addressed hub (ADR-0142 D11) recording who the
// kernel says answered.
type recordingHub struct {
	*domain.AddressedHub
	mu        sync.Mutex
	approvers []string
}

func newRecordingHub() *recordingHub {
	return &recordingHub{AddressedHub: domain.NewAddressedHub(domain.AddressedHubConfig{DefaultTimeout: 5 * time.Second})}
}

func (h *recordingHub) Answer(ctx context.Context, id string, ans domain.ApprovalAnswer) (domain.ApprovalItem, error) {
	h.mu.Lock()
	h.approvers = append(h.approvers, ans.By.Principal.ID)
	h.mu.Unlock()
	return h.AddressedHub.Answer(ctx, id, ans)
}

// pendingToolApproval raises an operator-ruled approval nobody blocks on.
func pendingToolApproval(t *testing.T, h *recordingHub, tool string) string {
	t.Helper()
	it, err := h.Raise(context.Background(), domain.ApprovalRequest{AgentID: "worker", ToolName: tool})
	if err != nil {
		t.Fatal(err)
	}
	return it.ID
}

// startApprovalServer serves the Orchestrator behind the SAME interceptor chain
// app.go installs (surface stamp, then operator auth), so the tests exercise the
// production gate rather than a re-statement of it.
func startApprovalServer(t *testing.T, hub domain.ApprovalHub, idp domain.Authenticator) pb.OrchestratorClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(authz.UnarySurfaceInterceptor(nil), operator.UnaryAuthInterceptor(idp, nil)),
		grpc.ChainStreamInterceptor(authz.StreamSurfaceInterceptor(nil), operator.StreamAuthInterceptor(idp, nil)),
	)
	pb.RegisterOrchestratorServer(srv, &network.Server{ApprovalHub: hub})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewOrchestratorClient(conn)
}

func approvalIDP(t *testing.T) (*identity.LocalAccounts, string, string) {
	t.Helper()
	ctx := context.Background()
	reg := identity.NewMemRegistry()
	idp := identity.NewLocalAccounts(reg, reg)
	for u, r := range map[string]string{"alice": "operator", "viewer": "viewer"} {
		if _, err := idp.EnsureAccount(ctx, u, "pw", []string{r}); err != nil {
			t.Fatal(err)
		}
	}
	op, _, err := idp.Login(ctx, "alice", "pw")
	if err != nil {
		t.Fatal(err)
	}
	vw, _, err := idp.Login(ctx, "viewer", "pw")
	if err != nil {
		t.Fatal(err)
	}
	return idp, op, vw
}

func withMD(kv ...string) context.Context {
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs(kv...))
}

// ADR-0039 D10: the approval surface is operator-authenticated, never
// agent-reachable. Before the fix, a client that simply omitted x-agent-id
// could approve its own dangerous tool call under any approver_id it liked.
func TestSubmitApprovalDecision_RequiresOperatorBearer(t *testing.T) {
	t.Parallel()
	idp, opTok, viewerTok := approvalIDP(t)
	hub := newRecordingHub()
	c := startApprovalServer(t, hub, idp)
	req := &pb.ApprovalDecisionRequest{Id: pendingToolApproval(t, hub, "shell"), Approve: true, ApproverId: "spoofed-boss"}

	// No x-agent-id, no bearer: the pre-fix bypass.
	if _, err := c.SubmitApprovalDecision(context.Background(), req); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous call must be Unauthenticated, got %v", err)
	}
	// An agent principal, no bearer.
	if _, err := c.SubmitApprovalDecision(withMD("x-agent-id", "rogue"), req); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("agent call must be refused, got %v", err)
	}
	// An agent that somehow also holds an operator token is still refused.
	if _, err := c.SubmitApprovalDecision(withMD("authorization", "Bearer "+opTok, "x-agent-id", "rogue"), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("agent principal must be refused even with a bearer, got %v", err)
	}
	// A Viewer may not approve (approval is a command).
	if _, err := c.SubmitApprovalDecision(withMD("authorization", "Bearer "+viewerTok), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer must be denied, got %v", err)
	}
	if len(hub.approvers) != 0 {
		t.Fatalf("no refused call may reach the hub, got %v", hub.approvers)
	}

	// The operator bearer works, and the approver is the AUTHENTICATED operator,
	// not the request's approver_id.
	resp, err := c.SubmitApprovalDecision(withMD("authorization", "Bearer "+opTok), req)
	if err != nil || !resp.GetOk() {
		t.Fatalf("operator call must succeed: resp=%v err=%v", resp, err)
	}
	if len(hub.approvers) != 1 || hub.approvers[0] != "alice" {
		t.Fatalf("approver must be the authenticated operator 'alice', got %v", hub.approvers)
	}
}

func TestWatchApprovals_RequiresOperatorBearer(t *testing.T) {
	t.Parallel()
	idp, opTok, _ := approvalIDP(t)
	hub := newRecordingHub()
	c := startApprovalServer(t, hub, idp)

	recvErr := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		s, err := c.WatchApprovals(ctx, &pb.WatchApprovalsRequest{})
		if err != nil {
			return err
		}
		_, err = s.Recv()
		return err
	}
	if err := recvErr(context.Background()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous watch must be Unauthenticated, got %v", err)
	}
	if err := recvErr(withMD("x-agent-id", "rogue")); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("agent watch must be refused, got %v", err)
	}

	ctx, cancel := context.WithTimeout(withMD("authorization", "Bearer "+opTok), 5*time.Second)
	defer cancel()
	s, err := c.WatchApprovals(ctx, &pb.WatchApprovalsRequest{})
	if err != nil {
		t.Fatalf("operator watch: %v", err)
	}
	// The server subscribes asynchronously; raise until the stream sees one.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				_, _ = hub.Raise(context.Background(), domain.ApprovalRequest{AgentID: "worker", ToolName: "shell", ArgsPreview: string(rune('a' + i%26))})
			}
		}
	}()
	got, err := s.Recv()
	if err != nil || got.GetId() == "" || got.GetRule() != "operator" || got.GetToolName() != "shell" {
		t.Fatalf("operator must receive the pending approval with its rule: got=%v err=%v", got, err)
	}
}

// Without the interceptor (a server assembled without it) the handler must not
// fall back to "no x-agent-id means operator": it fails closed.
func TestSubmitApprovalDecision_FailsClosedWithoutInterceptor(t *testing.T) {
	t.Parallel()
	hub := newRecordingHub()
	s := &network.Server{ApprovalHub: hub}
	_, err := s.SubmitApprovalDecision(context.Background(), &pb.ApprovalDecisionRequest{Id: "a1", Approve: true})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unstamped context must be refused, got %v", err)
	}
	if len(hub.approvers) != 0 {
		t.Fatal("refused call reached the hub")
	}
}
