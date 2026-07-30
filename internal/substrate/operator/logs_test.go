package operator

import (
	"context"
	"log/slog"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/pkg/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func logService(t *testing.T) (*Service, *util.LogRing) {
	t.Helper()
	ring := util.NewLogRing(100)
	svc := &Service{}
	svc.SetLogRing(ring)
	return svc, ring
}

func rec(level slog.Level, component, msg string, attrs map[string]any) util.LogRecord {
	return util.LogRecord{
		At: time.Now(), Level: level, Component: component, Message: msg, Attrs: attrs,
	}
}

// The security property. A log line carries whatever the kernel wrote and
// bypasses the access-policy plane, so it is not filtered per principal the way
// a memory read is — a Viewer must not get it.
func TestLogRPCs_AreOperatorOnly(t *testing.T) {
	for _, m := range []string{"QueryLogs", "TailLogs"} {
		if !isOperatorOnly(operatorMethodPrefix + m) {
			t.Fatalf("%s is open to a Viewer", m)
		}
	}
	// The guard has to beat the naming convention, which would otherwise open
	// anything called Query* by its spelling alone.
	if !isOperatorOnly(operatorMethodPrefix + "QueryLogs") {
		t.Fatal("the Query* convention overrode the explicit operator-only list")
	}
	// And the convention still holds for genuine reads.
	if isOperatorOnly(operatorMethodPrefix + "QueryMemory") {
		t.Fatal("an ordinary read became operator-only")
	}
}

// A kernel that retains nothing says so, rather than returning an empty window
// that reads as "the kernel has been silent".
func TestQueryLogs_NoRingIsUnimplemented(t *testing.T) {
	svc := &Service{}
	_, err := svc.QueryLogs(context.Background(), &pb.QueryLogsOpRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("want Unimplemented, got %v", err)
	}
}

func TestQueryLogs_ReturnsRecordsWithTypedAttributes(t *testing.T) {
	svc, ring := logService(t)
	ring.Append(rec(slog.LevelError, "llm", "request rejected",
		map[string]any{"status": 504, "generator": "mimo", "ok": false}))

	resp, err := svc.QueryLogs(context.Background(), &pb.QueryLogsOpRequest{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(resp.GetRecords()) != 1 {
		t.Fatalf("want 1 record, got %d", len(resp.GetRecords()))
	}
	r := resp.GetRecords()[0]
	if r.GetLevel() != "error" || r.GetComponent() != "llm" {
		t.Fatalf("record header wrong: %+v", r)
	}

	byKey := map[string]*pb.LogAttrOp{}
	for _, a := range r.GetAttrs() {
		byKey[a.GetKey()] = a
	}
	// A number stays a number: `status=504` has to be comparable, not text.
	if got := byKey["status"].GetI(); got != 504 {
		t.Fatalf("status lost its type: %+v", byKey["status"])
	}
	if byKey["generator"].GetS() != "mimo" {
		t.Fatalf("string attr wrong: %+v", byKey["generator"])
	}
	if byKey["ok"].GetB() != false || byKey["ok"].GetValue() == nil {
		t.Fatalf("bool attr wrong: %+v", byKey["ok"])
	}
}

// The window rides on every response so a console can state what it cannot show.
func TestQueryLogs_ReportsTheWindowIncludingDrops(t *testing.T) {
	svc := &Service{}
	ring := util.NewLogRing(3)
	svc.SetLogRing(ring)
	for i := 0; i < 8; i++ {
		ring.Append(rec(slog.LevelInfo, "kernel", "line", nil))
	}

	resp, err := svc.QueryLogs(context.Background(), &pb.QueryLogsOpRequest{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	w := resp.GetWindow()
	if w.GetDropped() != 5 {
		t.Fatalf("want 5 dropped, got %d", w.GetDropped())
	}
	if w.GetCount() != 3 || w.GetCapacity() != 3 {
		t.Fatalf("window shape wrong: %+v", w)
	}
	if w.GetBootId() == "" {
		t.Fatal("no boot id; a reader cannot tell a restart happened")
	}
}

func TestQueryLogs_FiltersByLevelComponentTextAndAttribute(t *testing.T) {
	svc, ring := logService(t)
	ring.Append(rec(slog.LevelInfo, "telegram", "ingress started", nil))
	ring.Append(rec(slog.LevelError, "llm", "request rejected", map[string]any{"generator": "mimo"}))
	ring.Append(rec(slog.LevelWarn, "llm", "retrying", map[string]any{"generator": "deepseek"}))

	q := func(f *pb.LogFilterOp) []*pb.LogRecordOp {
		resp, err := svc.QueryLogs(context.Background(), &pb.QueryLogsOpRequest{Filter: f})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		return resp.GetRecords()
	}

	if got := q(&pb.LogFilterOp{LevelAtLeast: "warn"}); len(got) != 2 {
		t.Fatalf("level floor: want 2, got %d", len(got))
	}
	if got := q(&pb.LogFilterOp{Components: []string{"llm"}}); len(got) != 2 {
		t.Fatalf("component: want 2, got %d", len(got))
	}
	if got := q(&pb.LogFilterOp{Contains: "REJECT"}); len(got) != 1 {
		// Case-insensitive: an operator types what they remember, not what was logged.
		t.Fatalf("contains: want 1, got %d", len(got))
	}
	if got := q(&pb.LogFilterOp{AttrEquals: map[string]string{"generator": "mimo"}}); len(got) != 1 {
		t.Fatalf("attr_equals: want 1, got %d", len(got))
	}
	// Terms combine rather than replace each other.
	if got := q(&pb.LogFilterOp{LevelAtLeast: "error", Components: []string{"llm"}}); len(got) != 1 {
		t.Fatalf("combined terms: want 1, got %d", len(got))
	}
}

// When more matches than the limit, an operator wants the RECENT ones — but
// still in reading order.
func TestQueryLogs_LimitKeepsTheNewestInChronologicalOrder(t *testing.T) {
	svc, ring := logService(t)
	for i := 0; i < 10; i++ {
		ring.Append(rec(slog.LevelInfo, "kernel", "line", nil))
	}

	resp, err := svc.QueryLogs(context.Background(), &pb.QueryLogsOpRequest{Limit: 3})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got := resp.GetRecords()
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	if got[0].GetSeq() != 8 || got[2].GetSeq() != 10 {
		t.Fatalf("want the newest three ascending, got %d..%d", got[0].GetSeq(), got[2].GetSeq())
	}
}

func TestQueryLogs_AfterSeqIsTheResumeCursor(t *testing.T) {
	svc, ring := logService(t)
	for i := 0; i < 5; i++ {
		ring.Append(rec(slog.LevelInfo, "kernel", "line", nil))
	}

	resp, err := svc.QueryLogs(context.Background(), &pb.QueryLogsOpRequest{AfterSeq: 3})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(resp.GetRecords()) != 2 {
		t.Fatalf("want 2 records after seq 3, got %d", len(resp.GetRecords()))
	}
}

// A filter that matches almost nothing must not pin the tail's cursor: advancing
// only on matches would re-scan the filtered-out records forever.
func TestTailLogs_CursorAdvancesPastNonMatches(t *testing.T) {
	svc, ring := logService(t)
	ring.Append(rec(slog.LevelInfo, "noise", "a", nil))
	ring.Append(rec(slog.LevelInfo, "noise", "b", nil))
	ring.Append(rec(slog.LevelError, "llm", "boom", nil))

	// Bounded, because a tail with nothing to send legitimately never returns —
	// that is what a tail IS. The deadline is the test's way of saying "long
	// enough to have sent something if it were going to".
	ctx, cancel := context.WithTimeout(context.Background(), 3*tailPollInterval)
	defer cancel()
	stream := &fakeTailStream{ctx: ctx}

	if err := svc.TailLogs(&pb.TailLogsOpRequest{
		AfterSeq: 0, // the live edge
		Filter:   &pb.LogFilterOp{LevelAtLeast: "error"},
	}, stream); err != nil {
		t.Fatalf("tail: %v", err)
	}

	// Starting at the live edge means nothing already retained is replayed — a
	// tail is for what happens next; QueryLogs is for history.
	if len(stream.sent) != 0 {
		t.Fatalf("a fresh tail replayed history: %d records", len(stream.sent))
	}
}

// A tail resumes from a sequence and sends only what matches after it.
func TestTailLogs_ResumesFromASequence(t *testing.T) {
	svc, ring := logService(t)
	ring.Append(rec(slog.LevelInfo, "noise", "a", nil))
	ring.Append(rec(slog.LevelError, "llm", "boom", nil))

	// Cancelled by the stream once it has what it wants, with a deadline so a
	// regression fails the test instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := &fakeTailStream{ctx: ctx, limit: 1, cancel: cancel}

	if err := svc.TailLogs(&pb.TailLogsOpRequest{
		AfterSeq: 1,
		Filter:   &pb.LogFilterOp{LevelAtLeast: "error"},
	}, stream); err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(stream.sent) != 1 || stream.sent[0].GetMessage() != "boom" {
		t.Fatalf("want the one error after seq 1, got %+v", stream.sent)
	}
}

func TestTailLogs_NoRingIsUnimplemented(t *testing.T) {
	svc := &Service{}
	err := svc.TailLogs(&pb.TailLogsOpRequest{}, &fakeTailStream{ctx: context.Background()})
	// Returns immediately: the ring check happens before the poll loop.
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("want Unimplemented, got %v", err)
	}
}

// fakeTailStream captures sends and ends the tail once it has enough.
type fakeTailStream struct {
	pb.OperatorConsole_TailLogsServer
	ctx    context.Context
	cancel context.CancelFunc
	limit  int
	sent   []*pb.LogRecordOp
}

func (f *fakeTailStream) Context() context.Context { return f.ctx }

func (f *fakeTailStream) Send(r *pb.LogRecordOp) error {
	f.sent = append(f.sent, r)
	if f.limit > 0 && len(f.sent) >= f.limit && f.cancel != nil {
		f.cancel()
	}
	return nil
}
