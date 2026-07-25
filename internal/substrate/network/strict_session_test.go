package network

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/session"
)

// strictServer is a minimal Server with a real SessionManager and strict mode on.
func strictServer(t *testing.T, strict bool) (*Server, *session.SessionManager) {
	t.Helper()
	s := minimalServer(t)
	cfg := s.ExecCfg
	cfg.RequireExplicitSession = strict
	// Stop after planning: these tests are about the SESSION GATE, not execution, and this
	// Server has no auctioneer to dispatch to.
	cfg.PlanPreviewOnly = true
	s.ExecCfg = cfg
	mgr := session.New(newMemSessionStore())
	s.SessionMgr = mgr
	return s, mgr
}

func execWith(s *Server, md metadata.MD) error {
	ctx := context.Background()
	if md != nil {
		ctx = metadata.NewIncomingContext(ctx, md)
	}
	_, err := s.Execute(ctx, &pb.Handoff{Id: "t", Payload: &pb.Object{Data: []byte("do it")}})
	return err
}

// Strict mode refuses to invent a session. Implicit creation cannot tell "new work" from "a
// continuation" from "a replay", so it is wrong for at least one of them — and it is why
// every Execute minted a session nothing ever closed.
func TestExecute_StrictMode_RejectsMissingSession(t *testing.T) {
	s, _ := strictServer(t, true)

	err := execWith(s, nil)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument with no session, got %v", err)
	}
}

// A session ID the kernel never issued is a not-found, never a silent create.
func TestExecute_StrictMode_RejectsUnknownSession(t *testing.T) {
	s, _ := strictServer(t, true)

	err := execWith(s, metadata.Pairs(sessionHeader, "never-opened"))
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for an unknown session, got %v", err)
	}
}

// A completed session is terminal: work cannot be appended to sealed work.
func TestExecute_StrictMode_RejectsCompletedSession(t *testing.T) {
	s, mgr := strictServer(t, true)
	ctx := context.Background()
	ses, err := mgr.CreateSession(ctx, "goal", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := mgr.TransitionStatus(ctx, ses.ID, domain.SessionCompleted); err != nil {
		t.Fatalf("complete: %v", err)
	}

	err = execWith(s, metadata.Pairs(sessionHeader, string(ses.ID)))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for a completed session, got %v", err)
	}
}

// An open session is accepted — strict mode rejects only the ambiguous cases. Execution
// itself is short-circuited by plan_preview_only, so what this asserts is precisely that the
// request got PAST the session gate.
func TestExecute_StrictMode_AcceptsOpenSession(t *testing.T) {
	s, mgr := strictServer(t, true)
	ses, err := mgr.CreateSession(context.Background(), "goal", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := execWith(s, metadata.Pairs(sessionHeader, string(ses.ID))); err != nil {
		t.Fatalf("an open session must pass the session gate, got %v", err)
	}
}

// Default (lenient) mode is unchanged: no session presented still runs, so existing clients
// and benchmarks keep working until they are migrated.
func TestExecute_LenientMode_StillOpensImplicitly(t *testing.T) {
	s, mgr := strictServer(t, false)

	if err := execWith(s, nil); err != nil {
		t.Fatalf("lenient mode must not require a session, got %v", err)
	}
	sessions, lerr := mgr.ListSessions(context.Background(), domain.SessionActive)
	if lerr != nil {
		t.Fatalf("list: %v", lerr)
	}
	if len(sessions) != 1 {
		t.Errorf("lenient mode should have opened exactly one session, got %d", len(sessions))
	}
}
