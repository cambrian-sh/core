package memory

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// optsCapturingStore records the SearchOptions the read path built, and returns its
// docs WITHOUT applying the window — the shape of a lane that never saw the SQL
// predicate (entity seeds, kgExpand, anchor promotion all reach chunks by id).
type optsCapturingStore struct {
	scopeApplyingStore
	got domain.SearchOptions
}

func (s *optsCapturingStore) Search(ctx context.Context, v []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	s.got = opts
	return s.scopeApplyingStore.Search(ctx, v, opts)
}

var (
	jan = time.Date(2023, 1, 15, 0, 0, 0, 0, time.UTC)
	mar = time.Date(2023, 3, 15, 0, 0, 0, 0, time.UTC)
	jun = time.Date(2023, 6, 15, 0, 0, 0, 0, time.UTC)
)

func timedDoc(id string, at time.Time) domain.Document {
	return domain.Document{ID: id, Text: id, Metadata: map[string]any{},
		EventStamp: domain.EventStamp{EventTime: at, TimeSource: domain.TimeSourceExplicit}}
}

// ADR-0134 P2: the window reaches SearchOptions (the SQL push-down beside Scope), AND
// is re-asserted where every lane converges, so a row that arrives without passing
// through the SQL can never be returned out of window.
func TestSearch_EventWindowPushedDownAndMirrored(t *testing.T) {
	t.Parallel()
	store := &optsCapturingStore{scopeApplyingStore: scopeApplyingStore{docs: []domain.Document{
		timedDoc("jan", jan), timedDoc("mar", mar), timedDoc("jun", jun),
	}}}
	q := NewQueryService(&fakeEmbedder{}, store, &recordingAuthorizer{})
	ctx := domain.WithEventTimeQuery(context.Background(), domain.EventTimeQuery{
		EventAfter: time.Date(2023, 2, 1, 0, 0, 0, 0, time.UTC), EventBefore: time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	res, err := q.Search(ctx, "what happened", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if store.got.Since.IsZero() || store.got.Until.IsZero() || store.got.Scope == nil {
		t.Fatalf("window/scope not on SearchOptions: %+v", store.got)
	}
	if len(res) != 1 || res[0].Document.ID != "mar" {
		ids := []string{}
		for _, r := range res {
			ids = append(ids, r.Document.ID)
		}
		t.Fatalf("returned %v, want only [mar]", ids)
	}
}

type zeroSignals struct{}

func (zeroSignals) ChunkPageRanks(context.Context, []string) (map[string]float64, error) {
	return map[string]float64{}, nil
}
func (zeroSignals) MeanChunkConfidence(context.Context, []string) (map[string]float64, error) {
	return map[string]float64{}, nil
}

// ADR-0134 D4: reference_time is the recency term's "now". A fact dated after the
// asker's now gets no recency credit and does not stretch the range, so "most
// recent as of April" is March, not June.
func TestStageABlend_RecencyAnchoredAtReferenceTime(t *testing.T) {
	t.Parallel()
	q := &QueryService{}
	b := NewBlender(BlendWeights{Recency: 1})
	q.EnableBlend(&b, zeroSignals{})
	pool := func() []domain.SearchResult {
		return []domain.SearchResult{{Document: timedDoc("jan", jan)}, {Document: timedDoc("jun", jun)}, {Document: timedDoc("mar", mar)}}
	}
	if got := q.applyStageABlend(context.Background(), pool(), nil); got[0].Document.ID != "jun" {
		t.Fatalf("without a reference time the newest wins, got %s", got[0].Document.ID)
	}
	ctx := domain.WithEventTimeQuery(context.Background(), domain.EventTimeQuery{
		ReferenceTime: time.Date(2023, 4, 1, 0, 0, 0, 0, time.UTC),
	})
	got := q.applyStageABlend(ctx, pool(), nil)
	if got[0].Document.ID != "mar" {
		t.Fatalf("as of April the most recent is March, got order %s,%s,%s",
			got[0].Document.ID, got[1].Document.ID, got[2].Document.ID)
	}
	for _, r := range got {
		if r.Document.ID == "jun" && r.Score != 0 {
			t.Fatalf("a fact after the reference time got recency credit: %v", r.Score)
		}
	}
}

func TestKeepInWindow(t *testing.T) {
	t.Parallel()
	rs := []domain.SearchResult{{Document: timedDoc("jan", jan)}, {Document: timedDoc("jun", jun)}}
	if got := keepInWindow(rs, domain.EventTimeQuery{}); len(got) != 2 {
		t.Fatal("no window must keep everything")
	}
	got := keepInWindow(rs, domain.EventTimeQuery{EventAfter: mar})
	if len(got) != 1 || got[0].Document.ID != "jun" {
		t.Fatalf("got %+v", got)
	}
}

// chronoCiter records the passages the cited synthesis was given.
type chronoCiter struct {
	fakePlanner
	got []string
}

func (c *chronoCiter) SynthesizeCited(_ context.Context, _ string, chunks []string) (string, string, error) {
	c.got = chunks
	return "answer", "a [1]", nil
}

// ADR-0134 D5a, flag off: the reader's context is exactly the relevance order with
// bare texts (the behaviour before D5a).
func TestSynthesisContext_OffIsUnchanged(t *testing.T) {
	t.Parallel()
	q := &QueryService{}
	rows := []domain.SearchResult{{Document: timedDoc("jun", jun)}, {Document: timedDoc("jan", jan)}}
	out, texts := q.synthesisContext(rows, 16)
	if out[0].Document.ID != "jun" || texts[0] != "jun" || texts[1] != "jan" {
		t.Fatalf("off must not reorder or decorate: %v %v", out, texts)
	}
}

// Flag on: the cited passages are ordered by event time and dated at their
// precision, undated (ingest_default) rows follow the dated ones and say so, rows
// past the context window are untouched, and the evidence returned by Answer is
// reordered WITH the passages so [n] still resolves to evidence[n-1].
func TestAnswer_ChronologicalContextOrdersAndDatesPassages(t *testing.T) {
	t.Parallel()
	undated := domain.Document{ID: "undated", Text: "undated", Metadata: map[string]any{},
		EventStamp: domain.EventStamp{EventTime: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), TimeSource: domain.TimeSourceIngestDefault}}
	dayPrec := timedDoc("jan", jan)
	dayPrec.TimePrecision = domain.TimePrecisionDay
	span := timedDoc("mar", mar)
	span.EventTimeEnd = mar.Add(48 * time.Hour)
	span.TimePrecision = domain.TimePrecisionDay
	store := &scopeApplyingStore{docs: []domain.Document{timedDoc("jun", jun), undated, dayPrec, span}}
	q := NewQueryService(&fakeEmbedder{}, store, &recordingAuthorizer{})
	cs := &chronoCiter{}
	q.planner = cs
	q.EnableChronologicalContext()

	_, _, evidence, err := q.Answer(context.Background(), "what happened", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"jan", "mar", "jun", "undated"}
	for i, id := range wantIDs {
		if evidence[i].Document.ID != id {
			t.Fatalf("evidence[%d] = %s, want order %v", i, evidence[i].Document.ID, wantIDs)
		}
	}
	wantPrefix := []string{
		"[Date: 2023-01-15]\njan",
		"[Date: 2023-03-15 to 2023-03-17]\nmar",
		"[Date: 2023-06-15 00:00:00 UTC]\njun",
		"[Date: unknown (recorded 2026-11-01 00:00:00 UTC)]\nundated",
	}
	for i, w := range wantPrefix {
		if cs.got[i] != w {
			t.Fatalf("passage %d = %q, want %q", i, cs.got[i], w)
		}
	}
}

type decisionCapture struct{ got []domain.RetrievalDecision }

func (d *decisionCapture) ObserveRetrieval(r domain.RetrievalDecision) { d.got = append(d.got, r) }

// ADR-0134 P3 / Q7: the retrieval decision (what a premium receipt is built from)
// records the query's reference time and window.
func TestEmitDecision_CarriesEventTimeQuery(t *testing.T) {
	t.Parallel()
	obs := &decisionCapture{}
	q := NewQueryService(&fakeEmbedder{}, corpusOfOne(), &recordingAuthorizer{})
	q.SetDecisionObserver(obs, "fp")
	want := domain.EventTimeQuery{ReferenceTime: jun, EventAfter: jan, EventBefore: mar}
	if _, err := q.Search(domain.WithEventTimeQuery(context.Background(), want), "q", "agent-a"); err != nil {
		t.Fatal(err)
	}
	if len(obs.got) != 1 || obs.got[0].EventTime != want {
		t.Fatalf("decision event time = %+v", obs.got)
	}
}
