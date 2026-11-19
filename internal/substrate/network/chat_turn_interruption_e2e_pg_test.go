package network

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-2124 end to end, through the real chat TurnService, the real LLM gateway over the
// real llm.Provider (the readiness fault shapes, faultServer), and the real Postgres
// conversation store: a provider credential failure, a provider 429 and a worker killed
// mid-turn, a turn with no model allocated (ENG-2156) and a worker that calls without the
// turn's lease (ENG-2171) each reach the person as one plain sentence and leave an interruption marker
// in conversation_messages; nothing raw leaks.
//
// The worker stands in for the SDK chat worker as it behaves before ENG-2125: it makes
// its model call on the turn's lease over the agent-plane RPC and reports ANY failure as
// Internal with the gateway's text. The killed worker is a real gRPC connection whose
// server is stopped under the in-flight call.

// sdkLikeWorker answers a turn by one GenerateWithTools call on the turn's lease.
type sdkLikeWorker struct {
	s   *Server
	ctx context.Context // carries the worker's x-agent-id
}

func (w sdkLikeWorker) Dispatch(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	resp, err := turn(w.s, w.ctx, domain.LeaseID(h.Context["_session_token_id"]))
	if err != nil {
		// The SDK TraitServicer today: every failure is Internal, raw text included.
		return nil, status.Error(codes.Internal, "INTERNAL: "+status.Convert(err).Message())
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(resp.GetText())}}, nil
}

// relayingWorker is the SDK chat worker since ENG-2125: it relays the gateway's status
// code with the gateway's text.
type relayingWorker struct {
	s   *Server
	ctx context.Context
}

func (w relayingWorker) Dispatch(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	resp, err := turn(w.s, w.ctx, domain.LeaseID(h.Context["_session_token_id"]))
	if err != nil {
		st := status.Convert(err)
		return nil, status.Error(st.Code(), st.Message())
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(resp.GetText())}}, nil
}

// noLeaseWorker is a worker wired wrong: it makes the model call WITHOUT the turn's
// lease and relays the gateway's code (ENG-2171). The person must hear that the
// working session was lost, never that the AI service rejected our credentials.
type noLeaseWorker struct {
	s   *Server
	ctx context.Context
}

func (w noLeaseWorker) Dispatch(_ context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
	resp, err := turn(w.s, w.ctx, "")
	if err != nil {
		st := status.Convert(err)
		return nil, status.Error(st.Code(), st.Message())
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(resp.GetText())}}, nil
}

// killedWorker's turn is in flight on a real gRPC connection when its server dies.
type killedWorker struct{ t *testing.T }

func (k killedWorker) Dispatch(ctx context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		k.t.Fatal(err)
	}
	srv := grpc.NewServer()
	inFlight := make(chan struct{})
	healthpb.RegisterHealthServer(srv, blockingHealth{inFlight: inFlight})
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		k.t.Fatal(err)
	}
	defer conn.Close()
	go func() {
		<-inFlight
		srv.Stop() // the process dies: listener and connections close under the call
	}()
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	return nil, err
}

type blockingHealth struct {
	healthpb.UnimplementedHealthServer
	inFlight chan struct{}
}

func (b blockingHealth) Check(ctx context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	close(b.inFlight)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestPg_E2E_InterruptedChatTurnsArePlainAndMarked(t *testing.T) {
	pool, ctx := pgscratch.Pool(t, "net_chat_interrupt_test", pgscratch.Options{
		Migrations: []string{"0002_conversations.sql", "0003_conversation_policy.sql", "0006_conversation_delivery.sql",
			"0060_conversation_message_sources.sql", "0063_conversation_message_own_sources.sql"},
	})
	store, err := postgres.NewPgConversationStore(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		fault string // faultServer mode; "" with kill
		kill  bool
		// noModel: the turn's allocation names no model and no default is configured;
		// the worker relays the gateway's Unavailable (ENG-2156).
		noModel bool
		// noLease: the worker calls the gateway without the turn's lease (ENG-2171).
		noLease bool
		reason  domain.TurnInterruptReason
		want    corechat.MessageID // the exact sentence, when the case pins it
	}{
		{name: "provider_auth", fault: faultAuth, reason: domain.TurnInterruptProviderAuth},
		{name: "provider_429", fault: fault429, reason: domain.TurnInterruptProviderBusy},
		{name: "worker_killed", kill: true, reason: domain.TurnInterruptAssistantRestarted},
		{name: "no_model_allocated", noModel: true, reason: domain.TurnInterruptNotConfigured, want: corechat.MsgNotConfigured},
		{name: "no_lease", noLease: true, reason: domain.TurnInterruptSessionLost, want: corechat.MsgSessionLost},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			convID := "conv-" + c.name
			if err := store.CreateConversation(ctx, domain.Conversation{
				ID: convID, OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee,
			}); err != nil {
				t.Fatal(err)
			}

			mode := c.fault
			if c.kill {
				mode = "" // the provider is healthy; the worker dies
			}
			m1, _ := faultServer(t, mode)
			s, _, workerCtx := faultGatewayServer(t, m1.URL, "")
			gw := s.LLMGateway.(*SubstrateLLMGateway)

			var worker corechat.Dispatcher = sdkLikeWorker{s: s, ctx: workerCtx}
			if c.kill {
				worker = killedWorker{t: t}
			}
			alloc := domain.StepAllocation{Winner: domain.AgentDefinition{ID: "llm:m1"}}
			if c.noModel {
				worker = relayingWorker{s: s, ctx: workerCtx}
				alloc = domain.StepAllocation{} // no winner, and the gateway has no default model
			}
			if c.noLease {
				worker = noLeaseWorker{s: s, ctx: workerCtx}
			}
			svc := corechat.NewTurnService(store, worker, func(ctx context.Context, limit int, ttl time.Duration) (string, func(), error) {
				lease, err := gw.Acquire(ctx, alloc, limit, ttl)
				if err != nil {
					return "", nil, err
				}
				return string(lease), func() { _, _ = gw.Complete(context.Background(), lease) }, nil
			})
			svc.SetLeaseBinder(gw, "a1") // the worker's agent id (faultGatewayServer's x-agent-id)
			svc.SetLeaseFaultProbe(gw.LeaseFault)

			_, err := svc.Turn(ctx, corechat.TurnRequest{ConversationID: convID, Text: "Who owns SOP-007?", ClientID: "cmd-1"})
			var ti *domain.TurnInterruption
			if !errors.As(err, &ti) {
				t.Fatalf("Turn error = %v, want a *domain.TurnInterruption", err)
			}
			if ti.Reason != c.reason {
				t.Fatalf("reason = %q, want %q (cause: %v)", ti.Reason, c.reason, ti.Cause)
			}
			assertNoRawLeak(t, "the person's message", ti.Message)
			if c.want != "" && ti.Message != corechat.Message(c.want, "en") {
				t.Fatalf("told %q, want %q", ti.Message, corechat.Message(c.want, "en"))
			}
			if c.noLease && strings.Contains(strings.ToLower(ti.Message), "credential") {
				t.Fatalf("told %q: a missing lease is not a provider credential failure", ti.Message)
			}
			if c.noModel && (strings.Contains(strings.ToLower(ti.Message), "allocated") || strings.Contains(strings.ToLower(ti.Message), "restarted")) {
				t.Fatalf("told %q: raw gateway text or the restart sentence", ti.Message)
			}

			msgs, err := store.ListMessages(ctx, convID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 2 || msgs[0].Role != domain.MessageRoleUser || msgs[1].Role != domain.MessageRoleInterruption {
				t.Fatalf("conversation_messages = %+v, want the user message then an interruption marker", msgs)
			}
			if msgs[1].Content != ti.Message {
				t.Fatalf("marker %q, told %q", msgs[1].Content, ti.Message)
			}
			assertNoRawLeak(t, "the stored marker", msgs[1].Content)
		})
	}

	// A kernel killed mid-turn: the next boot's sweep marks the unanswered turn.
	t.Run("kernel_killed", func(t *testing.T) {
		convID := "conv-kernel-killed"
		if err := store.CreateConversation(ctx, domain.Conversation{
			ID: convID, OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendMessage(ctx, domain.Message{ID: "u-cut", ConversationID: convID, Role: domain.MessageRoleUser, Content: "Who owns SOP-007?"}); err != nil {
			t.Fatal(err)
		}
		bootAt := time.Now().Add(time.Second)
		svc := corechat.NewTurnService(store, killedWorker{t: t}, nil)
		if _, err := svc.MarkInterruptedTurns(ctx, bootAt); err != nil {
			t.Fatal(err)
		}
		if n, err := svc.MarkInterruptedTurns(ctx, bootAt); err != nil || n != 0 {
			t.Fatalf("second sweep marked %d (%v), want 0", n, err)
		}
		msgs, _ := store.ListMessages(ctx, convID, 0, 0)
		if len(msgs) != 2 || msgs[1].Role != domain.MessageRoleInterruption || !strings.Contains(msgs[1].Content, "restarted") {
			t.Fatalf("conversation_messages = %+v, want the restarted marker", msgs)
		}
		assertNoRawLeak(t, "the boot marker", msgs[1].Content)
	})
}

func assertNoRawLeak(t *testing.T, what, text string) {
	t.Helper()
	if strings.TrimSpace(text) == "" {
		t.Fatalf("%s is empty", what)
	}
	low := strings.ToLower(text)
	for _, bad := range []string{"http", "{", ".sock", "unix", "127.0.0.1", "rpc error", "internal:", "openai", "eof"} {
		if strings.Contains(low, bad) {
			t.Fatalf("%s %q leaks %q", what, text, bad)
		}
	}
}
