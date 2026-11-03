package memory

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0136 gate: TestStageA_FeaturesArePoolIndependent. Adding an unrelated
// candidate — the newest-dated, most central, TOP lexical hit, but with a tiny
// ts_rank_cd, no shared entity and an orthogonal embedding — changes none of the
// other candidates' calibrated feature values. Under the legacy blend the same
// newcomer moves three of them (lexical 1/rank, pool min-max PageRank and recency),
// which the test also asserts so it cannot pass vacuously.
func TestStageA_FeaturesArePoolIndependent(t *testing.T) {
	fx := newRankingFixture()
	ref := time.Date(2023, 5, 3, 0, 0, 0, 0, time.UTC)
	newcomer := timedDoc("zz", time.Date(2023, 5, 2, 0, 0, 0, 0, time.UTC)) // newest in the pool
	newcomer.Metadata["document_id"] = "Z"
	newcomer.Embedding.Vector = []float32{0, 0, 1e-3}

	w := BlendWeights{Cosine: 0.4, Lexical: 0.2, GraphCoherence: 0.05, Confidence: 0.1, PageRank: 0.05, Recency: 0.1, TemporalFit: 0.2}
	ti := TemporalIntent("What did I most recently plant two weeks ago?", ref) // order latest + a band: every feature live
	if ti.Target == nil || ti.Order != OrderLatest {
		t.Fatalf("fixture intent = %+v", ti)
	}
	signals := fixedSignals{pr: map[string]float64{}, conf: fx.signals.conf}
	for k, v := range fx.signals.pr {
		signals.pr[k] = v
	}
	signals.pr["zz"] = 0.5 // far above every other PageRank: stretches a pool min-max

	q := &QueryService{}
	bl := NewBlender(w)
	q.EnableBlend(&bl, corpusSizedSignals{signals, 4000})
	q.chunkTriplets = fx.triplets
	q.SetRecencyCurve(RecencyCurve{HalfLife: 90 * 24 * time.Hour})
	q.SetRankingV2(RankingV2{Calibrated: true, TemporalIntent: true, TemporalFitWeight: 0.2})
	ctx := domain.WithEventTimeQuery(context.Background(), domain.EventTimeQuery{ReferenceTime: ref})

	features := func(lexical []domain.SearchResult) map[string]StageASignals {
		pool := rrfFuse(cloneResults(fx.dense), lexical, 60, 100, 1)
		ids := make([]string, 0, len(pool))
		for _, r := range pool {
			ids = append(ids, r.Document.ID)
		}
		pr, _ := signals.ChunkPageRanks(ctx, ids)
		conf, _ := signals.MeanChunkConfidence(ctx, ids)
		fs := q.calibratedFeatures(ctx, pool, fx.queryVec, w, pr, conf, ti, true)
		out := map[string]StageASignals{}
		for i, r := range pool {
			out[r.Document.ID] = fs[i]
		}
		return out
	}
	before := features(cloneResults(fx.lexical))
	withNew := append([]domain.SearchResult{{Document: newcomer, Score: 0.001}}, cloneResults(fx.lexical)...)
	after := features(withNew)
	if _, ok := after["zz"]; !ok {
		t.Fatal("the newcomer must be in the second pool")
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-12 }
	for id, f := range before {
		g := after[id]
		if !near(f.Cosine, g.Cosine) || !near(f.Lexical, g.Lexical) || !near(f.Recency, g.Recency) ||
			!near(f.PageRank, g.PageRank) || !near(f.GraphCoherence, g.GraphCoherence) ||
			!near(f.MeanConfidence, g.MeanConfidence) || !near(f.Activation, g.Activation) || !near(f.TemporalFit, g.TemporalFit) {
			t.Errorf("%s: features moved when an unrelated candidate joined the pool:\n before %+v\n after  %+v", id, f, g)
		}
	}
	// Teeth: some features must actually be live, or equality proves nothing.
	if before["b1"].TemporalFit < 0.99 || before["a1"].Recency <= 0 || before["d2"].Lexical <= 0 || before["b1"].PageRank <= 0 {
		t.Fatalf("fixture features not live: b1 %+v a1 %+v d2 %+v", before["b1"], before["a1"], before["d2"])
	}
	// …and the legacy signals DO move for the same newcomer.
	legacyBefore := rrfFuse(cloneResults(fx.dense), cloneResults(fx.lexical), 60, 100, 1)
	legacyAfter := rrfFuse(cloneResults(fx.dense), withNew, 60, 100, 1)
	if lexOf(legacyBefore, "d2") == lexOf(legacyAfter, "d2") {
		t.Fatal("legacy 1/rank lexical should change when a new top lexical hit arrives")
	}
}

type corpusSizedSignals struct {
	fixedSignals
	n int
}

func (c corpusSizedSignals) PageRankMeta(context.Context) (time.Time, int, int, error) {
	return time.Time{}, c.n, 0, nil
}

func lexOf(rs []domain.SearchResult, id string) float64 {
	for _, r := range rs {
		if r.Document.ID == id {
			return r.LexicalScore
		}
	}
	return -1
}

func TestCalibratedFeatureScales(t *testing.T) {
	if got := saturatedLexical(0.1, 0.1); got != 0.5 {
		t.Fatalf("one ts_rank_cd cover (0.1) at k=0.1 must be 0.5, got %v", got)
	}
	if saturatedLexical(0, 0.1) != 0 {
		t.Fatal("no lexical match must contribute 0")
	}
	if got := calibratedPageRank(1.0/1000, 1000); got != 0.5 {
		t.Fatalf("an average chunk (x = 1/N) must be 0.5, got %v", got)
	}
	if calibratedPageRank(0.01, 0) != 0 {
		t.Fatal("PageRank never computed (N = 0) must contribute 0")
	}
	ref := time.Date(2023, 5, 30, 0, 0, 0, 0, time.UTC)
	hl := 90 * 24 * time.Hour
	if got := calibratedRecency(ref.Add(-hl), ref, hl, 1); math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("one half-life old must decay to 0.5, got %v", got)
	}
	if got := calibratedRecency(ref.Add(-hl), ref, hl, -1); math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("earliest inverts the decay, got %v", got)
	}
	if calibratedRecency(ref.Add(-hl), time.Time{}, hl, 1) != 0 {
		t.Fatal("without a reference time the calibrated recency must be 0, not pool-anchored")
	}
	if calibratedRecency(ref.Add(time.Hour), ref, hl, 1) != 0 {
		t.Fatal("a chunk dated after the asker's now gets no recency credit")
	}
	if calibratedRecency(ref.Add(-hl), ref, hl, 0) != 0 {
		t.Fatal("no order asked ⇒ recency off")
	}
	if calibratedCoherence(2.5) != 1 || calibratedCoherence(0.5) != 0.5 {
		t.Fatal("coherence is raw / cap, clamped to 1")
	}
}

// ADR-0136 gate: TestTemporalIntent_RelativePhrases, over phrases typical of
// LongMemEval questions, as seen from a question dated 2023-05-30 14:00 UTC.
func TestTemporalIntent_RelativePhrases(t *testing.T) {
	ref := time.Date(2023, 5, 30, 14, 0, 0, 0, time.UTC)
	day := func(m time.Month, d int) time.Time { return time.Date(2023, m, d, 0, 0, 0, 0, time.UTC) }
	type want struct {
		order     TemporalOrder
		hasTarget bool
		from, to  time.Time
		precision string
		explicit  bool
	}
	cases := []struct {
		q string
		w want
	}{
		{"What gardening activity did I do two weeks ago?", want{OrderNone, true, ref.AddDate(0, 0, -14).Add(-84 * time.Hour), ref.AddDate(0, 0, -14).Add(84 * time.Hour), "week", false}},
		{"What milestone did I reach four weeks ago?", want{OrderNone, true, ref.AddDate(0, 0, -28).Add(-84 * time.Hour), ref.AddDate(0, 0, -28).Add(84 * time.Hour), "week", false}},
		{"Who did I cook for a couple of days ago?", want{OrderNone, true, day(5, 28), day(5, 29), "day", false}},
		{"What did I buy last month?", want{OrderNone, true, day(4, 1), day(5, 1), "month", false}},
		{"What did I do in March 2023?", want{OrderNone, true, day(3, 1), day(4, 1), "month", true}},
		{"Where did I travel most recently?", want{OrderLatest, false, time.Time{}, time.Time{}, "", false}},
		{"When was the first time I went hiking?", want{OrderEarliest, false, time.Time{}, time.Time{}, "", false}},
		{"Where am I currently working?", want{OrderCurrent, false, time.Time{}, time.Time{}, "", false}},
		{"How has my running routine changed?", want{OrderHistory, false, time.Time{}, time.Time{}, "", false}},
		{"How many days ago did I attend the concert?", want{OrderNone, false, time.Time{}, time.Time{}, "", false}},
		{"What is the name of my dog?", want{OrderNone, false, time.Time{}, time.Time{}, "", false}},
		{"What did I do yesterday?", want{OrderNone, true, day(5, 29), day(5, 30), "day", false}},
		{"How many books did I read in the last two weeks?", want{OrderNone, true, ref.AddDate(0, 0, -14), ref, "range", false}},
		{"What was my first job and what is my current one?", want{OrderHistory, false, time.Time{}, time.Time{}, "", false}},
	}
	for _, c := range cases {
		got := TemporalIntent(c.q, ref)
		if got.Order != c.w.order {
			t.Errorf("%q: order %q, want %q", c.q, got.Order, c.w.order)
		}
		if (got.Target != nil) != c.w.hasTarget {
			t.Errorf("%q: target %+v (phrase %q), want target=%t", c.q, got.Target, got.Phrase, c.w.hasTarget)
			continue
		}
		if got.Explicit != c.w.explicit {
			t.Errorf("%q: explicit %t, want %t", c.q, got.Explicit, c.w.explicit)
		}
		if got.Target == nil {
			continue
		}
		if !got.Target.From.Equal(c.w.from) || !got.Target.To.Equal(c.w.to) || got.Target.Precision != c.w.precision {
			t.Errorf("%q (phrase %q): band [%s, %s) %s, want [%s, %s) %s", c.q, got.Phrase,
				got.Target.From.Format(time.RFC3339), got.Target.To.Format(time.RFC3339), got.Target.Precision,
				c.w.from.Format(time.RFC3339), c.w.to.Format(time.RFC3339), c.w.precision)
		}
	}
}

// With no reference time on the request there is nothing for a relative phrase to
// be relative to: no target. An absolute range still has one.
func TestTemporalIntent_NoReferenceTime(t *testing.T) {
	if ti := TemporalIntent("What did I do two weeks ago?", time.Time{}); ti.Target != nil {
		t.Fatalf("relative phrase without reference_time must have no target, got %+v (%q)", ti.Target, ti.Phrase)
	}
	ti := TemporalIntent("What did I do in March 2023?", time.Time{})
	if ti.Target == nil || !ti.Explicit || !ti.Target.From.Equal(time.Date(2023, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("absolute range without reference_time keeps its band, got %+v", ti)
	}
	if ti := TemporalIntent("What is my current job?", time.Time{}); ti.Order != OrderCurrent {
		t.Fatalf("order is read without a reference time, got %q", ti.Order)
	}
}

func TestTemporalIntent_Fit(t *testing.T) {
	b := &TimeBand{From: time.Date(2023, 5, 13, 0, 0, 0, 0, time.UTC), To: time.Date(2023, 5, 20, 0, 0, 0, 0, time.UTC), Sigma: 84 * time.Hour}
	ti := TimeIntent{Target: b}
	in := time.Date(2023, 5, 15, 0, 0, 0, 0, time.UTC)
	if ti.Fit(in, in) != 1 {
		t.Fatal("inside the band ⇒ 1")
	}
	oneSigma := b.From.Add(-84 * time.Hour)
	if got := ti.Fit(oneSigma, oneSigma); math.Abs(got-math.Exp(-1)) > 1e-12 {
		t.Fatalf("one σ before the band ⇒ e^-1, got %v", got)
	}
	if (TimeIntent{}).Fit(in, in) != 0 {
		t.Fatal("no target ⇒ 0")
	}
	// A span that overlaps the band fits fully even when it starts before it.
	if ti.Fit(b.From.Add(-48*time.Hour), b.From.Add(time.Hour)) != 1 {
		t.Fatal("an overlapping span fits")
	}
}

// The shared go-dateparser Parser is used from concurrent recalls; -race checks it.
func TestTemporalIntent_ConcurrentUse(t *testing.T) {
	ref := time.Date(2023, 5, 30, 14, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if ti := TemporalIntent("What did I do two weeks ago?", ref); ti.Target == nil {
					t.Error("lost the target under concurrency")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// D3 on, but the query names no time and asks for no order: the ranking is exactly
// the flags-off ranking (the inactive features leave numerator and denominator).
func TestRankingV2_NonTemporalQueryUnchangedByTemporalIntent(t *testing.T) {
	fx := newRankingFixture()
	w := BlendWeights{Cosine: 0.4, Lexical: 0.2, GraphCoherence: 0.05, Confidence: 0.1, PageRank: 0.05, Activation: 0.05}
	ref := time.Date(2023, 5, 3, 0, 0, 0, 0, time.UTC)
	ctx := domain.WithEventTimeQuery(context.Background(), domain.EventTimeQuery{ReferenceTime: ref})
	mk := func(rv RankingV2) *QueryService {
		q := &QueryService{}
		bl := NewBlender(w)
		q.EnableBlend(&bl, fx.signals)
		q.chunkTriplets = fx.triplets
		q.SetRankingV2(rv)
		return q
	}
	off := rankFixture(t, mk(RankingV2{}), ctx, fx, 6)
	on := mk(RankingV2{TemporalIntent: true, TemporalFitWeight: 0.2})
	ti := TemporalIntent("What is the name of the tomato variety I planted?", ref)
	if ti.Target != nil || ti.Order != OrderNone {
		t.Fatalf("fixture query must carry no intent, got %+v", ti)
	}
	if got := rankFixture(t, on, withTimeIntent(ctx, ti), fx, 6); got != off {
		t.Fatalf("a non-temporal query must rank as with D3 off.\n off:\n%s\n on:\n%s", off, got)
	}
}

// D3's band lifts the chunk dated in it, and "earliest" inverts recency.
func TestRankingV2_TemporalIntentMovesTheRanking(t *testing.T) {
	ref := time.Date(2023, 5, 30, 14, 0, 0, 0, time.UTC)
	pool := func() []domain.SearchResult {
		return []domain.SearchResult{
			{Document: timedDoc("recent", ref.AddDate(0, 0, -1)), RawScore: 0.80},
			{Document: timedDoc("two-weeks", ref.AddDate(0, 0, -14)), RawScore: 0.78},
			{Document: timedDoc("old", ref.AddDate(0, -6, 0)), RawScore: 0.79},
		}
	}
	ctx := domain.WithEventTimeQuery(context.Background(), domain.EventTimeQuery{ReferenceTime: ref})
	for _, calibrated := range []bool{false, true} {
		q := &QueryService{}
		bl := NewBlender(BlendWeights{Cosine: 0.4, Recency: 0.2})
		q.EnableBlend(&bl, zeroSignals{})
		q.SetRecencyCurve(RecencyCurve{Decay: true, HalfLife: 90 * 24 * time.Hour})
		q.SetRankingV2(RankingV2{Calibrated: calibrated, TemporalIntent: true, TemporalFitWeight: 0.2})

		ti := TemporalIntent("What did I plant two weeks ago?", ref)
		if got := q.applyStageABlend(withTimeIntent(ctx, ti), pool(), nil); got[0].Document.ID != "two-weeks" {
			t.Errorf("calibrated=%t: the in-band chunk must rank first, got %s", calibrated, got[0].Document.ID)
		}
		early := TemporalIntent("What was the first thing I planted?", ref)
		if got := q.applyStageABlend(withTimeIntent(ctx, early), pool(), nil); got[0].Document.ID != "old" {
			t.Errorf("calibrated=%t: earliest must favour the oldest chunk, got %s", calibrated, got[0].Document.ID)
		}
		late := TemporalIntent("What did I plant most recently?", ref)
		if got := q.applyStageABlend(withTimeIntent(ctx, late), pool(), nil); got[0].Document.ID != "recent" {
			t.Errorf("calibrated=%t: latest must favour the newest chunk, got %s", calibrated, got[0].Document.ID)
		}
	}
}

// D3 through the real read path: an absolute range is pushed down as the hard
// window (ADR-0134 D5b) and mirrored; a relative phrase is not; a caller's own
// window wins over a parsed one.
func TestSearch_TemporalIntentExplicitWindowOnly(t *testing.T) {
	ref := time.Date(2023, 6, 30, 0, 0, 0, 0, time.UTC)
	newStore := func() *optsCapturingStore {
		return &optsCapturingStore{scopeApplyingStore: scopeApplyingStore{docs: []domain.Document{
			timedDoc("jan", jan), timedDoc("mar", mar), timedDoc("jun", jun),
		}}}
	}
	run := func(store *optsCapturingStore, query string, tq domain.EventTimeQuery) []string {
		q := NewQueryService(&fakeEmbedder{}, store, &recordingAuthorizer{})
		q.SetRankingV2(RankingV2{TemporalIntent: true, TemporalFitWeight: 0.2})
		res, err := q.Search(domain.WithEventTimeQuery(context.Background(), tq), query, "agent-a")
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, r := range res {
			ids = append(ids, r.Document.ID)
		}
		return ids
	}

	s := newStore()
	got := run(s, "What did I do in March 2023?", domain.EventTimeQuery{ReferenceTime: ref})
	if !s.got.Since.Equal(time.Date(2023, 3, 1, 0, 0, 0, 0, time.UTC)) || !s.got.Until.Equal(time.Date(2023, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("explicit range not pushed down: since %s until %s", s.got.Since, s.got.Until)
	}
	if len(got) != 1 || got[0] != "mar" {
		t.Fatalf("explicit range must be mirrored in admit, got %v", got)
	}

	s = newStore()
	got = run(s, "What did I do two weeks ago?", domain.EventTimeQuery{ReferenceTime: ref})
	if !s.got.Since.IsZero() || !s.got.Until.IsZero() || len(got) != 3 {
		t.Fatalf("a relative phrase must never hard-filter: since %s until %s, got %v", s.got.Since, s.got.Until, got)
	}

	s = newStore()
	caller := domain.EventTimeQuery{ReferenceTime: ref, EventAfter: time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC)}
	got = run(s, "What did I do in March 2023?", caller)
	if !s.got.Since.Equal(caller.EventAfter) || !s.got.Until.IsZero() {
		t.Fatalf("the caller's window must win: since %s until %s", s.got.Since, s.got.Until)
	}
	if len(got) != 1 || got[0] != "jun" {
		t.Fatalf("caller window applied, got %v", got)
	}
}
