package network

import (
	"context"
	"fmt"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// streamKeyedSyncProcessor behaves like premium's ReactiveEngine.ProcessSync: sync
// pipelines are keyed to a conversation stream, so a signal with no stream matches
// nothing and is an error ("no sync WatchConfig found for stream \"\"").
type streamKeyedSyncProcessor struct {
	calls int
}

func (p *streamKeyedSyncProcessor) OnSignal(context.Context, domain.Signal) error { return nil }

func (p *streamKeyedSyncProcessor) ProcessSync(_ context.Context, sig domain.Signal) (*domain.Handoff, error) {
	p.calls++
	if sig.StreamID == "" {
		return nil, fmt.Errorf("no sync WatchConfig found for stream %q", sig.StreamID)
	}
	return &domain.Handoff{Payload: &domain.Payload{Type: "text", Data: []byte("chat answer")}}, nil
}

// T-20261104-agent-plane-execute-with-an-unrecognised-raw-x: an agent-plane Execute the
// router classifies CHAT, carrying no _conversation_id, used to reach ProcessSync with an
// empty stream and fail with UNKNOWN — with or without an x-session-id header, because the
// CHAT branch runs before any session is resolved. There is no conversation to answer in,
// so the turn runs on the PLAN path and opens a fresh session. A raw foreign x-session-id
// is still NOT adopted (ADR-0139 D8).
func TestExecute_ChatWithoutConversation_RunsAsFreshSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		rawHeader bool
	}{
		{"no session header", false},
		{"raw foreign x-session-id", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, mgr := strictServer(t, false)
			proc := &streamKeyedSyncProcessor{}
			s.Router = &stubChatRouter{}
			s.SignalReceiver = proc

			opSes, err := mgr.CreateSession(context.Background(), "operator session", "")
			if err != nil {
				t.Fatalf("create operator session: %v", err)
			}
			var pairs []string
			if tc.rawHeader {
				pairs = []string{sessionHeader, string(opSes.ID)}
			}
			ctx := agentPlaneCtx("agent-a", pairs...)

			if _, err := s.Execute(ctx, &pb.Handoff{Id: "t",
				Payload: &pb.Object{Type: "task", Data: []byte("reply with the single word ok.")}}); err != nil {
				t.Fatalf("Execute must not fail for a CHAT turn with no conversation: %v", err)
			}
			if proc.calls != 0 {
				t.Errorf("ProcessSync called %d times with no conversation to answer in", proc.calls)
			}

			all, err := mgr.ListSessions(context.Background(), "")
			if err != nil {
				t.Fatalf("list sessions: %v", err)
			}
			if len(all) != 2 {
				t.Fatalf("want the operator session plus one fresh session, got %d sessions", len(all))
			}
			for _, ses := range all {
				if ses.ID != opSes.ID && ses.Goal == opSes.Goal {
					t.Errorf("fresh session %s copied the operator session's goal", ses.ID)
				}
			}
			after, err := mgr.GetSession(context.Background(), opSes.ID)
			if err != nil || after == nil {
				t.Fatalf("operator session lost: %v", err)
			}
			if !after.UpdatedAt.Equal(opSes.UpdatedAt) || after.Goal != opSes.Goal {
				t.Errorf("operator session was touched by the agent call: before=%+v after=%+v", opSes, after)
			}
		})
	}
}

// A CHAT turn that does carry a conversation still answers through ProcessSync.
func TestExecute_ChatWithConversation_StillUsesProcessSync(t *testing.T) {
	t.Parallel()
	s, _ := strictServer(t, false)
	proc := &streamKeyedSyncProcessor{}
	s.Router = &stubChatRouter{}
	s.SignalReceiver = proc

	resp, err := s.Execute(agentPlaneCtx("agent-a"), &pb.Handoff{Id: "t",
		Payload:  &pb.Object{Data: []byte("hi")},
		Metadata: map[string]string{"_conversation_id": "conv-1"}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if proc.calls != 1 || string(resp.GetPayload().GetData()) != "chat answer" {
		t.Fatalf("want the ProcessSync answer, got calls=%d payload=%q", proc.calls, resp.GetPayload().GetData())
	}
}
