package memory

import (
	"context"
	"math"
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

// T-20261102-stage-a-recency-is-min-max-over-the-candidate: decay recency is a FIXED
// scale. Under min-max, the same two dates a week apart score 0 and 1 in a tight pool
// (a large gap a recency weight of 0.1 turns into an eviction), and a document's
// credit depends on what else is in the pool; under decay a document's credit
// depends only on its own age (from the reference time, else from the newest
// candidate), so adding an older document to the pool changes nobody else's score.
func TestStageABlend_RecencyDecayIsFixedScale(t *testing.T) {
	t.Parallel()
	week := 7 * 24 * time.Hour
	newest := time.Date(2023, 2, 14, 0, 0, 0, 0, time.UTC)
	scoreOf := func(q *QueryService, ctx context.Context, pool []domain.SearchResult, id string) float64 {
		for _, r := range q.applyStageABlend(ctx, pool, nil) {
			if r.Document.ID == id {
				return r.Score
			}
		}
		t.Fatalf("%s missing", id)
		return 0
	}
	tight := func() []domain.SearchResult {
		return []domain.SearchResult{{Document: timedDoc("new", newest)}, {Document: timedDoc("week-old", newest.Add(-week))}}
	}
	wide := func() []domain.SearchResult {
		return append(tight(), domain.SearchResult{Document: timedDoc("year-old", newest.Add(-365*24*time.Hour))})
	}

	mm := &QueryService{}
	b := NewBlender(BlendWeights{Recency: 1})
	mm.EnableBlend(&b, zeroSignals{})
	if got := scoreOf(mm, context.Background(), tight(), "week-old"); got != 0 {
		t.Fatalf("min-max (default): the older of two is stretched to 0, got %v", got)
	}

	dq := &QueryService{}
	dq.EnableBlend(&b, zeroSignals{})
	dq.SetRecencyCurve(RecencyCurve{Decay: true, HalfLife: 90 * 24 * time.Hour})
	inTight := scoreOf(dq, context.Background(), tight(), "week-old")
	inWide := scoreOf(dq, context.Background(), wide(), "week-old")
	want := math.Exp2(-7.0 / 90.0)
	if math.Abs(inTight-want) > 1e-9 || math.Abs(inWide-want) > 1e-9 {
		t.Fatalf("decay: a week-old doc scores 2^(-7/90)=%.4f whatever the pool; got tight %.4f wide %.4f", want, inTight, inWide)
	}
	if got := scoreOf(dq, context.Background(), tight(), "new"); math.Abs(got-1) > 1e-9 {
		t.Fatalf("decay without a reference time anchors at the newest candidate (score 1), got %v", got)
	}
	ref := domain.WithEventTimeQuery(context.Background(), domain.EventTimeQuery{ReferenceTime: newest.Add(90 * 24 * time.Hour)})
	if got := scoreOf(dq, ref, tight(), "new"); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("with a reference time one half-life later the newest scores 0.5, got %v", got)
	}

	// A non-positive half-life never reaches the formula (config refuses it; the
	// setter falls back to min-max rather than divide by zero).
	z := &QueryService{}
	z.SetRecencyCurve(RecencyCurve{Decay: true})
	if z.recencyCurve.Decay {
		t.Fatal("a zero half-life must fall back to min-max")
	}
}
