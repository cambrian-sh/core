package authz_test

import (
	"context"
	"net"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/substrate/operator"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// ADR-0130 gate tests. The verifier here is built STRICT (waived=false), which
// is what the composition root builds whenever server.insecure_localhost is not
// set — on a routable bind that is the only legal configuration, so these are
// the routable-bind gates (criteria 1 and 2) exercised through the production
// interceptor chain rather than a restatement of it.

// secretStore is the ADR-0101 named-secret seam as the verifier sees it.
type secretStore map[string]string

func (s secretStore) resolve(name string) (string, bool) {
	v, ok := s[name]
	return v, ok
}

// recordingOrchestrator reaches one unary and one streaming handler so the
// test can tell "refused at the interceptor" from "refused by the handler".
type recordingOrchestrator struct {
	pb.UnimplementedOrchestratorServer
	seen chan domain.PrincipalRef
}

func (r *recordingOrchestrator) ListTools(ctx context.Context, _ *pb.ListToolsRequest) (*pb.ListToolsResponse, error) {
	r.seen <- domain.PrincipalFromContext(ctx)
	return &pb.ListToolsResponse{}, nil
}

func (r *recordingOrchestrator) SignalStream(ss pb.Orchestrator_SignalStreamServer) error {
	r.seen <- domain.PrincipalFromContext(ss.Context())
	return nil
}

// startAgentPlane serves the Orchestrator behind the SAME three-interceptor
// chain app.go installs: surface stamp, agent credential, operator bearer.
func startAgentPlane(t *testing.T, store secretStore, waived bool) (pb.OrchestratorClient, *recordingOrchestrator) {
	t.Helper()
	verifier := authz.NewAgentCredentialVerifier(store.resolve, waived)
	idp := operator.NewStaticIdentity(nil)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			authz.UnarySurfaceInterceptor(nil),
			authz.UnaryAgentAuthInterceptor(verifier, nil),
			operator.UnaryAuthInterceptor(idp, nil)),
		grpc.ChainStreamInterceptor(
			authz.StreamSurfaceInterceptor(nil),
			authz.StreamAgentAuthInterceptor(verifier, nil),
			operator.StreamAuthInterceptor(idp, nil)),
	)
	rec := &recordingOrchestrator{seen: make(chan domain.PrincipalRef, 8)}
	pb.RegisterOrchestratorServer(srv, rec)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewOrchestratorClient(conn), rec
}

func withMD(kv ...string) context.Context {
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs(kv...))
}

func wantCode(t *testing.T, err error, want codes.Code, what string) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("%s: code = %v (%v), want %v", what, status.Code(err), err, want)
	}
}

// Gate 1: with no credential, EVERY Orchestrator RPC that belongs to the agent
// plane is Unauthenticated. The method list is read from the generated service
// descriptor, so a new RPC cannot be added to the proto without being covered.
func TestAgentAuth_Gate1_NoCredentialIsUnauthenticatedOnEveryAgentRPC(t *testing.T) {
	t.Parallel()
	verifier := authz.NewAgentCredentialVerifier(secretStore{}.resolve, false)
	covered := 0
	for _, m := range pb.Orchestrator_ServiceDesc.Methods {
		full := "/" + pb.Orchestrator_ServiceDesc.ServiceName + "/" + m.MethodName
		if authz.IsOperatorGatedAgentServiceMethod(full) {
			continue // takes the operator bearer (ADR-0039 D10), gated elsewhere
		}
		if _, err := verifier.Authenticate(context.Background(), full, nil); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: no credential was NOT refused (err=%v)", full, err)
		}
		covered++
	}
	for _, s := range pb.Orchestrator_ServiceDesc.Streams {
		full := "/" + pb.Orchestrator_ServiceDesc.ServiceName + "/" + s.StreamName
		if authz.IsOperatorGatedAgentServiceMethod(full) {
			continue
		}
		if _, err := verifier.Authenticate(context.Background(), full, nil); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: no credential was NOT refused (err=%v)", full, err)
		}
		covered++
	}
	if covered == 0 {
		t.Fatal("no Orchestrator methods covered — the descriptor walk is broken")
	}
}

// Gate 1, over the wire: the chain refuses a bare x-agent-id (the pre-ADR-0130
// identity) on a unary and a streaming RPC, and the handler never runs.
func TestAgentAuth_Gate1_OverTheWire(t *testing.T) {
	t.Parallel()
	client, rec := startAgentPlane(t, secretStore{authz.AgentTokenSecretName("analyst_agent"): "s3cret"}, false)

	_, err := client.ListTools(context.Background(), &pb.ListToolsRequest{})
	wantCode(t, err, codes.Unauthenticated, "no metadata at all")

	_, err = client.ListTools(withMD("x-agent-id", "analyst_agent"), &pb.ListToolsRequest{})
	wantCode(t, err, codes.Unauthenticated, "x-agent-id with no token (the old hole)")

	_, err = client.ListTools(withMD("authorization", "Bearer s3cret"), &pb.ListToolsRequest{})
	wantCode(t, err, codes.Unauthenticated, "token with no x-agent-id")

	stream, err := client.SignalStream(withMD("x-agent-id", "analyst_agent"))
	if err == nil {
		_, err = stream.Recv()
	}
	wantCode(t, err, codes.Unauthenticated, "streaming RPC with no token")

	select {
	case p := <-rec.seen:
		t.Fatalf("handler ran for an unauthenticated call with principal %+v", p)
	default:
	}
}

// Gate 2: a token that matches SOME agent's secret, presented with a different
// x-agent-id, is refused — the header is an assertion that must equal the
// credential's principal, never a choice of identity.
func TestAgentAuth_Gate2_AgentIDMustMatchCredentialPrincipal(t *testing.T) {
	t.Parallel()
	store := secretStore{
		authz.AgentTokenSecretName("analyst_agent"): "analyst-token",
		authz.AgentTokenSecretName("summariser"):    "summariser-token",
	}
	client, rec := startAgentPlane(t, store, false)

	_, err := client.ListTools(withMD("x-agent-id", "analyst_agent", "authorization", "Bearer summariser-token"), &pb.ListToolsRequest{})
	wantCode(t, err, codes.Unauthenticated, "summariser's token asserting analyst_agent")

	_, err = client.ListTools(withMD("x-agent-id", "analyst_agent", "authorization", "Bearer wrong"), &pb.ListToolsRequest{})
	wantCode(t, err, codes.Unauthenticated, "wrong token for a known agent")

	_, err = client.ListTools(withMD("x-agent-id", "nobody", "authorization", "Bearer analyst-token"), &pb.ListToolsRequest{})
	wantCode(t, err, codes.Unauthenticated, "unknown agent")

	// The matching pair reaches the handler with the verified principal.
	if _, err := client.ListTools(withMD("x-agent-id", "analyst_agent", "authorization", "Bearer analyst-token"), &pb.ListToolsRequest{}); err != nil {
		t.Fatalf("matching credential refused: %v", err)
	}
	if p := <-rec.seen; p.Kind != domain.PrincipalAgent || p.ID != "analyst_agent" {
		t.Fatalf("principal = %+v, want agent analyst_agent", p)
	}
	stream, err := client.SignalStream(withMD("x-agent-id", "summariser", "authorization", "bearer summariser-token"))
	if err != nil {
		t.Fatalf("stream open: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) == codes.Unauthenticated {
		t.Fatalf("matching credential refused on a stream: %v", err)
	}
	if p := <-rec.seen; p.ID != "summariser" {
		t.Fatalf("stream principal = %+v, want summariser", p)
	}
}

// The operator-gated Orchestrator methods are NOT the agent plane's business:
// the agent interceptor passes them through and the operator interceptor gates
// them, so a dangerous-tool approval can never be made with an agent token.
func TestAgentAuth_OperatorGatedMethodsPassThroughToOperatorGate(t *testing.T) {
	t.Parallel()
	store := secretStore{authz.AgentTokenSecretName("analyst_agent"): "analyst-token"}
	client, _ := startAgentPlane(t, store, false)
	_, err := client.SubmitApprovalDecision(
		withMD("x-agent-id", "analyst_agent", "authorization", "Bearer analyst-token"),
		&pb.ApprovalDecisionRequest{})
	// Refused by the OPERATOR gate: the agent token is not an operator token.
	wantCode(t, err, codes.Unauthenticated, "approval with an agent credential")
}

// D2: with server.insecure_localhost=true the requirement is waived and the
// x-agent-id header is the principal, exactly as before ADR-0130 — the loopback
// benchmark harness depends on this.
func TestAgentAuth_D2_WaiverKeepsHeaderPrincipal(t *testing.T) {
	t.Parallel()
	client, rec := startAgentPlane(t, secretStore{}, true)
	if _, err := client.ListTools(withMD("x-agent-id", "analyst_agent"), &pb.ListToolsRequest{}); err != nil {
		t.Fatalf("waived plane refused a bare x-agent-id: %v", err)
	}
	if p := <-rec.seen; p.ID != "analyst_agent" {
		t.Fatalf("principal = %+v, want analyst_agent from the header under the waiver", p)
	}
	if _, err := client.ListTools(context.Background(), &pb.ListToolsRequest{}); err != nil {
		t.Fatalf("waived plane refused an anonymous call: %v", err)
	}
	if p := <-rec.seen; !p.IsZero() {
		t.Fatalf("anonymous call carried a principal %+v", p)
	}
}

// A declared agent-facing premium service (ADR-0118 D3) is the agent plane
// extended: it takes the same credential, and the principal is seeded here.
func TestAgentAuth_DeclaredPremiumServiceIsAgentPlane(t *testing.T) {
	t.Parallel()
	agentPlane := map[string]bool{"cambrian.premium.substrate.SubstrateRetrieval": true}
	store := secretStore{authz.AgentTokenSecretName("analyst_agent"): "tok"}
	verifier := authz.NewAgentCredentialVerifier(store.resolve, false)
	const method = "/cambrian.premium.substrate.SubstrateRetrieval/Query"

	if _, err := verifier.Authenticate(context.Background(), method, agentPlane); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("declared agent-plane service without a credential: err=%v", err)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", "analyst_agent", "authorization", "Bearer tok"))
	out, err := verifier.Authenticate(ctx, method, agentPlane)
	if err != nil {
		t.Fatal(err)
	}
	if p := domain.PrincipalFromContext(out); p.ID != "analyst_agent" || p.Kind != domain.PrincipalAgent {
		t.Fatalf("principal = %+v", p)
	}
	// An UNDECLARED premium service is the operator plane and is none of the
	// agent verifier's business.
	if _, err := verifier.Authenticate(context.Background(), "/cambrian.premium.substrate.SubstrateLane/Query", agentPlane); err != nil {
		t.Fatalf("operator-plane premium service was gated by the agent verifier: %v", err)
	}
}

// VerifyNamedSecret is the ONE compare; every absence fails closed.
func TestVerifyNamedSecret_FailsClosedOnEveryAbsence(t *testing.T) {
	t.Parallel()
	store := secretStore{"agent:a:token": "tok", "agent:empty:token": ""}
	cases := []struct {
		name, secret, token string
		resolve             func(string) (string, bool)
		want                bool
	}{
		{"match", "agent:a:token", "tok", store.resolve, true},
		{"mismatch", "agent:a:token", "tok2", store.resolve, false},
		{"unknown name", "agent:b:token", "tok", store.resolve, false},
		{"empty token", "agent:a:token", "", store.resolve, false},
		{"empty stored secret never matches empty token", "agent:empty:token", "", store.resolve, false},
		{"empty name", "", "tok", store.resolve, false},
		{"no resolver", "agent:a:token", "tok", nil, false},
	}
	for _, c := range cases {
		if got := authz.VerifyNamedSecret(c.resolve, c.secret, c.token); got != c.want {
			t.Errorf("%s: VerifyNamedSecret = %v, want %v", c.name, got, c.want)
		}
	}
}
