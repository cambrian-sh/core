package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// scavengerRecordingStore records the doc IDs passed to VectorStore.Delete so
// a test can inspect which entities the scavenger pass marked for GC. It
// embeds the package-wide fakeVectorStore so every other domain.VectorStore
// method is satisfied without duplication; only Save (so the doc is visible
// to the scavenger's caller) and Delete (so the call list is observable) are
// overridden.
type scavengerRecordingStore struct {
	fakeVectorStore
	saved   []*domain.Document
	deleted []string
}

func (s *scavengerRecordingStore) Save(_ context.Context, d *domain.Document) error {
	s.saved = append(s.saved, d)
	return nil
}

func (s *scavengerRecordingStore) Delete(_ context.Context, id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}

// newScavengerTestAgent returns an Agent wired to a recording vector store
// and the recording store itself, so a test can drive the production GC pass
// (Agent.decayLoner) and observe which entity rows it would have removed.
func newScavengerTestAgent() (*Agent, *scavengerRecordingStore) {
	store := &scavengerRecordingStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	return agent, store
}

// T-1.13 / ADR-0060 D8: source-document entities (DocTypeMnemonicEntity with
// kind=source_document AND content_cid set) are GC-exempt. They are the
// drill-down targets for chunk recall — the agent follows
// chunk_relations.parent_entity_id → source-doc entity → content_cid → full
// body via ContentStore.Get. Deleting them would break the parent link in
// every chunk_relations row that points at them.
//
// This test drives the production scavenger pass (Agent.decayLoner at
// worker.go:143) with activation/access values that would otherwise satisfy
// the "loner" GC predicate, and asserts the row is left alone.
func TestScavenger_SourceDocumentExempt(t *testing.T) {
	agent, store := newScavengerTestAgent()
	ctx := context.Background()

	sourceDoc := &domain.Document{
		ID:                 "source_doc:docs/a.md",
		DocumentType:       domain.DocTypeMnemonicEntity,
		Text:               "docs/a.md",
		ActivationStrength: 0.1, // below the decayLoner threshold of 0.3
		AccessCount:        0,   // below the decayLoner threshold of 2
		Metadata: map[string]interface{}{
			"kind":        "source_document",
			"source_uri":  "docs/a.md",
			"source_type": "file_drop",
			"title":       "A document",
			"content_cid": "cid-abc",
		},
	}
	if err := store.Save(ctx, sourceDoc); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	processedIDs := map[string]bool{}
	deletedCount := 0
	agent.decayLoner(ctx, *sourceDoc, processedIDs, false, &deletedCount)

	if got := len(store.deleted); got != 0 {
		t.Errorf("source-document entity with content_cid must be GC-exempt (ADR-0060 D8); "+
			"decayLoner deleted %d doc(s): %v", got, store.deleted)
	}
	if deletedCount != 0 {
		t.Errorf("decayLoner reported %d deletions; source_document with content_cid must be exempt", deletedCount)
	}
	if !processedIDs[sourceDoc.ID] {
		t.Errorf("decayLoner must still mark the source-doc entity as processed; processedIDs=%v", processedIDs)
	}
}

// T-1.13 / ADR-0060 D8: the exemption is correctly SCOPED. The full shape
// (kind=source_document AND content_cid set) is required — any
// DocTypeMnemonicEntity that fails either condition is fair game for the
// GC. This proves the exemption isn't over-broad:
//
//   - source_document with NO content_cid  (the offload handle is missing →
//     not a valid drill-down target, so it's GC-eligible).
//   - a different kind (e.g. "entity") even WITH content_cid (the
//     discriminator is what makes it a source doc, not the cid alone).
func TestScavenger_NonSourceDocumentGC(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		cid   string
		docID string
	}{
		{
			name:  "source_document_kind_without_content_cid",
			kind:  "source_document",
			cid:   "",
			docID: "source_doc:no-cid",
		},
		{
			name:  "non_source_kind_with_content_cid",
			kind:  "entity",
			cid:   "cid-abc",
			docID: "file:docs/a.md",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent, store := newScavengerTestAgent()
			ctx := context.Background()

			doc := &domain.Document{
				ID:                 tc.docID,
				DocumentType:       domain.DocTypeMnemonicEntity,
				Text:               "test",
				ActivationStrength: 0.1, // below the decayLoner threshold of 0.3
				AccessCount:        0,   // below the decayLoner threshold of 2
				Metadata: map[string]interface{}{
					"kind": tc.kind,
				},
			}
			if tc.cid != "" {
				doc.Metadata["content_cid"] = tc.cid
			}
			if err := store.Save(ctx, doc); err != nil {
				t.Fatalf("seed Save: %v", err)
			}

			processedIDs := map[string]bool{}
			deletedCount := 0
			agent.decayLoner(ctx, *doc, processedIDs, false, &deletedCount)

			if len(store.deleted) != 1 || store.deleted[0] != tc.docID {
				t.Errorf("DocTypeMnemonicEntity with kind=%q content_cid=%q must be GC'd "+
					"(exemption requires BOTH kind=source_document AND content_cid); "+
					"decayLoner deleted %d doc(s): %v", tc.kind, tc.cid, len(store.deleted), store.deleted)
			}
			if deletedCount != 1 {
				t.Errorf("decayLoner reported %d deletions; want 1 (the non-exempt entity)", deletedCount)
			}
		})
	}
}
