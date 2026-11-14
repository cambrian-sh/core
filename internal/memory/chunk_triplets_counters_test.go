package memory

import (
	"context"
	"testing"
)

// With an injected (non-LLM) extractor — the kg_extractor system agent in
// production — every batch is an extractor call and none is an LLM call. The old
// single counter reported each batch as an LLM call
// (T-20261103-chunktripletsbatcher-logs-llm-calls-total-for).
func TestChunkTripletsBatcher_AgentExtractorMakesNoLLMCalls(t *testing.T) {
	ext := &scriptedExtractor{}
	b := newFailureBatcher(ext, newLockedTripletStore(), 0)
	if got := b.extractorBackend(); got != "agent" {
		t.Fatalf("extractorBackend = %q, want agent", got)
	}
	b.Start(context.Background())
	w := b.EnqueueTracked(chunkDocs("c", 3))
	if err := w.Wait(waitCtx(t)); err != nil {
		t.Fatalf("wait: %v", err)
	}
	b.Stop()
	if b.ExtractorCalls() == 0 || b.ExtractorCalls() != uint64(ext.calls.Load()) {
		t.Fatalf("ExtractorCalls = %d, extractor saw %d calls", b.ExtractorCalls(), ext.calls.Load())
	}
	if _, _, _, llm, _ := b.Stats(); llm != 0 {
		t.Fatalf("llmCalls = %d with the agent extractor, want 0", llm)
	}
}

// The default extractor is the LLM adapter: its batches count on both counters.
func TestChunkTripletsBatcher_DefaultExtractorIsLLM(t *testing.T) {
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, newLockedTripletStore(), ChunkTripletsBatcherConfig{})
	if got := b.extractorBackend(); got != "llm" {
		t.Fatalf("extractorBackend = %q, want llm", got)
	}
}
