package network

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/session"
	"google.golang.org/grpc/metadata"
)

type turnSink struct{ got []domain.ProgressUpdate }

func (s *turnSink) Progress(_ context.Context, u domain.ProgressUpdate) { s.got = append(s.got, u) }

// ENG-1539: an agent step's progress names the turn it works for — the chat turn
// lease's OriginMessageID, or, for a delegated step's lease, its session's
// ADR-0084 link — so the progress sink can drop it after that turn's Final.
func TestReportProgress_NamesTheTurnOfTheCallersLease(t *testing.T) {
	s, gw := newLeaseServer(t)
	mgr := session.New(newMemSessionStore())
	s.SessionMgr = mgr
	sink := &turnSink{}
	s.Progress = sink
	ctx := context.Background()
	lease := func(b domain.LeaseBinding) context.Context {
		t.Helper()
		l, err := gw.Acquire(ctx, domain.StepAllocation{Winner: domain.AgentDefinition{ID: "llm:m1"}}, 4096, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		gw.BindLease(l, b)
		return metadata.NewIncomingContext(ctx, metadata.Pairs(leaseHeader, string(l)))
	}

	// The chat turn's own lease.
	s.reportProgress(lease(domain.LeaseBinding{ConversationID: "c1", OriginMessageID: "m-1", AgentID: "chat"}), domain.PhaseWorking)
	// A delegated step: the lease names a session, the session links the turn.
	ses, err := mgr.CreateScopedSession(ctx, "goal", "", domain.TagSet{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SaveConversationLink(ctx, ses.ID, "c1", "m-2"); err != nil {
		t.Fatal(err)
	}
	s.reportProgress(lease(domain.LeaseBinding{SessionID: ses.ID, RunID: "r1", AgentID: "worker"}), domain.PhaseWorking)

	if len(sink.got) != 2 {
		t.Fatalf("snapshots %+v, want 2", sink.got)
	}
	if u := sink.got[0]; u.ConversationID != "c1" || u.TurnID != "m-1" {
		t.Fatalf("chat lease snapshot %+v, want c1/m-1", u)
	}
	if u := sink.got[1]; u.ConversationID != "c1" || u.TurnID != "m-2" {
		t.Fatalf("delegated step snapshot %+v, want c1/m-2", u)
	}
}
