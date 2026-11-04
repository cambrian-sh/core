package memory

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// updateRankingGolden regenerates testdata/ranking_v2_flags_off.golden. The golden
// was written by the PRE-ADR-0136 code (2026-11-03, before any ranking-v2 edit), so
// it is the definition of "flags off changes nothing": never regenerate it to make a
// failing test pass — a diff means the flag-off ranking function moved.
var updateRankingGolden = flag.Bool("update-ranking-golden", false, "rewrite the ADR-0136 flags-off golden")

// fixedSignals is a RankSignalStore with fixed per-chunk PageRank and confidence.
type fixedSignals struct {
	pr, conf map[string]float64
}

func (f fixedSignals) ChunkPageRanks(_ context.Context, ids []string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, id := range ids {
		if v, ok := f.pr[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func (f fixedSignals) MeanChunkConfidence(_ context.Context, ids []string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, id := range ids {
		if v, ok := f.conf[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// rankingFixture is a deterministic candidate pool spread over four source
// documents: dense and lexical lists that overlap, embeddings, event times,
// PageRank, confidence, and a triplet graph for coherence. Everything every
// Stage-A signal reads is populated, so a change to any of them shows in the golden.
type rankingFixture struct {
	dense, lexical []domain.SearchResult
	signals        fixedSignals
	triplets       *fakeChunkTripletsStore
	queryVec       []float32
}

func newRankingFixture() rankingFixture {
	base := time.Date(2023, 5, 1, 12, 0, 0, 0, time.UTC)
	mk := func(id, docID string, day int, vec []float32) domain.Document {
		d := timedDoc(id, base.AddDate(0, 0, day))
		d.Metadata["document_id"] = docID
		d.Embedding.Vector = vec
		d.ActivationStrength = float64(day%3) / 3
		return d
	}
	docs := []domain.Document{
		mk("a1", "A", 0, []float32{0.9, 0.1, 0.2}),
		mk("a2", "A", 0, []float32{0.85, 0.2, 0.1}),
		mk("a3", "A", 0, []float32{0.8, 0.3, 0.3}),
		mk("a4", "A", 0, []float32{0.75, 0.1, 0.4}),
		mk("b1", "B", -14, []float32{0.7, 0.5, 0.1}),
		mk("b2", "B", -14, []float32{0.6, 0.6, 0.2}),
		mk("c1", "C", -30, []float32{0.5, 0.2, 0.8}),
		mk("c2", "C", -30, []float32{0.4, 0.7, 0.5}),
		mk("d1", "D", -2, []float32{0.3, 0.9, 0.1}),
		mk("d2", "D", -2, []float32{0.2, 0.4, 0.9}),
	}
	byID := map[string]domain.Document{}
	for _, d := range docs {
		byID[d.ID] = d
	}
	dense := []domain.SearchResult{}
	for i, id := range []string{"a1", "a2", "b1", "a3", "a4", "c1", "b2", "d1"} {
		cos := 0.9 - 0.05*float64(i)
		dense = append(dense, domain.SearchResult{Document: byID[id], Score: cos, RawScore: cos})
	}
	lexical := []domain.SearchResult{}
	for i, id := range []string{"d2", "b1", "c2", "a1"} {
		lexical = append(lexical, domain.SearchResult{Document: byID[id], Score: 0.3 - 0.05*float64(i)})
	}
	ts := newFakeChunkTripletsStore()
	link := func(id string, ents ...string) {
		var trs []domain.ChunkTriplet
		for _, e := range ents {
			trs = append(trs, domain.ChunkTriplet{H: "user", R: "mentions", T: e})
		}
		ts.byChunkID[id] = trs
	}
	link("a1", "tomato", "garden")
	link("a2", "garden")
	link("b1", "tomato", "saplings")
	link("b2", "saplings")
	link("c2", "garden", "hose")
	link("d2", "tomato")
	return rankingFixture{
		dense:    dense,
		lexical:  lexical,
		signals:  fixedSignals{pr: map[string]float64{"a1": 1e-4, "b1": 3e-4, "c1": 2e-4, "d2": 5e-5}, conf: map[string]float64{"a1": 2, "b2": 1, "c2": 1.5}},
		triplets: ts,
		queryVec: []float32{1, 0.2, 0.2},
	}
}

func cloneResults(in []domain.SearchResult) []domain.SearchResult {
	out := make([]domain.SearchResult, len(in))
	copy(out, in)
	return out
}

// rankFixture runs the deterministic part of searchByType over the fixture: RRF
// fusion, the Stage-A blend, and lane assembly, and renders the outcome with full
// float precision less the last few bits: chunkCoherence sums over a Go map, so the
// last ulp of a pre-existing score varies run to run (T-20261103-chunkcoherence-float-sum-depends-on-go-map);
// 12 significant digits is far below any ranking-relevant difference.
func rankFixture(t *testing.T, q *QueryService, ctx context.Context, fx rankingFixture, topK int) string {
	t.Helper()
	fused := rrfFuse(cloneResults(fx.dense), cloneResults(fx.lexical), 60, 100, 1)
	primary := map[string]bool{}
	for _, r := range fused {
		primary[r.Document.ID] = true
	}
	blended := q.applyStageABlend(ctx, fused, fx.queryVec)
	out := assembleLanes(blended, primary, topK, false, nil)
	var b strings.Builder
	for _, r := range out {
		fmt.Fprintf(&b, "%s score=%s raw=%s lex=%s\n", r.Document.ID,
			strconv.FormatFloat(r.Score, 'g', 12, 64),
			strconv.FormatFloat(r.RawScore, 'g', 12, 64),
			strconv.FormatFloat(r.LexicalScore, 'g', 12, 64))
	}
	return b.String()
}

// goldenConfigs are the flag-off configurations the golden pins: every Stage-A
// weight non-zero, under both recency curves, with and without a reference time.
func goldenRankingOutput(t *testing.T, setup ...func(*QueryService)) string {
	fx := newRankingFixture()
	w := BlendWeights{Cosine: 0.4, Lexical: 0.2, GraphCoherence: 0.05, Confidence: 0.1, PageRank: 0.05, Recency: 0.1, Activation: 0.05}
	var b strings.Builder
	for _, curve := range []RecencyCurve{{}, {Decay: true, HalfLife: 90 * 24 * time.Hour}} {
		for _, ref := range []time.Time{{}, time.Date(2023, 5, 3, 0, 0, 0, 0, time.UTC)} {
			q := &QueryService{}
			bl := NewBlender(w)
			q.EnableBlend(&bl, fx.signals)
			q.chunkTriplets = fx.triplets
			q.SetRecencyCurve(curve)
			for _, f := range setup {
				f(q)
			}
			ctx := context.Background()
			if !ref.IsZero() {
				ctx = domain.WithEventTimeQuery(ctx, domain.EventTimeQuery{ReferenceTime: ref})
			}
			fmt.Fprintf(&b, "## decay=%t ref=%s\n", curve.Decay, ref.Format(time.RFC3339))
			b.WriteString(rankFixture(t, q, ctx, fx, 6))
		}
	}
	return b.String()
}

// TestRankingV2_FlagsOffIsByteIdentical is the ADR-0136 "behaviour unchanged by
// default" gate: with stage_a_calibrated and temporal_intent_enabled at their
// defaults, fusion + Stage A + assembly produce
// exactly the order, and the scores to 12 significant digits, that the
// pre-ADR-0136 code produced on the fixture pool (12, not 17: see rankFixture).
func TestRankingV2_FlagsOffIsByteIdentical(t *testing.T) {
	got := goldenRankingOutput(t)
	path := filepath.Join("testdata", "ranking_v2_flags_off.golden")
	if *updateRankingGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Fatalf("flags-off ranking changed.\n--- want\n%s\n--- got\n%s", want, got)
	}
}

// ADR-0136 D4/D5 (A1: a substrate-backed reader) flags-off: a service holding a fact reader, with every fact tunable
// set but the lane and supersession OFF, ranks the golden pool byte-identically.
func TestRankingV2_FactLayerOffIsByteIdentical(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "ranking_v2_flags_off.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	got := goldenRankingOutput(t, func(q *QueryService) {
		q.facts = newMemSubstrate()
		q.rankingV2.FactLaneTopK, q.rankingV2.FactMatchWeight, q.rankingV2.CurrentFactWeight = 20, 0.2, 0.1
		// ADR-0136 A4: the scoped lane's tunables set, but it runs only inside the lane.
		q.rankingV2.FactLaneScopedDocuments, q.rankingV2.FactLaneScopedTopK = 5, 20
	})
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Fatalf("the fact layer, switched off, changed the ranking.\n--- got\n%s", got)
	}
}
