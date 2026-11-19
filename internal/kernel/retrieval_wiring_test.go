package kernel

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory"
)

// ENG-1320: the QueryService lanes moved from app/app.go into NewMemoryStack
// (composeRetrieval) and ComposeLateRetrieval. These tests pin that the composed
// QueryService is wired exactly as the pre-move boot path wired it: the reference
// below replays the pre-ENG-1320 sequence verbatim — NewMemoryStack's own setters,
// then every mem.QueryService.Enable*/Set* call app.go made, in its order — and the
// two services must agree on every field.

type fakeReranker struct{}

func (fakeReranker) Rerank(context.Context, string, []string) ([]float64, error) { return nil, nil }

type fakePlanner struct{}

func (fakePlanner) PlanQuery(context.Context, string, string, []string, int) (string, error) {
	return "", nil
}
func (fakePlanner) DecideContinue(context.Context, string, []string, []string) (bool, string, error) {
	return true, "", nil
}
func (fakePlanner) Synthesize(context.Context, string, []string) (string, string, error) {
	return "", "", nil
}

type fakeObserver struct{}

func (fakeObserver) ObserveRetrieval(domain.RetrievalDecision) {}

type fakeConsultant struct{}

func (fakeConsultant) Consult(context.Context, string, string) ([]domain.SubstrateCitation, error) {
	return nil, nil
}

// legacyWiredQueryService is the pre-ENG-1320 wiring, copied from
// internal/kernel/memory_stack.go and app/app.go at core e9acab6 (the setter calls
// only; boot lines dropped). Do not "fix" it to match the new code: it is the oracle.
func legacyWiredQueryService(backend MemoryBackend, execCfg config.ExecutionConfig, embedModel string,
	obs domain.DecisionObserver, consultant domain.SubstrateConsultant, rr memory.Reranker, pl memory.Planner) *memory.QueryService {
	scopedRead := authz.NewEnforcingVectorStore(backend.Vector, slog.Default())
	qs := memory.NewQueryServiceWithFacts(nil, scopedRead, nil, nil)
	// memory_stack.go
	qs.SetRelevanceFloor(execCfg.Retrieval.RecallSimilarityFloor)
	qs.SetRecallSizes(execCfg.Retrieval.RecallTopK, execCfg.Retrieval.RecallOverFetch)
	if gs := backend.Graph; gs != nil {
		spEngine := memory.NewSpreadingEngine(gs, scopedRead,
			execCfg.Graph.DecayFactor, execCfg.Graph.MaxDepth, execCfg.Graph.EnergyFloor)
		if execCfg.Retrieval.RecallSpreadingEnabled {
			qs.EnableSpreading(spEngine)
			qs.SetSpreadingKeepsAssemblyOrder(execCfg.Retrieval.RecallSpreadingKeepAssemblyOrder)
		}
		if execCfg.Retrieval.HebbianEnabled {
			qs.EnableHebbian(gs,
				execCfg.Retrieval.HebbianLearningRate, execCfg.Retrieval.HebbianMaxWeight, execCfg.Retrieval.HebbianCoActivationFloor,
				execCfg.Retrieval.HebbianDecayPerDay, execCfg.Retrieval.HebbianBaseWeight, execCfg.Retrieval.HebbianTopN)
		}
	}
	// app.go
	cfg := config.Config{Execution: execCfg}
	cfg.Embedder.Model = embedModel
	qs.SetDecisionObserver(obs, cfg.Execution.RetrievalFingerprint(cfg.Embedder.Model))
	qs.SetSubstrateConsultant(consultant)
	if cfg.Execution.Retrieval.KG2RAGEnabled {
		qs.EnableKG2RAG(backend.ChunkTriplets,
			cfg.Execution.Retrieval.KG2RAGMaxHops,
			cfg.Execution.Retrieval.KG2RAGMaxExpanded,
			cfg.Execution.Retrieval.KG2RAGMaxEntities,
			cfg.Execution.Retrieval.KG2RAGPerEntity,
		)
		if cfg.Execution.Retrieval.QueryEntitySeedingEnabled {
			qs.EnableQueryEntitySeeding()
		}
		if cfg.Execution.Retrieval.AnchorConstraintEnabled {
			qs.EnableAnchorConstraint()
		}
	}
	if cfg.Execution.Retrieval.BlendEnabled {
		w := memory.BlendWeights{
			Cosine:         cfg.Execution.Retrieval.BlendWeightCosine,
			Recency:        cfg.Execution.Retrieval.BlendWeightRecency,
			Confidence:     cfg.Execution.Retrieval.BlendWeightConfidence,
			PageRank:       cfg.Execution.Retrieval.BlendWeightPageRank,
			Activation:     cfg.Execution.Retrieval.BlendWeightActivation,
			Lexical:        cfg.Execution.Retrieval.BlendWeightLexical,
			GraphCoherence: cfg.Execution.Retrieval.BlendWeightCoherence,
		}
		if (w == memory.BlendWeights{}) {
			w = memory.DefaultBlendWeights()
		}
		blender := memory.NewBlender(w)
		qs.EnableBlend(&blender, backend.Vector.(memory.RankSignalStore))
		qs.SetRecencyCurve(memory.RecencyCurve{
			Decay:    cfg.Execution.Retrieval.BlendRecencyMode == "decay",
			HalfLife: time.Duration(cfg.Execution.Retrieval.BlendRecencyHalfLifeHours * float64(time.Hour)),
		})
	}
	rv2 := cfg.Execution.Retrieval
	fc := cfg.Execution.Facts
	qs.SetRankingV2(memory.RankingV2{
		Calibrated:              rv2.StageACalibrated,
		LexicalSaturationK:      rv2.LexicalSaturationK,
		TemporalIntent:          rv2.TemporalIntentEnabled,
		TemporalFitWeight:       rv2.TemporalFitWeight,
		FactLane:                fc.LaneEnabled,
		FactLaneTopK:            fc.LaneTopK,
		FactLaneScopedDocuments: fc.LaneScopedDocuments,
		FactLaneScopedTopK:      fc.LaneScopedTopK,
		FactMatchWeight:         fc.FactMatchWeight,
		Supersession:            fc.SupersessionEnabled,
		CurrentFactWeight:       fc.CurrentFactWeight,
		FactChunkCosineMax:      fc.LaneChunkCosineMax,
	})
	if cfg.Execution.Retrieval.HybridSearchEnabled {
		qs.EnableHybrid(backend.Lexical, cfg.Execution.Retrieval.HybridRRFK)
		qs.SetLexicalWeight(cfg.Execution.Retrieval.HybridLexicalWeight)
	}
	if cfg.Execution.Retrieval.NeighborWindowEnabled {
		qs.EnableNeighborWindow()
	}
	if cfg.Execution.Retrieval.DocumentCompletionEnabled {
		qs.EnableDocumentCompletion(cfg.Execution.Retrieval.DocumentCompletionDocuments, cfg.Execution.Retrieval.DocumentCompletionMaxChunks)
	}
	if cfg.Execution.Retrieval.ChronologicalContextEnabled {
		qs.EnableChronologicalContext()
	}
	if cfg.Execution.Retrieval.RerankerEnabled {
		qs.EnableReranker(rr, cfg.Execution.Retrieval.RerankerTopK, cfg.Execution.Retrieval.RerankerWeight)
	}
	if cfg.Execution.Retrieval.StructureGraphEnabled {
		qs.EnableSectionScopedRetrieval(backend.Vector.(memory.SectionScopedStore), cfg.Execution.Retrieval.SectionScopeCues)
	}
	if r := cfg.Execution.Retrieval; len(r.SupersededMarkers) > 0 && r.SupersededRankFactor > 0 && r.SupersededRankFactor < 1 {
		qs.EnableSupersededDemotion(r.SupersededMarkers, r.SupersededRankFactor)
	}
	if cfg.Execution.Retrieval.AgenticRetrievalEnabled {
		qs.EnableAgenticRetrieval(pl, cfg.Execution.Retrieval.AgenticMaxHops)
		qs.SetDecomposeEnabled(cfg.Execution.Retrieval.AgenticDecomposeEnabled)
		qs.SetSubQuestionConcurrency(cfg.Execution.Retrieval.AgenticSubQuestionConcurrency)
		qs.SetAdaptiveRoutesEnabled(cfg.Execution.Retrieval.AgenticAdaptiveRoutesEnabled)
	}
	return qs
}

// laneSnapshot renders every field of a QueryService: a reference field (pointer,
// interface, chan, func, map) as nil or its dynamic type — two services built from the
// same inputs hold different instances — and every value field as its value. The
// blender is compared by its live weights; queryCounter is runtime state.
func laneSnapshot(t *testing.T, qs *memory.QueryService) map[string]string {
	t.Helper()
	v := reflect.ValueOf(qs).Elem()
	out := map[string]string{}
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		f := v.Field(i)
		switch name {
		case "queryCounter":
			continue
		case "blender":
			out[name] = fmt.Sprintf("%+v", qs.CurrentBlendWeights())
			continue
		}
		switch f.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Chan, reflect.Func, reflect.Map, reflect.Slice:
			if f.IsNil() {
				out[name] = "nil"
			} else if f.Kind() == reflect.Interface {
				out[name] = "set:" + f.Elem().Type().String()
			} else {
				out[name] = "set:" + f.Type().String()
			}
		default:
			out[name] = fmt.Sprintf("%+v", f)
		}
	}
	return out
}

func allLanesOn() config.ExecutionConfig {
	e := config.DefaultConfig().Execution
	r := &e.Retrieval
	r.RecallSpreadingEnabled, r.RecallSpreadingKeepAssemblyOrder, r.HebbianEnabled = true, true, true
	r.KG2RAGEnabled, r.QueryEntitySeedingEnabled, r.AnchorConstraintEnabled = true, true, true
	r.KG2RAGMaxHops, r.KG2RAGMaxExpanded, r.KG2RAGMaxEntities, r.KG2RAGPerEntity = 2, 31, 41, 7
	r.BlendEnabled = true
	r.BlendWeightCosine, r.BlendWeightRecency, r.BlendWeightLexical = 0.6, 0.1, 0.05
	r.BlendRecencyMode, r.BlendRecencyHalfLifeHours = "decay", 72
	r.StageACalibrated, r.TemporalIntentEnabled, r.LexicalSaturationK, r.TemporalFitWeight = true, true, 3, 0.2
	r.HybridSearchEnabled, r.HybridRRFK, r.HybridLexicalWeight = true, 55, 1.5
	r.NeighborWindowEnabled, r.ChronologicalContextEnabled, r.StructureGraphEnabled = true, true, true
	r.DocumentCompletionEnabled, r.DocumentCompletionDocuments, r.DocumentCompletionMaxChunks = true, 3, 12
	r.RerankerEnabled, r.RerankerTopK, r.RerankerWeight = true, 17, 0.4
	r.AgenticRetrievalEnabled, r.AgenticMaxHops, r.AgenticDecomposeEnabled = true, 3, true
	r.AgenticSubQuestionConcurrency, r.AgenticAdaptiveRoutesEnabled = 4, true
	r.SupersededMarkers, r.SupersededRankFactor = []string{"superseded", "retired"}, 0.4
	f := &e.Facts
	f.LaneEnabled, f.LaneTopK, f.LaneScopedDocuments, f.LaneScopedTopK = true, 9, 2, 6
	f.FactMatchWeight, f.SupersessionEnabled, f.CurrentFactWeight, f.LaneChunkCosineMax = 0.3, true, 0.25, true
	return e
}

func allLanesOff() config.ExecutionConfig {
	e := config.DefaultConfig().Execution
	r := &e.Retrieval
	r.RecallSpreadingEnabled, r.HebbianEnabled, r.KG2RAGEnabled, r.BlendEnabled = false, false, false, false
	r.HybridSearchEnabled, r.NeighborWindowEnabled, r.ChronologicalContextEnabled = false, false, false
	r.StructureGraphEnabled, r.DocumentCompletionEnabled, r.RerankerEnabled, r.AgenticRetrievalEnabled = false, false, false, false
	r.SupersededMarkers = nil
	e.Facts.LaneEnabled = false
	return e
}

func TestComposedRetrieval_WiresEveryLaneAsThePreMoveBootPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec config.ExecutionConfig
	}{
		{"defaults", config.DefaultConfig().Execution},
		{"all lanes on", allLanesOn()},
		{"all lanes off", allLanesOff()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := NewMemoryBackend(&postgres.PgVectorAdapter{})
			deps := RetrievalDeps{DecisionObserver: fakeObserver{}, SubstrateConsultant: fakeConsultant{}, EmbedModel: "eng1320-model"}
			mem := NewMemoryStack(backend, nil, nil, tc.exec, nil, config.DefaultConfig().Chunker, nil, FactSubstrate{}, deps)
			ComposeLateRetrieval(mem, tc.exec, LateRetrievalDeps{Reranker: fakeReranker{}, Planner: fakePlanner{}})

			want := laneSnapshot(t, legacyWiredQueryService(backend, tc.exec, deps.EmbedModel,
				deps.DecisionObserver, deps.SubstrateConsultant, fakeReranker{}, fakePlanner{}))
			got := laneSnapshot(t, mem.QueryService)
			var diffs []string
			for k, w := range want {
				if got[k] != w {
					diffs = append(diffs, fmt.Sprintf("%s: got %s, want %s", k, got[k], w))
				}
			}
			sort.Strings(diffs)
			if len(diffs) > 0 {
				t.Fatalf("composed QueryService differs from the pre-ENG-1320 wiring:\n%s", strings.Join(diffs, "\n"))
			}
		})
	}
}

// The all-on case must actually turn lanes on, or the comparison above proves nothing
// about them: every lane field differs from its all-off value.
func TestComposedRetrieval_AllOnDiffersFromAllOffOnEveryLane(t *testing.T) {
	build := func(e config.ExecutionConfig) map[string]string {
		mem := NewMemoryStack(NewMemoryBackend(&postgres.PgVectorAdapter{}), nil, nil, e, nil, config.DefaultConfig().Chunker,
			nil, FactSubstrate{}, RetrievalDeps{})
		ComposeLateRetrieval(mem, e, LateRetrievalDeps{Reranker: fakeReranker{}, Planner: fakePlanner{}})
		return laneSnapshot(t, mem.QueryService)
	}
	on, off := build(allLanesOn()), build(allLanesOff())
	for _, lane := range []string{"spreader", "spreadKeepsOrder", "graphWriter", "heb", "chunkTriplets", "kgHops",
		"queryEntitySeed", "anchorConstraint", "sectionStore", "neighborWindow", "docCompletion", "chronoContext",
		"blender", "recencyCurve", "rankingV2", "rankSignals", "lexical", "rrfK", "lexicalWeight", "decompEnabled",
		"subqConcurrency", "adaptiveRoutes", "reranker", "rerankTopK", "rerankWeight", "agenticEnabled", "planner",
		"agenticMaxHops", "superseded"} {
		if _, ok := on[lane]; !ok {
			t.Errorf("QueryService has no field %q any more; update this list", lane)
			continue
		}
		if on[lane] == off[lane] {
			t.Errorf("lane %s is %s with every lane on and with every lane off", lane, on[lane])
		}
	}
}

// A partial backend (a test fake) leaves the lanes whose capability it lacks off
// instead of wiring a nil store.
func TestComposedRetrieval_PartialBackendLeavesCapabilityLanesOff(t *testing.T) {
	mem := NewMemoryStack(MemoryBackend{Vector: plainVec{}}, nil, nil, allLanesOn(), nil, config.DefaultConfig().Chunker,
		nil, FactSubstrate{}, RetrievalDeps{})
	s := laneSnapshot(t, mem.QueryService)
	for _, lane := range []string{"chunkTriplets", "rankSignals", "lexical", "sectionStore"} {
		if s[lane] != "nil" {
			t.Errorf("%s = %s, want nil without the capability", lane, s[lane])
		}
	}
	if s["queryEntitySeed"] != "false" || s["anchorConstraint"] != "false" {
		t.Errorf("KG²RAG companions need the triplet store: seed=%s anchor=%s", s["queryEntitySeed"], s["anchorConstraint"])
	}
}

func TestRankingV2From_ReadsEveryField(t *testing.T) {
	e := allLanesOn()
	got := rankingV2From(e.Retrieval, e.Facts)
	v := reflect.ValueOf(got)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("RankingV2.%s is zero with every source field set: not read from config", v.Type().Field(i).Name)
		}
	}
}
