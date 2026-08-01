package config

import "testing"

// The fingerprint's whole job is to be a stable identity for a ranking function
// (ADR-0103 D7). Two properties matter and they pull against each other: it must
// change when the ranking changes, and it must NOT change when anything else does.

func TestRetrievalFingerprint_StableForSameConfig(t *testing.T) {
	c := ExecutionConfig{Retrieval: RetrievalConfig{RecallTopK: 10, BlendEnabled: true, BlendWeightCosine: 0.4}}
	if a, b := c.RetrievalFingerprint("bge-large"), c.RetrievalFingerprint("bge-large"); a != b {
		t.Fatalf("fingerprint not deterministic: %s != %s", a, b)
	}
}

func TestRetrievalFingerprint_ChangesWithRankingInputs(t *testing.T) {
	base := ExecutionConfig{Retrieval: RetrievalConfig{RecallTopK: 10, BlendEnabled: true, BlendWeightCosine: 0.4}}
	baseFP := base.RetrievalFingerprint("bge-large")

	// Each of these genuinely changes what comes back or in what order, so each must
	// produce a different identity. A caught duplicate here means a receipt would
	// claim two different rankings were produced by the same configuration.
	cases := map[string]ExecutionConfig{
		"top_k":        {Retrieval: RetrievalConfig{RecallTopK: 20, BlendEnabled: true, BlendWeightCosine: 0.4}},
		"blend_off":    {Retrieval: RetrievalConfig{RecallTopK: 10, BlendEnabled: false, BlendWeightCosine: 0.4}},
		"blend_weight": {Retrieval: RetrievalConfig{RecallTopK: 10, BlendEnabled: true, BlendWeightCosine: 0.5}},
		"kg2rag":       {Retrieval: RetrievalConfig{RecallTopK: 10, BlendEnabled: true, BlendWeightCosine: 0.4, KG2RAGEnabled: true}},
		"anchor":       {Retrieval: RetrievalConfig{RecallTopK: 10, BlendEnabled: true, BlendWeightCosine: 0.4, AnchorConstraintEnabled: true}},
		"hybrid":       {Retrieval: RetrievalConfig{RecallTopK: 10, BlendEnabled: true, BlendWeightCosine: 0.4, HybridSearchEnabled: true}},
		"rrf_k":        {Retrieval: RetrievalConfig{RecallTopK: 10, BlendEnabled: true, BlendWeightCosine: 0.4, HybridRRFK: 60}},
		"floor":        {Retrieval: RetrievalConfig{RecallTopK: 10, BlendEnabled: true, BlendWeightCosine: 0.4, RecallSimilarityFloor: 0.25}},
	}
	for name, c := range cases {
		if got := c.RetrievalFingerprint("bge-large"); got == baseFP {
			t.Errorf("%s: fingerprint unchanged (%s) despite a different ranking function", name, got)
		}
	}
}

func TestRetrievalFingerprint_ChangesWithEmbedder(t *testing.T) {
	// The 768→1024 migration moved LoCoMo recall@100 from 0.47 to 0.94 with no flag
	// changing at all — so the embedder must be part of the identity.
	c := ExecutionConfig{Retrieval: RetrievalConfig{RecallTopK: 10}}
	if c.RetrievalFingerprint("bge-large") == c.RetrievalFingerprint("nomic-embed") {
		t.Fatal("fingerprint ignored the embedder")
	}
}

func TestRetrievalFingerprint_IgnoresUnrelatedConfig(t *testing.T) {
	// Reflection over the struct would fold every unrelated field in and invalidate
	// every historical fingerprint on an unrelated change. This test is what keeps
	// the field list explicit.
	a := ExecutionConfig{Retrieval: RetrievalConfig{RecallTopK: 10}}
	b := ExecutionConfig{Memory: MemoryConfig{ExperientialMemoryEnabled: true}, Retrieval: RetrievalConfig{RecallTopK: 10}}
	if a.RetrievalFingerprint("e") != b.RetrievalFingerprint("e") {
		t.Fatal("fingerprint changed on a field that does not affect ranking")
	}
}
