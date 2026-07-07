package memory

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// captureAllStore is a VectorStore that records EVERY Save call so a test
// can pick out the source-doc entity row and the chunk rows that the
// IngestionManager pushed through Manager.Store + Agent.EnqueueExternal.
type captureAllStore struct {
	fakeVectorStore
	savedDocs []*domain.Document
}

func (c *captureAllStore) Save(_ context.Context, doc *domain.Document) error {
	c.savedDocs = append(c.savedDocs, doc)
	return nil
}

func (c *captureAllStore) SaveBatch(_ context.Context, docs []*domain.Document) error {
	c.savedDocs = append(c.savedDocs, docs...)
	return nil
}

// findByID returns the saved doc whose ID matches, or nil.
func (c *captureAllStore) findByID(id string) *domain.Document {
	for _, d := range c.savedDocs {
		if d != nil && d.ID == id {
			return d
		}
	}
	return nil
}

// findByKind returns the saved entity whose Metadata["kind"] matches.
func (c *captureAllStore) findByKind(kind string) *domain.Document {
	for _, d := range c.savedDocs {
		if d == nil || d.Metadata == nil {
			continue
		}
		if k, _ := d.Metadata["kind"].(string); k == kind {
			return d
		}
	}
	return nil
}

func (c *captureAllStore) facts() []*domain.Document {
	var out []*domain.Document
	for _, d := range c.savedDocs {
		if d != nil && d.DocumentType == domain.DocTypeMnemonicFact {
			out = append(out, d)
		}
	}
	return out
}

func TestIngestionManager_ProcessSync_savesDocumentQAChunksBeforeReturn(t *testing.T) {
	// Given
	store := &captureAllStore{}
	agent := &Agent{
		Manager:    &MemoryManager{Store: store, Embedder: &mockEmbedder{vec: []float32{0.1, 0.2}}},
		pendingCap: 256,
	}
	reg, err := NewRegistry(defaultChunkers(), ChunkerConfig{Default: "option_c"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	im := NewIngestionManagerWithRegistry(
		NewSceneGenerator(&mockLLMGen{response: scenesJSON(1)}),
		&mockEmbedder{vec: []float32{0.1, 0.2}},
		agent,
		reg,
		IngestionConfig{},
	)
	doc := makeDoc(0)
	doc.Body = "The copper listening horn was locked in the bell room.\n\nThe silver hinge map was hidden under the pier."
	doc.Tags = []string{"document-qa", "source_document", "tidebound-archive"}
	doc.Importance = 1.0

	// When
	entityID, err := im.ProcessSync(context.Background(), doc)

	// Then
	if err != nil {
		t.Fatalf("ProcessSync: %v", err)
	}
	if entityID == "" {
		t.Fatal("ProcessSync returned empty source-doc entity id")
	}
	facts := store.facts()
	if len(facts) != 2 {
		t.Fatalf("ProcessSync must save 2 chunk facts before return, got %d", len(facts))
	}
	first := facts[0]
	if first.ID != "tidebound-archive-chunk-1" {
		t.Fatalf("first chunk ID = %q, want tidebound-archive-chunk-1", first.ID)
	}
	if first.Text != "The copper listening horn was locked in the bell room." {
		t.Fatalf("first chunk text was not saved raw; got %q", first.Text)
	}
	if first.Summary != "" {
		t.Fatalf("document chunks must not be replaced by recall summaries; got %q", first.Summary)
	}
	if first.ActivationStrength != 1.0 {
		t.Fatalf("chunk activation = %v, want 1.0 from ingest importance", first.ActivationStrength)
	}
	if first.Metadata["document_id"] != "tidebound-archive" {
		t.Fatalf("document_id metadata = %v, want tidebound-archive", first.Metadata["document_id"])
	}
	if first.Metadata["chunk_id"] != "tidebound-archive-chunk-1" {
		t.Fatalf("chunk_id metadata = %v, want tidebound-archive-chunk-1", first.Metadata["chunk_id"])
	}
	if first.Metadata["source_agent_id"] != doc.Author {
		t.Fatalf("source_agent_id metadata = %v, want %v", first.Metadata["source_agent_id"], doc.Author)
	}
	if gotTags, ok := first.Metadata["tags"].([]string); !ok || len(gotTags) != 3 {
		t.Fatalf("tags metadata = %#v, want preserved []string tags", first.Metadata["tags"])
	}
}

type batchCountingEmbedder struct {
	embedCalls int
	batchCalls int
	batchTexts []string
}

func (b *batchCountingEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	b.embedCalls++
	return []float32{9}, nil
}

func (b *batchCountingEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	b.batchCalls++
	b.batchTexts = append([]string(nil), texts...)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(i + 1)}
	}
	return out, nil
}

func TestIngestionManager_ProcessSync_batchesChunkEmbeddings(t *testing.T) {
	store := &captureAllStore{}
	embedder := &batchCountingEmbedder{}
	agent := &Agent{
		Manager:    &MemoryManager{Store: store, Embedder: embedder},
		pendingCap: 256,
	}
	reg, err := NewRegistry(defaultChunkers(), ChunkerConfig{Default: "option_c"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	im := NewIngestionManagerWithRegistry(
		NewSceneGenerator(&mockLLMGen{response: scenesJSON(1)}),
		embedder,
		agent,
		reg,
		IngestionConfig{},
	)
	doc := makeDoc(0)
	doc.Body = "First chunk text.\n\nSecond chunk text."

	_, err = im.ProcessSync(context.Background(), doc)
	if err != nil {
		t.Fatalf("ProcessSync: %v", err)
	}
	if embedder.batchCalls != 1 {
		t.Fatalf("EmbedBatch calls = %d, want 1", embedder.batchCalls)
	}
	if embedder.embedCalls != 0 {
		t.Fatalf("Embed calls = %d, want 0 when batch embed succeeds", embedder.embedCalls)
	}
	if len(embedder.batchTexts) != 2 {
		t.Fatalf("batch texts = %#v, want two chunk bodies", embedder.batchTexts)
	}
	facts := store.facts()
	if len(facts) != 2 {
		t.Fatalf("saved facts = %d, want 2", len(facts))
	}
	if got := facts[1].Embedding.Vector[0]; got != 2 {
		t.Fatalf("second embedding = %v, want vector from batch result", got)
	}
}

// T-1.10: the IngestionManager must (1) mint a DocTypeMnemonicEntity
// "source_document" row per doc with the offload content_cid, and (2)
// stamp every chunk's Metadata["chunk_relations"] with the parent entity
// ID, the linear neighbor IDs, and the SiblingContext payload.
func TestIngestionPipeline_SourceDocumentEntity(t *testing.T) {
	store := &captureAllStore{}
	cs := &fakeContentStore{}
	agent := &Agent{
		Manager:      &MemoryManager{Store: store, Embedder: &mockEmbedder{vec: []float32{0.1, 0.2}}},
		pendingCap:   256,
		ContentStore: cs,
	}

	reg, err := NewRegistry(defaultChunkers(), ChunkerConfig{Default: "option_c"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	sg := NewSceneGenerator(&mockLLMGen{response: scenesJSON(1)})
	im := NewIngestionManagerWithRegistry(sg, &mockEmbedder{vec: []float32{0.1, 0.2}}, agent, reg, IngestionConfig{
		QueueSize: 8, BatchSize: 1, Workers: 1, BatchWait: 20 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	im.Start(ctx)

	doc := makeDoc(0)
	doc.Body = "First paragraph of body.\n\nSecond paragraph of body.\n\nThird paragraph of body."
	if !im.Enqueue(doc) {
		t.Fatal("Enqueue returned false unexpectedly")
	}

	// Wait for the source-doc entity to be saved to the store.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.findByKind("source_document") != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	entity := store.findByKind("source_document")
	if entity == nil {
		t.Fatalf("no DocTypeMnemonicEntity row with kind=source_document was saved; saved=%d", len(store.savedDocs))
	}

	// 1. Entity shape: DocTypeMnemonicEntity, kind, all the provenance fields, content_cid.
	if entity.DocumentType != domain.DocTypeMnemonicEntity {
		t.Errorf("entity DocumentType = %q, want %q", entity.DocumentType, domain.DocTypeMnemonicEntity)
	}
	if entity.Metadata["source_uri"] != doc.SourceURI {
		t.Errorf("entity source_uri = %v, want %v", entity.Metadata["source_uri"], doc.SourceURI)
	}
	if entity.Metadata["source_type"] != doc.SourceType {
		t.Errorf("entity source_type = %v, want %v", entity.Metadata["source_type"], doc.SourceType)
	}
	if entity.Metadata["title"] != doc.Title {
		t.Errorf("entity title = %v, want %v", entity.Metadata["title"], doc.Title)
	}
	if entity.Metadata["author"] != doc.Author {
		t.Errorf("entity author = %v, want %v", entity.Metadata["author"], doc.Author)
	}
	if entity.Metadata["content_cid"] != "cid-abc" {
		t.Errorf("entity content_cid = %v, want %q (ContentStore.Put returned cid-abc)", entity.Metadata["content_cid"], "cid-abc")
	}
	if string(cs.putData) != doc.Body {
		t.Errorf("ContentStore was offloaded %d bytes, want %d", len(cs.putData), len(doc.Body))
	}

	chunks := store.facts()
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunk facts, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		raw, ok := chunk.Metadata["chunk_relations"]
		if !ok {
			t.Errorf("chunk %d: Metadata[chunk_relations] is missing", i)
			continue
		}
		data, _ := json.Marshal(raw)
		var rel ChunkRelations
		if err := json.Unmarshal(data, &rel); err != nil {
			t.Errorf("chunk %d: chunk_relations is not well-formed JSON: %v\npayload: %s", i, err, data)
			continue
		}
		if rel.ParentEntityID != entity.ID {
			t.Errorf("chunk %d: ParentEntityID = %q, want %q", i, rel.ParentEntityID, entity.ID)
		}
		if i == 0 && rel.PrecedingChunkID != "" {
			t.Errorf("chunk 0: PrecedingChunkID = %q, want \"\" (first chunk)", rel.PrecedingChunkID)
		}
		if i == len(chunks)-1 && rel.FollowingChunkID != "" {
			t.Errorf("chunk %d: FollowingChunkID = %q, want \"\" (last chunk)", i, rel.FollowingChunkID)
		}
		if i > 0 && rel.PrecedingChunkID == "" {
			t.Errorf("chunk %d: PrecedingChunkID is empty (expected a neighbor id)", i)
		}
		if i < len(chunks)-1 && rel.FollowingChunkID == "" {
			t.Errorf("chunk %d: FollowingChunkID is empty (expected a neighbor id)", i)
		}
		// SiblingContext sanity: at least the parent title and scene should be set.
		if rel.SiblingContext.ParentTitle != doc.Title {
			t.Errorf("chunk %d: SiblingContext.ParentTitle = %q, want %q", i, rel.SiblingContext.ParentTitle, doc.Title)
		}
	}

	// 3. Adjacent chunks reference each other (chunks[i].FollowingChunkID == chunks[i+1].PrecedingChunkID).
	for i := 0; i < len(chunks)-1; i++ {
		cur, _ := json.Marshal(chunks[i].Metadata["chunk_relations"])
		next, _ := json.Marshal(chunks[i+1].Metadata["chunk_relations"])
		var curR, nextR ChunkRelations
		_ = json.Unmarshal(cur, &curR)
		_ = json.Unmarshal(next, &nextR)
		if curR.FollowingChunkID != chunks[i+1].ID {
			t.Errorf("chunk %d FollowingChunkID = %q, want %q (chunks[%d].ID)", i, curR.FollowingChunkID, chunks[i+1].ID, i+1)
		}
		if nextR.PrecedingChunkID != chunks[i].ID {
			t.Errorf("chunk %d PrecedingChunkID = %q, want %q (chunks[%d].ID)", i+1, nextR.PrecedingChunkID, chunks[i].ID, i)
		}
	}
}

// Single-chunk edge case: preceding/following IDs must both be empty.
func TestIngestionPipeline_SourceDocumentEntity_SingleChunk(t *testing.T) {
	store := &captureAllStore{}
	cs := &fakeContentStore{}
	agent := &Agent{
		Manager:      &MemoryManager{Store: store, Embedder: &mockEmbedder{vec: []float32{0.1}}},
		pendingCap:   256,
		ContentStore: cs,
	}
	reg, _ := NewRegistry(defaultChunkers(), ChunkerConfig{Default: "option_c"})
	sg := NewSceneGenerator(&mockLLMGen{response: scenesJSON(1)})
	im := NewIngestionManagerWithRegistry(sg, &mockEmbedder{vec: []float32{0.1}}, agent, reg, IngestionConfig{
		QueueSize: 4, BatchSize: 1, Workers: 1, BatchWait: 20 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	im.Start(ctx)

	doc := makeDoc(0)
	doc.Body = "Only one paragraph here, no double newline."
	im.Enqueue(doc)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(store.facts()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	chunks := store.facts()
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	raw, _ := json.Marshal(chunks[0].Metadata["chunk_relations"])
	var rel ChunkRelations
	if err := json.Unmarshal(raw, &rel); err != nil {
		t.Fatalf("single-chunk chunk_relations: %v", err)
	}
	if rel.PrecedingChunkID != "" || rel.FollowingChunkID != "" {
		t.Errorf("single chunk: preceding=%q following=%q, both must be empty", rel.PrecedingChunkID, rel.FollowingChunkID)
	}
}
