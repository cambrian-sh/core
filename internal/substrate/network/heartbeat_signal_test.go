package network

import (
	"context"
	"sync"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

type beatCall struct {
	instance, stream, agent string
	busy                    bool
}

type recordingSink struct {
	mu    sync.Mutex
	calls []beatCall
}

func (r *recordingSink) RecordDaemonHeartbeat(instanceID, streamID, agentID string, busy bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, beatCall{instanceID, streamID, agentID, busy})
	return true
}

type countingReceiver struct {
	mu      sync.Mutex
	signals []domain.Signal
}

func (c *countingReceiver) OnSignal(_ context.Context, s domain.Signal) error {
	c.mu.Lock()
	c.signals = append(c.signals, s)
	c.mu.Unlock()
	return nil
}

func beat(stream string, busy bool) *pb.Handoff {
	b := "false"
	if busy {
		b = "true"
	}
	return &pb.Handoff{FromAgent: "declared_name", Metadata: map[string]string{
		"_signal_type": HeartbeatSignalType, "_stream_id": stream, "_heartbeat_busy": b}}
}

// ADR-0070 D3: a heartbeat is consumed before signal routing — the sink records it with
// the token-stamped instance and the AUTHENTICATED agent, and it never reaches the
// SignalReceiver. Ordinary signals on the same stream still do.
func TestSignalStream_HeartbeatIsConsumedNotRouted(t *testing.T) {
	sink := &recordingSink{}
	recv := &countingReceiver{}
	s := &Server{SignalReceiver: recv, Heartbeats: sink}
	ctx := domain.WithAgentInstance(
		domain.WithPrincipal(context.Background(), domain.AgentPrincipal("chat_agent")), "inst-7")
	stream := &fakeSignalStream{ctx: ctx, in: []*pb.Handoff{
		beat("chat-chat_agent-0", true),
		{FromAgent: "chat_agent", Metadata: map[string]string{"_signal_type": "prices"},
			Payload: &pb.Object{Data: []byte("tick")}},
		beat("chat-chat_agent-0", false),
	}}
	_ = s.SignalStream(stream)

	if len(recv.signals) != 1 || recv.signals[0].StreamID != "prices" {
		t.Fatalf("receiver saw %+v, want only the ordinary signal", recv.signals)
	}
	want := []beatCall{
		{"inst-7", "chat-chat_agent-0", "chat_agent", true},
		{"inst-7", "chat-chat_agent-0", "chat_agent", false},
	}
	if len(sink.calls) != len(want) {
		t.Fatalf("sink calls %+v, want %+v", sink.calls, want)
	}
	for i := range want {
		if sink.calls[i] != want[i] {
			t.Fatalf("beat %d recorded as %+v, want %+v", i, sink.calls[i], want[i])
		}
	}
}

// Liveness off (no sink): a beat is still swallowed, never routed as a signal.
func TestSignalStream_HeartbeatWithoutSinkIsDropped(t *testing.T) {
	recv := &countingReceiver{}
	s := &Server{SignalReceiver: recv}
	_ = s.SignalStream(&fakeSignalStream{ctx: context.Background(), in: []*pb.Handoff{beat("s", false)}})
	if len(recv.signals) != 0 {
		t.Fatalf("a heartbeat reached the receiver: %+v", recv.signals)
	}
}

// A non-agent principal cannot beat for anyone.
func TestSignalStream_HeartbeatFromNonAgentIsIgnored(t *testing.T) {
	sink := &recordingSink{}
	s := &Server{SignalReceiver: &countingReceiver{}, Heartbeats: sink}
	ctx := domain.WithPrincipal(context.Background(), domain.PrincipalRef{ID: "u1", Kind: domain.PrincipalUser})
	_ = s.SignalStream(&fakeSignalStream{ctx: ctx, in: []*pb.Handoff{beat("s", false)}})
	if len(sink.calls) != 0 {
		t.Fatalf("a non-agent principal's beat was recorded: %+v", sink.calls)
	}
}
