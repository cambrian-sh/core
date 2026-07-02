package operator_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/substrate/operator"
)

// fakeStream is an in-memory pb.OperatorConsole_StreamEventsServer. Embedding
// grpc.ServerStream satisfies the unused methods; only Send/Context are used.
type fakeStream struct {
	grpc.ServerStream
	ctx  context.Context
	recv chan *pb.OperatorEvent
}

func (f *fakeStream) Send(e *pb.OperatorEvent) error { f.recv <- e; return nil }
func (f *fakeStream) Context() context.Context       { return f.ctx }

func recvOne(t *testing.T, ch chan *pb.OperatorEvent) *pb.OperatorEvent {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an OperatorEvent")
		return nil
	}
}

// StreamEvents drains the backlog, maps domain events to the wire envelope,
// delivers live events as they are emitted, and exits on context cancellation.
func TestService_StreamEventsDeliversBacklogAndLive(t *testing.T) {
	feed := operator.NewSpool(operator.SpoolConfig{})
	svc := operator.NewService(feed)

	// Backlog emitted before the client subscribes.
	feed.Emit(domain.AuctionEventPayload{TaskID: "t1", Status: "completed", WinnerID: "w1"})
	feed.Emit(domain.AgentReadyEvent{AgentID: "a1", TrustScore: 0.9})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeStream{ctx: ctx, recv: make(chan *pb.OperatorEvent, 8)}

	done := make(chan error, 1)
	go func() { done <- svc.StreamEvents(&pb.SubscribeRequest{LastSeq: 0}, stream) }()

	// Backlog: the auction event, mapped.
	e1 := recvOne(t, stream.recv)
	if e1.GetSeq() != 1 || e1.GetAuction() == nil || e1.GetAuction().GetWinnerId() != "w1" {
		t.Fatalf("event 1: expected mapped auction at seq 1, got %+v", e1)
	}
	// Backlog: the agent-ready event, mapped.
	e2 := recvOne(t, stream.recv)
	if e2.GetSeq() != 2 || e2.GetAgentReady() == nil || e2.GetAgentReady().GetAgentId() != "a1" {
		t.Fatalf("event 2: expected mapped agent_ready at seq 2, got %+v", e2)
	}

	// A live event emitted after subscription is delivered without a refresh.
	feed.Emit(domain.SessionDormantEvent{SessionID: "s9"})
	e3 := recvOne(t, stream.recv)
	if e3.GetSeq() != 3 || e3.GetSessionDormant() == nil || e3.GetSessionId() != "s9" {
		t.Fatalf("event 3: expected live session_dormant at seq 3 with session_id, got %+v", e3)
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("StreamEvents should return the context error on cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamEvents did not exit after context cancellation")
	}
}

// An out-of-window cursor yields an explicit RESYNC_REQUIRED control event
// (no silent gap), then resumes live from the head. ADR-0047 0047-02.
func TestService_StreamEventsResyncOnStaleCursor(t *testing.T) {
	feed := operator.NewSpool(operator.SpoolConfig{MaxEvents: 2})
	svc := operator.NewService(feed)
	for i := 0; i < 5; i++ { // events 1..5; only 4,5 retained
		feed.Emit(domain.AgentReadyEvent{})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeStream{ctx: ctx, recv: make(chan *pb.OperatorEvent, 8)}
	go func() { _ = svc.StreamEvents(&pb.SubscribeRequest{LastSeq: 0}, stream) }()

	first := recvOne(t, stream.recv)
	if first.GetResync() == nil {
		t.Fatalf("stale cursor 0 should receive RESYNC_REQUIRED first, got %+v", first)
	}
	if first.GetResync().GetLatestSeq() != 5 {
		t.Fatalf("resync should carry the head seq 5, got %d", first.GetResync().GetLatestSeq())
	}

	// After resync, a live event is delivered from the head onward.
	feed.Emit(domain.AgentReadyEvent{}) // seq 6
	live := recvOne(t, stream.recv)
	if live.GetResync() != nil || live.GetSeq() != 6 {
		t.Fatalf("expected live event at seq 6 after resync, got %+v", live)
	}
}

// An in-window cursor resumes with a gap-free replay and never resyncs.
func TestService_StreamEventsInWindowReplayNoResync(t *testing.T) {
	feed := operator.NewSpool(operator.SpoolConfig{})
	svc := operator.NewService(feed)
	for i := 0; i < 4; i++ {
		feed.Emit(domain.AgentReadyEvent{})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeStream{ctx: ctx, recv: make(chan *pb.OperatorEvent, 8)}
	go func() { _ = svc.StreamEvents(&pb.SubscribeRequest{LastSeq: 2}, stream) }()

	for _, want := range []uint64{3, 4} {
		e := recvOne(t, stream.recv)
		if e.GetResync() != nil {
			t.Fatal("in-window cursor must not resync")
		}
		if e.GetSeq() != want {
			t.Fatalf("expected gap-free replay seq %d, got %d", want, e.GetSeq())
		}
	}
}

// Token chunks are delivered live via StreamEvents but never enter the replay
// ring and consume no seq (ADR-0047 D12).
func TestService_TokenChunksAreLiveOnly(t *testing.T) {
	feed := operator.NewSpool(operator.SpoolConfig{})
	svc := operator.NewService(feed)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeStream{ctx: ctx, recv: make(chan *pb.OperatorEvent, 8)}
	go func() { _ = svc.StreamEvents(&pb.SubscribeRequest{LastSeq: 0}, stream) }()

	// Give the stream a moment to subscribe to the ephemeral lane.
	time.Sleep(50 * time.Millisecond)
	feed.EmitEphemeral(domain.TokenChunkEvent{SessionID: "s1", StepIndex: 2, Text: "hel"})

	e := recvOne(t, stream.recv)
	if e.GetToken() == nil || e.GetToken().GetText() != "hel" || e.GetSeq() != 0 {
		t.Fatalf("expected live token chunk at seq 0, got %+v", e)
	}
	// Not retained: ReadFrom finds nothing and the head did not advance.
	if got, _ := feed.ReadFrom(0); len(got) != 0 {
		t.Fatalf("token chunk must not be replayable, found %d retained", len(got))
	}
	if feed.Head() != 0 {
		t.Fatalf("token chunk must not consume a seq, head=%d", feed.Head())
	}
}

// SubscribeBridge wires EventBus publications into the spool, including the
// ADR-0047 D3 event types (memory/HITL/verifier/llm-health).
func TestSubscribeBridge_PublishedEventsReachTheFeed(t *testing.T) {
	bus := domain.NewInMemoryEventBus()
	feed := operator.NewSpool(operator.SpoolConfig{})
	operator.SubscribeBridge(bus, feed)

	_ = bus.Publish(domain.AuctionEventPayload{TaskID: "t1"})
	_ = bus.Publish(domain.AgentReadyEvent{AgentID: "a1"})
	_ = bus.Publish(domain.MemoryWrittenEvent{DocID: "d1", SessionID: "s1"})
	_ = bus.Publish(domain.HITLRaisedEvent{InterventionID: "i1", SessionID: "s1", IsDestructive: true})
	_ = bus.Publish(domain.VerifierRoundEvent{TaskID: "t1", QualityScore: 0.8})
	_ = bus.Publish(domain.LLMHealthEvent{ModelID: "m1", State: "open"})

	got, resync := feed.ReadFrom(0)
	if resync {
		t.Fatal("unexpected resync")
	}
	if len(got) != 6 {
		t.Fatalf("expected 6 events to reach the feed, got %d", len(got))
	}
}

// The four ADR-0047 D3 event types map to their wire payloads end-to-end.
func TestService_StreamEventsMapsNewEventTypes(t *testing.T) {
	feed := operator.NewSpool(operator.SpoolConfig{})
	svc := operator.NewService(feed)
	feed.Emit(domain.MemoryWrittenEvent{DocID: "d1", DocType: "mnemonic_fact", SessionID: "s1"})
	feed.Emit(domain.HITLRaisedEvent{InterventionID: "i1", SessionID: "s1", IsDestructive: true})
	feed.Emit(domain.VerifierRoundEvent{TaskID: "t1", QualityScore: 0.8, WinnerAgent: "w"})
	feed.Emit(domain.LLMHealthEvent{ModelID: "m1", State: "open", Reason: "timeout"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeStream{ctx: ctx, recv: make(chan *pb.OperatorEvent, 8)}
	go func() { _ = svc.StreamEvents(&pb.SubscribeRequest{LastSeq: 0}, stream) }()

	if e := recvOne(t, stream.recv); e.GetMemoryWritten().GetDocId() != "d1" {
		t.Fatalf("expected memory_written d1, got %+v", e)
	}
	if e := recvOne(t, stream.recv); !e.GetHitlRaised().GetIsDestructive() || e.GetSessionId() != "s1" {
		t.Fatalf("expected destructive hitl_raised in session s1, got %+v", e)
	}
	if e := recvOne(t, stream.recv); e.GetVerifierRound().GetWinnerAgent() != "w" {
		t.Fatalf("expected verifier_round winner w, got %+v", e)
	}
	if e := recvOne(t, stream.recv); e.GetLlmHealth().GetState() != "open" {
		t.Fatalf("expected llm_health open, got %+v", e)
	}
}
