package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Cycle 1 — Tier-2 commitItem writes session_id to Document.Metadata when StepResult.SessionID is set.
func TestTier2_SessionIDTagged_WhenSessionIDProvided(t *testing.T) {
	gen := &mockGenerator{
		response: `[{"relevance":8,"specificity":8,"explicitness":8,"tier":"FULL"}]`,
	}
	agent := testAgentWithMocks(gen)

	err := agent.RecordExecution(context.Background(), domain.StepResult{
		Index:     0,
		Output:    "auth flow analysis complete",
		SessionID: "sess-test-001",
	})
	if err != nil {
		t.Fatalf("RecordExecution: %v", err)
	}

	agent.drainBatch(context.Background())

	store := agent.Manager.Store.(*capturingStore)
	var factDoc *domain.Document
	for _, doc := range store.saved {
		if doc.DocumentType == domain.DocTypeMnemonicFact {
			factDoc = doc
			break
		}
	}
	if factDoc == nil {
		t.Fatal("expected a MnemonicFact document to be saved")
	}
	got, ok := factDoc.Metadata["session_id"]
	if !ok {
		t.Fatalf("expected session_id in Document.Metadata, got: %v", factDoc.Metadata)
	}
	if got != "sess-test-001" {
		t.Errorf("session_id: want %q got %q", "sess-test-001", got)
	}
}

// Cycle 2 — Tier-2 commitItem does NOT write session_id when StepResult.SessionID is empty.
func TestTier2_SessionIDNotTagged_WhenSessionIDEmpty(t *testing.T) {
	gen := &mockGenerator{
		response: `[{"relevance":8,"specificity":8,"explicitness":8,"tier":"FACT_ONLY"}]`,
	}
	agent := testAgentWithMocks(gen)

	err := agent.RecordExecution(context.Background(), domain.StepResult{
		Index:  0,
		Output: "some result without session",
		// SessionID intentionally empty
	})
	if err != nil {
		t.Fatalf("RecordExecution: %v", err)
	}

	agent.drainBatch(context.Background())

	store := agent.Manager.Store.(*capturingStore)
	for _, doc := range store.saved {
		if _, ok := doc.Metadata["session_id"]; ok {
			t.Errorf("session_id should NOT be present when SessionID is empty, found in doc %q", doc.ID)
		}
	}
}

// Cycle 3 — session_id is propagated to SCENE doc as well as FACT doc on FULL tier.
func TestTier2_SessionIDTagged_OnSceneDoc(t *testing.T) {
	gen := &mockGenerator{
		response: `[{"relevance":9,"specificity":9,"explicitness":9,"tier":"FULL"}]`,
	}
	agent := testAgentWithMocks(gen)

	err := agent.RecordExecution(context.Background(), domain.StepResult{
		Index:     1,
		Output:    "analysis result",
		Snapshot:  map[string]string{"key": "context-value"},
		SessionID: "sess-scene-check",
	})
	if err != nil {
		t.Fatalf("RecordExecution: %v", err)
	}

	agent.drainBatch(context.Background())

	store := agent.Manager.Store.(*capturingStore)
	for _, doc := range store.saved {
		sid, ok := doc.Metadata["session_id"]
		if !ok {
			t.Errorf("doc %q (%s) missing session_id in metadata", doc.ID, doc.DocumentType)
			continue
		}
		if sid != "sess-scene-check" {
			t.Errorf("doc %q session_id: want %q got %q", doc.ID, "sess-scene-check", sid)
		}
	}
}

// Cycle 4 — existing Tier-2 FULL/FACT_ONLY/DROP routing is unaffected by session_id addition.
func TestTier2_ExistingTierRouting_UnaffectedBySessionID(t *testing.T) {
	gen := &mockGenerator{
		response: `[
			{"relevance":8,"specificity":8,"explicitness":8,"tier":"FULL"},
			{"relevance":5,"specificity":5,"explicitness":5,"tier":"FACT_ONLY"},
			{"relevance":2,"specificity":2,"explicitness":2,"tier":"DROP"}
		]`,
	}
	agent := testAgentWithMocks(gen)

	for i, out := range []string{"full-result", "fact-result", "drop-result"} {
		_ = agent.RecordExecution(context.Background(), domain.StepResult{
			Index:     i,
			Output:    out,
			Snapshot:  map[string]string{"snap": "val"},
			SessionID: "sess-routing-check",
		})
	}

	agent.drainBatch(context.Background())

	store := agent.Manager.Store.(*capturingStore)
	factCount, sceneCount := 0, 0
	for _, doc := range store.saved {
		switch doc.DocumentType {
		case domain.DocTypeMnemonicFact:
			factCount++
		case domain.DocTypeMnemonicScene:
			sceneCount++
		}
	}
	// ADR-0049 D5: per-step scenes are removed (scenes are plan-wide, written by
	// WritePlanScene). FULL → 1 FACT; FACT_ONLY → 1 FACT; DROP → 0; no per-step scene.
	if factCount != 2 {
		t.Errorf("factCount: want 2 got %d", factCount)
	}
	if sceneCount != 0 {
		t.Errorf("sceneCount: want 0 (per-step scenes removed) got %d", sceneCount)
	}
}
