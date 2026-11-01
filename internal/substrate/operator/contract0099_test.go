package operator_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// ── Logout (ADR-0130 D5, contract 0099) ─────────────────────────────────────

// logoutVia runs Logout behind the production auth interceptor, as a client does.
func logoutVia(t *testing.T, idp operator.OperatorIdentity, svc *operator.Service, token string) error {
	t.Helper()
	_, err := operator.UnaryAuthInterceptor(idp, nil)(ctxWithToken(token), &pb.LogoutRequest{},
		&grpc.UnaryServerInfo{FullMethod: "/cambrian.OperatorConsole/Logout"},
		func(ctx context.Context, req any) (any, error) { return svc.Logout(ctx, req.(*pb.LogoutRequest)) })
	return err
}

// Logout revokes exactly the caller's token: it is refused afterwards, on every
// RPC, while another session of the same user is untouched.
func TestLogout_RevokesTheCallersToken(t *testing.T) {
	t.Parallel()
	idp := newIDP()
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetIdentity(idp)
	tok := login(t, idp, "op", "pw")
	other := login(t, idp, "op", "pw")

	if err := logoutVia(t, idp, svc, tok); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := callUnary(idp, ctxWithToken(tok), "/cambrian.OperatorConsole/Snapshot"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("revoked token: got %v, want Unauthenticated", err)
	}
	if err := logoutVia(t, idp, svc, tok); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("second Logout with the revoked token: got %v, want Unauthenticated", err)
	}
	if called, err := callUnary(idp, ctxWithToken(other), "/cambrian.OperatorConsole/Snapshot"); err != nil || !called {
		t.Fatalf("another session must survive: called=%v err=%v", called, err)
	}
}

// A Viewer may end its own session: Logout is not a mutating command.
func TestLogout_ViewerMayLogOut(t *testing.T) {
	t.Parallel()
	idp := newIDP()
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetIdentity(idp)
	tok := login(t, idp, "viewer", "pw")
	if err := logoutVia(t, idp, svc, tok); err != nil {
		t.Fatalf("viewer Logout: %v", err)
	}
	if _, _, err := idp.Resolve(tok); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("viewer token survived Logout: %v", err)
	}
}

func TestLogout_UnconfiguredIdentityIsUnimplemented(t *testing.T) {
	t.Parallel()
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	if _, err := svc.Logout(ctxWithToken("x"), &pb.LogoutRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("got %v, want Unimplemented", err)
	}
}

// ── Event time on ingest (ADR-0134 D1) ──────────────────────────────────────

func TestIngestMemory_OccurredAtAndPrecisionReachTheIngestor(t *testing.T) {
	t.Parallel()
	svc, _, ing := newIngestService()
	stated := time.Date(2023, 5, 20, 0, 0, 0, 0, time.UTC)
	if _, err := svc.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{
		CommandId: "e1", Reason: "r", Text: "x", OccurredAt: timestamppb.New(stated), TimePrecision: "day",
	}); err != nil {
		t.Fatal(err)
	}
	et := ing.last.EventTime
	if !et.EventTime.Equal(stated) || et.Source != domain.TimeSourceExplicit || et.Precision != domain.TimePrecisionDay {
		t.Fatalf("event time = %+v", et)
	}
}

func TestIngestMemory_NoTimeIsIngestDefault(t *testing.T) {
	t.Parallel()
	svc, _, ing := newIngestService()
	if _, err := svc.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{CommandId: "e2", Reason: "r", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if ing.last.EventTime.Source != domain.TimeSourceIngestDefault || ing.last.EventTime.EventTime.IsZero() {
		t.Fatalf("event time = %+v", ing.last.EventTime)
	}
}

// parts is a third body lane; the ingestor gets the assembled body and the span.
func TestIngestMemory_PartsLane(t *testing.T) {
	t.Parallel()
	svc, _, ing := newIngestService()
	a := time.Date(2023, 5, 20, 7, 0, 0, 0, time.UTC)
	if _, err := svc.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{CommandId: "e3", Reason: "r", Parts: []*pb.MemoryPartOp{
		{Text: "user: hi", OccurredAt: timestamppb.New(a)},
		{Text: "assistant: hello", OccurredAt: timestamppb.New(a.Add(time.Hour))},
	}}); err != nil {
		t.Fatal(err)
	}
	et := ing.last.EventTime
	if et.Body != "user: hi\n\nassistant: hello" || !et.EventTime.Equal(a) || !et.EventTimeEnd.Equal(a.Add(time.Hour)) {
		t.Fatalf("event time = %+v", et)
	}
}

func TestIngestMemory_EventTimeValidation(t *testing.T) {
	t.Parallel()
	svc, _, ing := newIngestService()
	cases := map[string]*pb.IngestMemoryOpRequest{
		"text and parts":    {Text: "x", Parts: []*pb.MemoryPartOp{{Text: "y"}}},
		"content and parts": {Content: []byte("x"), Filename: "a.pdf", Parts: []*pb.MemoryPartOp{{Text: "y"}}},
		"bad precision":     {Text: "x", TimePrecision: "fortnight"},
		"invalid ts":        {Text: "x", OccurredAt: &timestamppb.Timestamp{Nanos: -1}},
	}
	for name, req := range cases {
		req.CommandId, req.Reason = "v-"+name, "r"
		if _, err := svc.IngestMemory(opCtx(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: got %v, want InvalidArgument", name, err)
		}
	}
	if ing.calls != 0 {
		t.Fatalf("a rejected request must not ingest, calls=%d", ing.calls)
	}
}

// The configured tolerance is honoured (SetEventTimeFutureTolerance, wired from
// execution.ingestion.event_time_future_tolerance_ms in app.go).
func TestIngestMemory_FutureToleranceIsConfigurable(t *testing.T) {
	t.Parallel()
	svc, _, ing := newIngestService()
	svc.SetEventTimeFutureTolerance(2 * time.Hour)
	ahead := time.Now().Add(time.Hour)
	if _, err := svc.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{CommandId: "f1", Reason: "r", Text: "x", OccurredAt: timestamppb.New(ahead)}); err != nil {
		t.Fatal(err)
	}
	if ing.last.EventTime.Source != domain.TimeSourceExplicit {
		t.Fatalf("1 h ahead with a 2 h tolerance: %q", ing.last.EventTime.Source)
	}
	svc.SetEventTimeFutureTolerance(0)
	if _, err := svc.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{CommandId: "f2", Reason: "r", Text: "x", OccurredAt: timestamppb.New(ahead)}); err != nil {
		t.Fatal(err)
	}
	if ing.last.EventTime.Source != domain.TimeSourceClampedFuture {
		t.Fatalf("1 h ahead with no tolerance: %q", ing.last.EventTime.Source)
	}
}

// ── Event time on query and answer (ADR-0134 D4) ────────────────────────────

type ctxMemory struct {
	fakeMemory
	gotCtx context.Context
}

func (c *ctxMemory) SearchSystem(ctx context.Context, q string) ([]domain.SearchResult, error) {
	c.gotCtx = ctx
	return c.fakeMemory.SearchSystem(ctx, q)
}

type ctxAnswerer struct {
	evidence []domain.SearchResult
	gotCtx   context.Context
}

func (c *ctxAnswerer) AnswerSystem(ctx context.Context, _ string) (string, string, []domain.SearchResult, error) {
	c.gotCtx = ctx
	return "answer", "a [1]", c.evidence, nil
}

var stampedHit = domain.SearchResult{Document: domain.Document{ID: "c1", Text: "t", EventStamp: domain.EventStamp{
	EventTime:  time.Date(2023, 5, 20, 7, 0, 0, 0, time.UTC),
	TimeSource: domain.TimeSourceExplicit, TimePrecision: domain.TimePrecisionMinute,
}}}

func TestQueryMemory_EventTimeParametersAndHitTimes(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newCommandService()
	mem := &ctxMemory{fakeMemory: fakeMemory{results: []domain.SearchResult{stampedHit}}}
	svc.SetReadSources(nil, nil, mem)
	ref := time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)

	resp, err := svc.QueryMemory(opCtx(), &pb.QueryMemoryRequest{Query: "q", ReferenceTime: timestamppb.New(ref)})
	if err != nil {
		t.Fatal(err)
	}
	if q, ok := domain.EventTimeQueryFromContext(mem.gotCtx); !ok || !q.ReferenceTime.Equal(ref) {
		t.Fatalf("reference time did not reach the search: %+v", q)
	}
	h := resp.GetResults()[0]
	if h.GetEventTime().AsTime().Hour() != 7 || h.GetTimeSource() != "explicit" || h.GetTimePrecision() != "minute" {
		t.Fatalf("hit time = %v %q %q", h.GetEventTime(), h.GetTimeSource(), h.GetTimePrecision())
	}
	if _, err := svc.QueryMemory(opCtx(), &pb.QueryMemoryRequest{Query: "q", EventBefore: timestamppb.New(ref)}); err != nil {
		t.Fatalf("a window must be accepted now that it is applied: %v", err)
	}
	if q, ok := domain.EventTimeQueryFromContext(mem.gotCtx); !ok || !q.EventBefore.Equal(ref) {
		t.Fatalf("window did not reach the search: %+v", q)
	}
}

func TestAnswerMemory_EventTimeParametersAndCitationTimes(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newCommandService()
	ans := &ctxAnswerer{evidence: []domain.SearchResult{stampedHit}}
	svc.SetMemoryAnswerer(ans)
	ref := time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)

	resp, err := svc.AnswerMemory(opCtx(), &pb.AnswerMemoryRequest{Query: "q", ReferenceTime: timestamppb.New(ref)})
	if err != nil {
		t.Fatal(err)
	}
	if q, ok := domain.EventTimeQueryFromContext(ans.gotCtx); !ok || !q.ReferenceTime.Equal(ref) {
		t.Fatalf("reference time did not reach the answer lane: %+v", q)
	}
	c := resp.GetCitations()[0]
	if c.GetEventTime() == nil || c.GetTimeSource() != "explicit" || c.GetTimePrecision() != "minute" {
		t.Fatalf("citation time = %v %q %q", c.GetEventTime(), c.GetTimeSource(), c.GetTimePrecision())
	}
	if _, err := svc.AnswerMemory(opCtx(), &pb.AnswerMemoryRequest{
		Query: "q", EventAfter: timestamppb.New(ref), EventBefore: timestamppb.New(ref),
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty window: got %v, want InvalidArgument", err)
	}
	if _, err := svc.AnswerMemory(opCtx(), &pb.AnswerMemoryRequest{Query: "q", EventAfter: timestamppb.New(ref)}); err != nil {
		t.Fatalf("a window must be accepted now that it is applied: %v", err)
	}
	if q, ok := domain.EventTimeQueryFromContext(ans.gotCtx); !ok || !q.EventAfter.Equal(ref) {
		t.Fatalf("window did not reach the answer lane: %+v", q)
	}
}
