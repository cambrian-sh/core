package network_test

import (
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// failingWatchHandler fails the test if a retired RPC ever reaches the store.
type failingWatchHandler struct{ t *testing.T }

func (h failingWatchHandler) RegisterWatch(domain.WatchConfig) (string, error) {
	h.t.Error("retired Orchestrator/RegisterWatch reached the watch store")
	return "", nil
}
func (h failingWatchHandler) ListWatches() ([]domain.WatchConfig, error) {
	h.t.Error("retired Orchestrator/ListWatches reached the watch store")
	return nil, nil
}
func (h failingWatchHandler) DeleteWatch(string) error {
	h.t.Error("retired Orchestrator/DeleteWatch reached the watch store")
	return nil
}
func (h failingWatchHandler) SetWatchActive(string, bool) error {
	h.t.Error("retired Orchestrator/SetWatchActive reached the watch store")
	return nil
}

// startWatchServer serves the Orchestrator behind the production interceptor
// chain, with a WatchHandler wired as the premium binary would.
func startWatchServer(t *testing.T, idp operator.OperatorIdentity) pb.OrchestratorClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(authz.UnarySurfaceInterceptor(nil), operator.UnaryAuthInterceptor(idp, nil)),
	)
	pb.RegisterOrchestratorServer(srv, &network.Server{WatchHandler: failingWatchHandler{t}})
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

// T-20261031-orchestrator-watch-crud-registerwatch: the four Orchestrator watch
// RPCs had no guard at all. They are now operator-gated AND retired: no caller
// reaches the watch store through them, and an authenticated operator is told
// which audited OperatorConsole command to use.
func TestOrchestratorWatchCRUD_RetiredAndOperatorGated(t *testing.T) {
	t.Parallel()
	idp, opTok, _ := approvalIDP(t)
	c := startWatchServer(t, idp)

	calls := map[string]func(context.Context) error{
		"RegisterWatch": func(ctx context.Context) error {
			_, err := c.RegisterWatch(ctx, &pb.RegisterWatchRequest{Config: &pb.WatchConfigProto{Id: "w1"}})
			return err
		},
		"ListWatches": func(ctx context.Context) error {
			_, err := c.ListWatches(ctx, &pb.ListWatchesRequest{})
			return err
		},
		"DeleteWatch": func(ctx context.Context) error {
			_, err := c.DeleteWatch(ctx, &pb.DeleteWatchRequest{Id: "w1"})
			return err
		},
		"SetWatchActive": func(ctx context.Context) error {
			_, err := c.SetWatchActive(ctx, &pb.SetWatchActiveRequest{Id: "w1", Active: false})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			// No credentials at all: the hole this closes.
			if err := call(context.Background()); status.Code(err) != codes.Unauthenticated {
				t.Errorf("anonymous: got %v, want Unauthenticated", err)
			}
			// An agent's self-asserted header is not an operator.
			if err := call(withMD("x-agent-id", "rogue")); status.Code(err) != codes.Unauthenticated {
				t.Errorf("agent: got %v, want Unauthenticated", err)
			}
			// A real operator is refused too, and told where to go instead.
			err := call(withMD("authorization", "Bearer "+opTok))
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("operator: got %v, want FailedPrecondition", err)
			}
			if msg := status.Convert(err).Message(); !strings.Contains(msg, "OperatorConsole/"+name) {
				t.Errorf("operator: message %q does not name OperatorConsole/%s", msg, name)
			}
		})
	}
}

// The gate is one list: the four methods are stamped operator-surface, so no
// path can treat them as agent-plane calls.
func TestOrchestratorWatchCRUD_AreOperatorGatedMethods(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"RegisterWatch", "ListWatches", "DeleteWatch", "SetWatchActive"} {
		full := "/cambrian.Orchestrator/" + m
		if !authz.IsOperatorGatedAgentServiceMethod(full) {
			t.Errorf("%s is not operator-gated", full)
		}
		if got := authz.SurfaceForMethod(full, nil); got.Kind != domain.SurfaceOperator {
			t.Errorf("%s surface = %v, want operator", full, got.Kind)
		}
	}
}
