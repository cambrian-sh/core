package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// RetrievalFingerprint identifies the configuration that produced a ranking
// (ADR-0103 D7).
//
// A record of WHICH chunks came back cannot answer WHY they were ranked that way,
// and cannot be reproduced, unless the ranking configuration is pinned alongside
// it. Two runs sharing a fingerprint over the same corpus must rank identically —
// that is exactly the property the kernel's determinism invariants already buy
// (temperature-0, deterministic fusion, no model setting weights at query time),
// and this makes it checkable.
//
// It is generic kernel capability, not a premium concept: benchmark runs can
// finally record the configuration that produced a number, which is worth having
// on its own.
//
// The field list is EXPLICIT rather than reflected over the struct, deliberately:
//
//   - Reflection would fold every unrelated ExecutionConfig field into the hash, so
//     an agent-timeout change would invalidate every historical fingerprint and make
//     comparison across runs meaningless.
//   - It forces a decision. A new ranking-relevant flag must be added here by hand,
//     and forgetting is a reviewable omission in the same diff rather than a silent
//     one. Adding a field changes every fingerprint from that build onward, which is
//     correct — the ranking really did become a different function.
func (c ExecutionConfig) RetrievalFingerprint(embedderID string) string {
	var b strings.Builder

	// The embedder is part of the ranking function, not a peripheral detail: the
	// 768→1024 migration moved LoCoMo recall@100 from 0.47 to 0.94 without a single
	// flag changing. Identity and dimension travel together in embedderID.
	fmt.Fprintf(&b, "embedder=%s\n", embedderID)

	// Where the ranking's vectors come from (ADR-0107 stage 3b). The WRITE flag
	// is deliberately absent — it cannot affect a ranking; the READ switch is a
	// different vector source and therefore a different ranking function.
	fmt.Fprintf(&b, "embedding_projection_read=%t\n", c.Retrieval.EmbeddingProjectionRead)

	// Window and floor.
	fmt.Fprintf(&b, "recall_top_k=%d\n", c.Retrieval.RecallTopK)
	fmt.Fprintf(&b, "recall_over_fetch=%d\n", c.Retrieval.RecallOverFetch)
	fmt.Fprintf(&b, "recall_similarity_floor=%g\n", c.Retrieval.RecallSimilarityFloor)

	// Stage toggles, in pipeline order.
	fmt.Fprintf(&b, "hybrid=%t\n", c.Retrieval.HybridSearchEnabled)
	fmt.Fprintf(&b, "hybrid_rrf_k=%d\n", c.Retrieval.HybridRRFK)
	fmt.Fprintf(&b, "kg2rag=%t\n", c.Retrieval.KG2RAGEnabled)
	fmt.Fprintf(&b, "query_entity_seeding=%t\n", c.Retrieval.QueryEntitySeedingEnabled)
	fmt.Fprintf(&b, "anchor=%t\n", c.Retrieval.AnchorConstraintEnabled)
	fmt.Fprintf(&b, "structure=%t\n", c.Retrieval.StructureGraphEnabled)
	fmt.Fprintf(&b, "neighbor_window=%t\n", c.Retrieval.NeighborWindowEnabled)
	fmt.Fprintf(&b, "agentic=%t\n", c.Retrieval.AgenticRetrievalEnabled)

	// Stage-A blend.
	fmt.Fprintf(&b, "blend=%t\n", c.Retrieval.BlendEnabled)
	fmt.Fprintf(&b, "w_cosine=%g\n", c.Retrieval.BlendWeightCosine)
	fmt.Fprintf(&b, "w_lexical=%g\n", c.Retrieval.BlendWeightLexical)
	fmt.Fprintf(&b, "w_coherence=%g\n", c.Retrieval.BlendWeightCoherence)
	fmt.Fprintf(&b, "w_confidence=%g\n", c.Retrieval.BlendWeightConfidence)
	fmt.Fprintf(&b, "w_pagerank=%g\n", c.Retrieval.BlendWeightPageRank)
	fmt.Fprintf(&b, "w_recency=%g\n", c.Retrieval.BlendWeightRecency)
	fmt.Fprintf(&b, "w_activation=%g\n", c.Retrieval.BlendWeightActivation)

	// Stage-B cross-encoder. The model id lives in the reranker agent's environment,
	// not in kernel config, so the fingerprint can only attest that reranking was on
	// and how it was weighted — noted rather than papered over.
	fmt.Fprintf(&b, "reranker=%t\n", c.Retrieval.RerankerEnabled)
	fmt.Fprintf(&b, "reranker_top_k=%d\n", c.Retrieval.RerankerTopK)
	fmt.Fprintf(&b, "reranker_weight=%g\n", c.Retrieval.RerankerWeight)

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
