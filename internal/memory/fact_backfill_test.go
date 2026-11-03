package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// FactBackfill scheduling tests (fake sink and lister), plus one end-to-end run with
// the real FactExtractor over the in-memory substrate (ADR-0136 A1.7).

type fakeLister struct {
	cands []domain.FactBackfillCandidate
	last  domain.FactBackfillFilter
}

func (l *fakeLister) ListFactBackfillCandidates(_ context.Context, f domain.FactBackfillFilter) ([]domain.FactBackfillCandidate, error) {
	l.last = f
	return append([]domain.FactBackfillCandidate(nil), l.cands...), nil
}

type fakeSink struct {
	mu     sync.Mutex
	marked map[string]bool
	order  []string
	calls  int
	block  chan struct{} // when set, ExtractDocument waits on it or ctx
	sawCtx []bool        // bypass scope + system principal seen per call
}

func (s *fakeSink) AlreadyExtracted(_ context.Context, doc string, _ []string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marked[doc], nil
}

func (s *fakeSink) ExtractDocument(ctx context.Context, doc, _ string, _ domain.SourceMeta, chunks []*domain.Document) (FactDocumentResult, error) {
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return FactDocumentResult{}, ctx.Err()
		}
	}
	sc, _ := domain.ScopeFromContext(ctx)
	ok := sc != nil && sc.Bypass && domain.PrincipalFromContext(ctx) == domain.SystemPrincipal && len(chunks) > 0
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.marked[doc] = true
	s.order = append(s.order, doc)
	s.sawCtx = append(s.sawCtx, ok)
	return FactDocumentResult{LLMCalls: 1, Facts: 1, Stats: FactSaveStats{Inserted: 1}}, nil
}

func (s *fakeSink) MaxLLMCallsPerDocument() int { return 2 }

func (s *fakeSink) EstimateLLMCalls(int) int { return 2 }

func (s *fakeSink) ExtractorVersion() string { return "fake-v1/test" }

func backfillFixture(t *testing.T) (*FactBackfill, *fakeSink) {
	t.Helper()
	day := func(d int) time.Time { return time.Date(2023, 1, d, 0, 0, 0, 0, time.UTC) }
	cands := []domain.FactBackfillCandidate{
		{DocumentID: "d3", ChunkIDs: []string{"c3"}, SessionTime: day(3), Tags: []string{"a"}},
		{DocumentID: "d1", ChunkIDs: []string{"c1"}, SessionTime: day(1), Tags: []string{"a"}},
		{DocumentID: "d2", ChunkIDs: []string{"c2"}, SessionTime: day(2), Tags: []string{"b"}},
		{DocumentID: "d0", ChunkIDs: []string{"c0"}, SessionTime: day(1), Tags: []string{"a"}},
	}
	var chunks []domain.Document
	for _, id := range []string{"c0", "c1", "c2", "c3"} {
		chunks = append(chunks, laneDoc(id, "secret"))
	}
	// The real enforcing read store: a chunk read without the bypass on ctx would fail
	// closed, so a successful run proves the bypass travels on ctx.
	read := authz.NewEnforcingVectorStore(&laneChunkStore{docs: chunks}, quietLog())
	sink := &fakeSink{marked: map[string]bool{"d0": true}}
	return NewFactBackfill(&fakeLister{cands: cands}, read, sink), sink
}

func TestFactBackfill_DryRunCountsOnly(t *testing.T) {
	b, sink := backfillFixture(t)
	rep, err := b.Run(context.Background(), FactBackfillOptions{DryRun: true, Ordered: true, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Candidates != 4 || rep.AlreadyDone != 1 || rep.Pending != 3 || rep.Chunks != 3 ||
		rep.Groups != 2 || rep.EstimatedLLMCalls != 6 || sink.calls != 0 {
		t.Fatalf("dry run report %+v, sink calls %d", rep, sink.calls)
	}
}

// A2.4: the lister's SQL pre-filter is told the RUNNING extractor version, so only a
// complete marker of that version excludes a document; the estimate sums per-document
// windows from text length.
func TestFactBackfill_PassesExtractorVersionAndEstimatesWindows(t *testing.T) {
	lister := &fakeLister{cands: []domain.FactBackfillCandidate{{DocumentID: "d1", ChunkIDs: []string{"c1"}, TextChars: 5}}}
	b := NewFactBackfill(lister, &laneChunkStore{}, &fakeSink{marked: map[string]bool{}})
	rep, err := b.Run(context.Background(), FactBackfillOptions{DryRun: true, Logger: quietLog()})
	if err != nil || lister.last.ExtractorVersion != "fake-v1/test" || rep.EstimatedLLMCalls != 2 {
		t.Fatalf("filter %+v report %+v err %v", lister.last, rep, err)
	}
}

func TestFactBackfill_OrderedIdempotentRerun(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	b, sink := backfillFixture(t)
	rep, err := b.Run(context.Background(), FactBackfillOptions{Ordered: true, Concurrency: 1, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Extracted != 3 || rep.LLMCalls != 3 || rep.Failed != 0 || rep.Missing != 0 {
		t.Fatalf("report %+v", rep)
	}
	// Concurrency 1, groups by first document: scope a (d1, d3) then scope b (d2).
	if got := sink.order; len(got) != 3 || got[0] != "d1" || got[1] != "d3" || got[2] != "d2" {
		t.Fatalf("order = %v, want [d1 d3 d2]", got)
	}
	for i, ok := range sink.sawCtx {
		if !ok {
			t.Fatalf("call %d lacked the bypass scope / system principal / chunk text", i)
		}
	}
	rep, err = b.Run(context.Background(), FactBackfillOptions{Ordered: true, Concurrency: 4, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pending != 0 || sink.calls != 3 {
		t.Fatalf("re-run spent: report %+v, sink calls %d", rep, sink.calls)
	}
}

// The real extractor as the sink: the backfilled items carry the document's recorded
// author as their actor (A1-Q2 — not the system principal the backfill writes as), the
// marker makes a re-run free, and the chunk text was read through the enforcing store.
func TestFactBackfill_RealExtractorKeysOnTheAuthor(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	sub := newMemSubstrate()
	gen := &scriptedGen{answer: func(string) (string, error) {
		return `{"facts":[{"subject":"user","attribute":"pet","value":"Luna","passage":1,"quote":"text of c1"}]}`, nil
	}}
	e := newTestExtractor(gen, sub, FactExtractorConfig{})
	read := authz.NewEnforcingVectorStore(&laneChunkStore{docs: []domain.Document{laneDoc("c1", "secret")}}, quietLog())
	lister := &fakeLister{cands: []domain.FactBackfillCandidate{{DocumentID: "d1", ChunkIDs: []string{"c1"},
		SessionTime: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), Tags: []string{"secret"}, Author: "lme-ns-q1"}}}
	b := NewFactBackfill(lister, read, e)
	rep, err := b.Run(context.Background(), FactBackfillOptions{Ordered: true, Logger: quietLog()})
	if err != nil || rep.Extracted != 1 || rep.FactsInserted != 1 || gen.calls.Load() != 1 {
		t.Fatalf("report %+v err %v calls %d", rep, err, gen.calls.Load())
	}
	f, ok := sub.byValue("Luna")
	if !ok || f.Actor != "lme-ns-q1" || len(f.Tags) != 1 || f.Tags[0] != "secret" {
		t.Fatalf("fact = %+v, want actor = the recorded author, tags = the source chunk's", f)
	}
	rep, _ = b.Run(context.Background(), FactBackfillOptions{Ordered: true, Logger: quietLog()})
	if rep.Pending != 0 || gen.calls.Load() != 1 {
		t.Fatalf("re-run spent: %+v calls %d", rep, gen.calls.Load())
	}
}

func TestFactBackfill_CancellationIsClean(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	b, sink := backfillFixture(t)
	sink.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	rep, err := b.Run(ctx, FactBackfillOptions{Ordered: false, Concurrency: 2, Logger: quietLog()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rep.Extracted != 0 || rep.Cancelled != 3 || sink.calls != 0 {
		t.Fatalf("report %+v, calls %d", rep, sink.calls)
	}
}
