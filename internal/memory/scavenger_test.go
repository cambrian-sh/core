package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
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
	agent.RecordExperiential = true
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

// ADR-0095 D6 / ADR-0049 D2: a record that hangs from an episode
// (Document.ExperienceID → the `exp-<planID>` parent row) is GC-exempt. Deletion
// is the EPISODE's operation — you forget an episode by deleting the
// `experiences` parent and letting the FK cascade take its chunks — so the
// per-row loner heuristic must not reach a parented row.
//
// Experiential records are born at activation 0.1 (scenes up to 0.5 with
// surprise) and are usually never recalled, so they satisfy the loner predicate
// (<0.3 activation, <2 accesses) on their FIRST night.
//
// MUTATION RATIONALE — delete the `doc.ExperienceID != ""` early return in
// decayLoner and every case below flips to deleted=1: the nightly pass shreds
// the rarely-recalled half of every episode while the `experiences` parent
// survives, leaving an episode whose transition log has holes in it. That is
// exactly the contradiction ADR-0049 D2 ("actions are durable, the transition
// log") forbids, and it is invisible at runtime because nothing reads a record
// it no longer has.
func TestScavenger_EpisodeParentedRecordsExempt(t *testing.T) {
	cases := []struct {
		name    string
		docID   string
		docType string
	}{
		{
			name:    "mnemonic_action_parented_to_episode",
			docID:   "action:p-1:step-1",
			docType: domain.DocTypeMnemonicAction,
		},
		{
			name:    "mnemonic_scene_parented_to_episode",
			docID:   "scene:p-1",
			docType: domain.DocTypeMnemonicScene,
		},
		{
			name:    "negative_edge_parented_to_episode",
			docID:   "negedge:p-1",
			docType: domain.DocTypeNegativeEdge,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent, store := newScavengerTestAgent()
			ctx := context.Background()

			doc := &domain.Document{
				ID:                 tc.docID,
				DocumentType:       tc.docType,
				Text:               "did a thing",
				ActivationStrength: 0.1, // below the decayLoner threshold of 0.3
				AccessCount:        0,   // below the decayLoner threshold of 2
				ExperienceID:       "exp-p-1",
				Metadata:           map[string]interface{}{"plan_id": "p-1"},
			}
			if err := store.Save(ctx, doc); err != nil {
				t.Fatalf("seed Save: %v", err)
			}

			processedIDs := map[string]bool{}
			deletedCount := 0
			agent.decayLoner(ctx, *doc, processedIDs, false, &deletedCount)

			if got := len(store.deleted); got != 0 {
				t.Errorf("%s parented to episode %q must be GC-exempt (ADR-0095 D6: retention is the "+
					"episode's operation, not the loner heuristic's); decayLoner deleted %d doc(s): %v",
					tc.docType, doc.ExperienceID, got, store.deleted)
			}
			if deletedCount != 0 {
				t.Errorf("decayLoner reported %d deletions; a record with a live episode parent must be exempt", deletedCount)
			}
			if !processedIDs[doc.ID] {
				t.Errorf("decayLoner must still mark the parented record as processed; processedIDs=%v", processedIDs)
			}
		})
	}
}

// The episode exemption is SCOPED to parentage, not to document type. An
// experiential record with no episode parent (empty ExperienceID — a pre-ADR-0095
// row, or a failure that was not one plan execution) has nobody to govern its
// lifetime, so the loner heuristic remains the only thing that can ever reclaim
// it and must keep working.
//
// MUTATION RATIONALE — widen the guard to `isExperientialDoc(doc)` or to a
// document-type switch and this case flips to deleted=0: unparented experiential
// rows become immortal, and the nightly pass silently stops reclaiming the one
// class of row it is still responsible for. The pair (this test + the parented
// one above) is what pins the exemption to the FK, which is the thing that
// actually cascades.
func TestScavenger_UnparentedExperientialRecordStillGC(t *testing.T) {
	agent, store := newScavengerTestAgent()
	ctx := context.Background()

	doc := &domain.Document{
		ID:                 "action:orphan",
		DocumentType:       domain.DocTypeMnemonicAction,
		Text:               "did a thing outside any plan",
		ActivationStrength: 0.1, // below the decayLoner threshold of 0.3
		AccessCount:        0,   // below the decayLoner threshold of 2
		ExperienceID:       "",  // no episode parent — nothing else governs its lifetime
		Metadata:           map[string]interface{}{},
	}
	if err := store.Save(ctx, doc); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	processedIDs := map[string]bool{}
	deletedCount := 0
	agent.decayLoner(ctx, *doc, processedIDs, false, &deletedCount)

	if len(store.deleted) != 1 || store.deleted[0] != doc.ID {
		t.Errorf("an unparented %s must stay GC-eligible (the exemption is parentage, not type); "+
			"decayLoner deleted %d doc(s): %v", doc.DocumentType, len(store.deleted), store.deleted)
	}
	if deletedCount != 1 {
		t.Errorf("decayLoner reported %d deletions; want 1 (the unparented experiential row)", deletedCount)
	}
}
