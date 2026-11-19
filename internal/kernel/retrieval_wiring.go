package kernel

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/memory"
)

// RetrievalDeps are the recall lanes' dependencies that are neither configuration nor
// a capability of the raw adapter (MemoryBackend): the plugin hooks app.Options carries
// and the embedder identity. NewMemoryStack takes them as a constructor argument and
// composes every config-and-store lane of the QueryService itself (AGENTS.md §2.1;
// ENG-1320). The zero value is the OSS default: no decision observer, no substrate
// consultant, an empty embedder model in the retrieval fingerprint.
type RetrievalDeps struct {
	// DecisionObserver receives the ADR-0103 decision-provenance record of every
	// recall. nil (the OSS default) leaves the emit site a nil check.
	DecisionObserver domain.DecisionObserver
	// SubstrateConsultant is the ADR-0118 D5 fact-lane substrate consultant. nil (the
	// OSS default) leaves the consult site a nil check and behaviour bit-identical.
	SubstrateConsultant domain.SubstrateConsultant
	// EmbedModel names the embedder (embedder.model) for the ADR-0103 retrieval
	// fingerprint.
	EmbedModel string
}

// LateRetrievalDeps are the lanes that call system agents through the agent transport.
// The transport belongs to the metabolism stack, which is built FROM the memory stack
// (it takes mem.Reader, mem.ProfileStore, mem.Agent), so these cannot be constructor
// arguments of NewMemoryStack; ComposeLateRetrieval attaches them once, right after
// the metabolism stack exists. They are ranking features, not access controls
// (card.core.retrieval-query): an unattached one leaves recall at the Stage-A order
// and the single pass, never less enforced.
type LateRetrievalDeps struct {
	// Reranker is the ADR-0054 Stage-B cross-encoder (subnetwork.RerankerDispatcher
	// over the reranker_agent). Used only when execution.retrieval.reranker_enabled.
	Reranker memory.Reranker
	// Planner is the agentic retrieval query-planner (subnetwork.RetrievalDispatcher
	// over the retrieval_agent). Used only when execution.retrieval.agentic_retrieval_enabled.
	Planner memory.Planner
}

// composeRetrieval switches on every QueryService lane whose inputs are configuration,
// a capability of the raw adapter, or a RetrievalDeps hook. It used to be spread over
// app/app.go as post-construction setters about 1,400 lines away from NewMemoryStack;
// the blocks below are those, in their original order and with their original boot
// lines (the expect_log anchors of experiment arms). A capability a partial backend
// lacks (a test fake; reported by reportMissing) leaves its lane off, as NewMemoryStack
// does for the spreading engine and the triplet batcher.
//
// Rejected: making each lane a QueryService constructor parameter. These are ranking
// features with nil-safe defaults, not security controls (AGENTS.md §2.3 is about
// enforcement); one composition point is the property ENG-1320 asks for, and the
// setters keep the memory package's own tests able to turn one lane on at a time.
func composeRetrieval(qs *memory.QueryService, execCfg config.ExecutionConfig, backend MemoryBackend, deps RetrievalDeps) {
	rc := execCfg.Retrieval

	// ADR-0103 D3/D7: wire the decision-provenance seam. nil observer (the OSS default)
	// leaves the emit site a nil check. The fingerprint is computed ONCE here, not per
	// query: it describes configuration that cannot change without a restart, so hashing
	// it on the retrieval path would be pure cost. The embedder is identified by model
	// name — its dimension is implied by the model and enforced by the pgvector schema,
	// so there is no separate dimension field to fold in.
	qs.SetDecisionObserver(deps.DecisionObserver, execCfg.RetrievalFingerprint(deps.EmbedModel))
	// ADR-0118 D5: the fact-lane substrate consultant. nil (the OSS default)
	// leaves the consult site a nil check and behaviour bit-identical.
	qs.SetSubstrateConsultant(deps.SubstrateConsultant)

	// ADR-0053 Phase 0: KG²RAG one-hop chunk expansion (config-gated). The
	// pgvector adapter doubles as the ChunkTripletsStore (per-chunk h, r, t
	// extracted at write time or via the offline chunk-fill CLI). The
	// pipeline walks per-chunk triplets from the seed chunks, pulls in
	// chunks that share entities, and feeds them into the same cosine
	// re-rank. Opt-in via `execution.kg2rag_enabled` in config.json so the
	// A/B test (KG²RAG on vs off) is a config flip, not a rebuild. The
	// max-hops / max-expanded / max-entities knobs bound the expansion.
	if rc.KG2RAGEnabled {
		if backend.ChunkTriplets != nil {
			qs.EnableKG2RAG(backend.ChunkTriplets,
				rc.KG2RAGMaxHops,
				rc.KG2RAGMaxExpanded,
				rc.KG2RAGMaxEntities,
				rc.KG2RAGPerEntity,
			)
			// LLM-free, structure-aware recall: seed kgExpand from entities extracted
			// from the query text (ADR-0053). Needs KG²RAG (the chunk_triplets store).
			if rc.QueryEntitySeedingEnabled {
				qs.EnableQueryEntitySeeding()
				slog.Info("ADR-0053: query-entity seeding ENABLED (LLM-free recall)")
			}
			// Document-local anchor promotion (companion to the deterministic anchor
			// tier). Also needs the chunk_triplets store; LLM-free. ADR-0053.
			if rc.AnchorConstraintEnabled {
				qs.EnableAnchorConstraint()
				slog.Info("ADR-0053: anchor constraint ENABLED (document-local anchor promotion)")
			}
		}
	} else {
		slog.Info("ADR-0053: KG²RAG expansion DISABLED via config (kg2rag_enabled=false)")
	}

	// ADR-0054 Stage A: multi-signal blend re-rank (cosine + recency + confidence
	// + pagerank + activation). On by default; `execution.retrieval.blend_enabled`
	// = false turns it off. Reads
	// chunk_pagerank (kept fresh by the pagerank-recompute worker) + per-chunk
	// confidence; bge cross-encoder (Stage B) is a separate, later flag.
	if rc.BlendEnabled && backend.RankSignals != nil {
		w := memory.BlendWeights{
			Cosine:         rc.BlendWeightCosine,
			Recency:        rc.BlendWeightRecency,
			Confidence:     rc.BlendWeightConfidence,
			PageRank:       rc.BlendWeightPageRank,
			Activation:     rc.BlendWeightActivation,
			Lexical:        rc.BlendWeightLexical,
			GraphCoherence: rc.BlendWeightCoherence,
		}
		if (w == memory.BlendWeights{}) {
			w = memory.DefaultBlendWeights() // all-unset ⇒ ADR defaults
		}
		blender := memory.NewBlender(w)
		qs.EnableBlend(&blender, backend.RankSignals)
		curve := memory.RecencyCurve{
			Decay:    rc.BlendRecencyMode == "decay",
			HalfLife: time.Duration(rc.BlendRecencyHalfLifeHours * float64(time.Hour)),
		}
		qs.SetRecencyCurve(curve)
		slog.Info("ADR-0054: Stage-A multi-signal blend ENABLED", "weights", w,
			"recency_mode", map[bool]string{false: "minmax", true: "decay"}[curve.Decay],
			"recency_half_life_h", rc.BlendRecencyHalfLifeHours)
	}

	// ADR-0136 ranking v2 (Phase 0): D1 calibrated Stage A and D3 query-conditioned
	// time (D2, a per-document cap, was rejected by the owner 2026-11-03 and removed).
	// Wired unconditionally — every switch defaults off and the zero RankingV2 is the
	// old ranking — so the boot line below states the EFFECTIVE value of each, which
	// is what an experiment arm's expect_log checks. D1 and the D3 features act inside
	// Stage A, so they need blend_enabled; D3's explicit window acts without it. The
	// line says which applies. (The fact layer's own boot line reports the composed
	// stack and stays with app.go's other boot reporting.)
	fc := execCfg.Facts
	qs.SetRankingV2(rankingV2From(rc, fc))
	slog.Info(fmt.Sprintf("ranking v2: stage_a_calibrated=%t temporal_intent_enabled=%t",
		rc.StageACalibrated, rc.TemporalIntentEnabled),
		"lexical_saturation_k", rc.LexicalSaturationK,
		"temporal_fit_weight", rc.TemporalFitWeight,
		"blend_enabled", rc.BlendEnabled)
	if (rc.StageACalibrated || rc.TemporalIntentEnabled) && !rc.BlendEnabled {
		slog.Warn("ranking v2: stage_a_calibrated / temporal_intent_enabled change Stage-A scoring, which is OFF (blend_enabled=false); only the explicit-range window of temporal intent applies")
	}
	if rc.TemporalIntentEnabled && rc.BlendWeightRecency == 0 {
		slog.Info("ranking v2: temporal intent order (latest/current/earliest) sets the recency direction, but blend_weight_recency=0, so only the temporal-fit feature and explicit windows take effect")
	}

	// ADR-0054 hybrid retrieval: fuse dense (vector) + sparse (lexical/full-text)
	// via RRF so exact-token chunks the embedder misses enter the pool. Opt-in via
	// execution.hybrid_search_enabled. The lexical half is the typed MemoryBackend
	// field (ENG-1504), not a runtime probe of the adapter.
	if rc.HybridSearchEnabled && backend.Lexical != nil {
		qs.EnableHybrid(backend.Lexical, rc.HybridRRFK)
		qs.SetLexicalWeight(rc.HybridLexicalWeight)
		slog.Info("ADR-0054: hybrid dense+lexical retrieval ENABLED", "rrf_k", rc.HybridRRFK, "lexical_weight", rc.HybridLexicalWeight)
	}

	if rc.NeighborWindowEnabled {
		qs.EnableNeighborWindow()
		slog.Info("ADR-0060: neighbor-window expansion ENABLED")
	}
	if rc.DocumentCompletionEnabled {
		qs.EnableDocumentCompletion(rc.DocumentCompletionDocuments, rc.DocumentCompletionMaxChunks)
		slog.Info("ENG-430: document/session completion ENABLED",
			"documents", rc.DocumentCompletionDocuments, "max_chunks", rc.DocumentCompletionMaxChunks)
	}
	if rc.ChronologicalContextEnabled {
		qs.EnableChronologicalContext()
		slog.Info("ADR-0134 D5a: chronological answer context ENABLED (passages ordered and dated by event time)")
	}

	// ADR-0060: the structure-graph section boost on the query side. The ingest side
	// (the docling parse that fills the sections) needs the agent transport and stays
	// in app.go under the same flag, which also logs the feature's boot line.
	if rc.StructureGraphEnabled && backend.Sections != nil {
		qs.EnableSectionScopedRetrieval(backend.Sections, rc.SectionScopeCues)
		slog.Info("ADR-0060: section-scoped retrieval ENABLED", "title_cues", rc.SectionScopeCues)
	}

	// ENG-2158: superseded revisions rank below their successors. ENG-2170: the
	// markers are read from a row's labels (and, for older rows, its tags).
	if len(rc.SupersededMarkers) > 0 && rc.SupersededRankFactor > 0 && rc.SupersededRankFactor < 1 {
		qs.EnableSupersededDemotion(rc.SupersededMarkers, rc.SupersededRankFactor)
		slog.Info("ENG-2158: superseded-revision demotion ENABLED",
			"markers", rc.SupersededMarkers, "factor", rc.SupersededRankFactor)
	}
}

// rankingV2From maps execution.retrieval and execution.facts onto the ADR-0136 ranking
// v2 settings. A function of its own so a test can prove each field is read
// (AGENTS.md §3.4).
func rankingV2From(rc config.RetrievalConfig, fc config.FactsConfig) memory.RankingV2 {
	return memory.RankingV2{
		Calibrated:         rc.StageACalibrated,
		LexicalSaturationK: rc.LexicalSaturationK,
		TemporalIntent:     rc.TemporalIntentEnabled,
		TemporalFitWeight:  rc.TemporalFitWeight,
		// ADR-0136 D4/D5: the fact lane and its features (execution.facts).
		FactLane:     fc.LaneEnabled,
		FactLaneTopK: fc.LaneTopK,
		// ADR-0136 A4: the document-scoped lane (0 = off). Acts only inside the lane.
		FactLaneScopedDocuments: fc.LaneScopedDocuments,
		FactLaneScopedTopK:      fc.LaneScopedTopK,
		FactMatchWeight:         fc.FactMatchWeight,
		Supersession:            fc.SupersessionEnabled,
		CurrentFactWeight:       fc.CurrentFactWeight,
		// ENG-432: fact-reached chunks rank on max(own, matched fact) cosine. Off by
		// default (owner 2026-11-15) until its A/B/A.
		FactChunkCosineMax: fc.LaneChunkCosineMax,
	}
}

// ComposeLateRetrieval attaches the lanes that call system agents through the agent
// transport (LateRetrievalDeps). app.go calls it exactly once, right after the
// metabolism stack (which owns the transport) is built; it is the only other place
// QueryService lanes are switched on at boot. The blocks are the ones app.go held,
// with their boot lines.
func ComposeLateRetrieval(mem *MemoryStack, execCfg config.ExecutionConfig, deps LateRetrievalDeps) {
	qs := mem.QueryService
	rc := execCfg.Retrieval

	// ADR-0054 Stage B: cross-encoder rerank of the top-K Stage-A candidates via
	// the warm reranker_agent system organ (bge cross-encoder), invoked DIRECTLY
	// through the agent transport (no selection) — the same privileged-organ pattern as
	// the kg_extractor. Opt-in via execution.reranker_enabled. Fail-soft: a
	// down/erroring agent leaves the Stage-A order intact. The model id is the
	// agent's RERANK_MODEL env (large = ceiling, base/v2-m3 = CPU edge).
	if rc.RerankerEnabled {
		qs.EnableReranker(deps.Reranker, rc.RerankerTopK, rc.RerankerWeight)
		slog.Info("ADR-0054: Stage-B cross-encoder rerank ENABLED",
			"top_k", rc.RerankerTopK, "w_bge", rc.RerankerWeight)
	}

	// AGENTIC_RETRIEVAL_SPEC Phase 2a: wire the LLM query-planner (retrieval_agent)
	// as the QueryService's Planner — invoked directly via the agent transport (no
	// selection) with a managed LLM session, the same privileged-organ + session
	// privileged-organ pattern. Default off; fail-open to the single pass when the
	// agent is unreachable or the config flag is unset.
	if rc.AgenticRetrievalEnabled {
		qs.EnableAgenticRetrieval(deps.Planner, rc.AgenticMaxHops)
		qs.SetDecomposeEnabled(rc.AgenticDecomposeEnabled)
		qs.SetSubQuestionConcurrency(rc.AgenticSubQuestionConcurrency)
		qs.SetAdaptiveRoutesEnabled(rc.AgenticAdaptiveRoutesEnabled) // ADR-0135
		slog.Info("AGENTIC_RETRIEVAL_SPEC: agentic retrieval query-planner ENABLED (retrieval_agent)", "decompose", rc.AgenticDecomposeEnabled, "subquestion_concurrency", rc.AgenticSubQuestionConcurrency, "adaptive_routes", rc.AgenticAdaptiveRoutesEnabled)
	}
}
