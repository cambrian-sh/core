package network

import (
	"context"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ADR-0134 D1 (contract 0099): an agent-plane ingest's occurred_at becomes the
// document's event time, labelled explicit, with the stated precision.
func TestIngestMemory_OccurredAtBecomesEventTime(t *testing.T) {
	t.Parallel()
	p := &fakeIngestionProcessor{docID: "source_doc:x"}
	s := &Server{IngestionProcessor: p}
	stated := time.Date(2023, 5, 20, 7, 51, 0, 0, time.UTC)
	if _, err := s.IngestMemory(agentCtx("analyst"), &pb.IngestMemoryRequest{
		Text: "body", OccurredAt: timestamppb.New(stated), TimePrecision: "minute",
	}); err != nil {
		t.Fatal(err)
	}
	d := p.gotDoc
	if !d.Timestamp.Equal(stated) || d.TimeSource != domain.TimeSourceExplicit || d.TimePrecision != domain.TimePrecisionMinute {
		t.Fatalf("doc time = %v %q %q", d.Timestamp, d.TimeSource, d.TimePrecision)
	}
}

// No time stated: dated by the ingest clock and labelled as such, never undated.
func TestIngestMemory_NoTimeIsIngestDefault(t *testing.T) {
	t.Parallel()
	p := &fakeIngestionProcessor{docID: "source_doc:x"}
	s := &Server{IngestionProcessor: p}
	before := time.Now().UTC()
	if _, err := s.IngestMemory(agentCtx("analyst"), &pb.IngestMemoryRequest{Text: "body"}); err != nil {
		t.Fatal(err)
	}
	if p.gotDoc.TimeSource != domain.TimeSourceIngestDefault || p.gotDoc.Timestamp.Before(before) {
		t.Fatalf("doc time = %v %q", p.gotDoc.Timestamp, p.gotDoc.TimeSource)
	}
}

// Parts: the body is assembled from the parts, the earliest part time is the
// document time and the latest its span end.
func TestIngestMemory_PartsAssembleBodyAndSpan(t *testing.T) {
	t.Parallel()
	p := &fakeIngestionProcessor{docID: "source_doc:x"}
	s := &Server{IngestionProcessor: p}
	a := time.Date(2023, 5, 20, 7, 0, 0, 0, time.UTC)
	b := a.Add(30 * time.Minute)
	if _, err := s.IngestMemory(agentCtx("analyst"), &pb.IngestMemoryRequest{Parts: []*pb.MemoryPart{
		{Text: "user: hi", OccurredAt: timestamppb.New(a)},
		{Text: "assistant: hello", OccurredAt: timestamppb.New(b)},
	}}); err != nil {
		t.Fatal(err)
	}
	d := p.gotDoc
	if d.Body != "user: hi\n\nassistant: hello" || d.Title != "user: hi" {
		t.Fatalf("body/title = %q / %q", d.Body, d.Title)
	}
	if !d.Timestamp.Equal(a) || !d.EventTimeEnd.Equal(b) || len(d.Parts) != 2 {
		t.Fatalf("doc time = %v..%v parts=%d", d.Timestamp, d.EventTimeEnd, len(d.Parts))
	}
}

// The future-time tolerance is read from config (execution.ingestion).
func TestIngestMemory_ClampsFutureWithConfiguredTolerance(t *testing.T) {
	t.Parallel()
	p := &fakeIngestionProcessor{docID: "source_doc:x"}
	s := &Server{IngestionProcessor: p}
	s.ExecCfg = config.ExecutionConfig{}
	s.ExecCfg.Ingestion.EventTimeFutureToleranceMs = int(time.Hour / time.Millisecond)
	within := time.Now().Add(30 * time.Minute)
	if _, err := s.IngestMemory(agentCtx("a"), &pb.IngestMemoryRequest{Text: "x", OccurredAt: timestamppb.New(within)}); err != nil {
		t.Fatal(err)
	}
	if p.gotDoc.TimeSource != domain.TimeSourceExplicit {
		t.Fatalf("30 min with a 1 h tolerance must be accepted as stated, got %q", p.gotDoc.TimeSource)
	}
	far := time.Now().Add(3 * time.Hour)
	if _, err := s.IngestMemory(agentCtx("a"), &pb.IngestMemoryRequest{Text: "x", OccurredAt: timestamppb.New(far)}); err != nil {
		t.Fatal(err)
	}
	if p.gotDoc.TimeSource != domain.TimeSourceClampedFuture || !p.gotDoc.ClaimedTime.Equal(far.UTC().Truncate(time.Nanosecond)) {
		t.Fatalf("3 h ahead must be clamped: %q claimed=%v", p.gotDoc.TimeSource, p.gotDoc.ClaimedTime)
	}
}

func TestIngestMemory_EventTimeValidation(t *testing.T) {
	t.Parallel()
	s := &Server{IngestionProcessor: &fakeIngestionProcessor{}}
	cases := map[string]*pb.IngestMemoryRequest{
		"text and parts": {Text: "x", Parts: []*pb.MemoryPart{{Text: "y"}}},
		"bad precision":  {Text: "x", TimePrecision: "fortnight"},
		"invalid ts":     {Text: "x", OccurredAt: &timestamppb.Timestamp{Seconds: -1 << 40}},
		"invalid part":   {Parts: []*pb.MemoryPart{{Text: "y", OccurredAt: &timestamppb.Timestamp{Nanos: -1}}}},
	}
	for name, req := range cases {
		if _, err := s.IngestMemory(agentCtx("a"), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: got %v, want InvalidArgument", name, err)
		}
	}
}

// ctxSearcher records the context the handler hands the searcher.
type ctxSearcher struct {
	mockMemorySearcher
	gotCtx context.Context
}

func (c *ctxSearcher) Search(ctx context.Context, q, id string) ([]domain.SearchResult, error) {
	c.gotCtx = ctx
	return c.mockMemorySearcher.Search(ctx, q, id)
}

// ADR-0134 D4: reference_time and the window ride the context to the memory stack
// (P2 applies the window in SQL beside the scope); an inverted one is the caller's
// error.
func TestQueryMemory_EventTimeParameters(t *testing.T) {
	t.Parallel()
	ref := time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)
	cs := &ctxSearcher{}
	s := makeQueryMemoryServer(cs)
	if _, err := s.QueryMemory(ctxWithAgentID("a"), &pb.MemoryRequest{Query: "q", ReferenceTime: timestamppb.New(ref)}); err != nil {
		t.Fatal(err)
	}
	if q, ok := domain.EventTimeQueryFromContext(cs.gotCtx); !ok || !q.ReferenceTime.Equal(ref) {
		t.Fatalf("reference time did not reach the searcher: %+v %v", q, ok)
	}

	if _, err := s.QueryMemory(ctxWithAgentID("a"), &pb.MemoryRequest{Query: "q", EventAfter: timestamppb.New(ref)}); err != nil {
		t.Fatalf("a window must be accepted now that it is applied: %v", err)
	}
	if q, ok := domain.EventTimeQueryFromContext(cs.gotCtx); !ok || !q.EventAfter.Equal(ref) || !q.EventBefore.IsZero() {
		t.Fatalf("window did not reach the searcher: %+v %v", q, ok)
	}
	if _, err := s.QueryMemory(ctxWithAgentID("a"), &pb.MemoryRequest{
		Query: "q", EventAfter: timestamppb.New(ref), EventBefore: timestamppb.New(ref.Add(-time.Hour)),
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("inverted window: got %v, want InvalidArgument", err)
	}
}

// Hits report their event time from the one reader (domain.HitEventTime).
func TestQueryMemory_HitsCarryEventTime(t *testing.T) {
	t.Parallel()
	s := makeQueryMemoryServer(&mockMemorySearcher{results: []domain.SearchResult{{
		Document: domain.Document{ID: "c1", Text: "t", EventStamp: domain.EventStamp{
			EventTime:  time.Date(2023, 5, 20, 7, 51, 0, 0, time.UTC),
			TimeSource: domain.TimeSourceExplicit, TimePrecision: domain.TimePrecisionDay,
		}},
	}}})
	resp, err := s.QueryMemory(ctxWithAgentID("a"), &pb.MemoryRequest{Query: "q"})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.GetResults()[0]
	if r.GetEventTime().AsTime().Year() != 2023 || r.GetTimeSource() != "explicit" || r.GetTimePrecision() != "day" {
		t.Fatalf("hit time = %v %q %q", r.GetEventTime(), r.GetTimeSource(), r.GetTimePrecision())
	}
	if r.GetEventTimeEnd() != nil {
		t.Fatal("a single instant has no span end")
	}
}
