package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// recordingEmbedder captures the last text embedded so a test can assert WHICH
// string was turned into the stored vector.
type recordingEmbedder struct{ lastText string }

func (r *recordingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	r.lastText = text
	return []float32{1, 0, 0}, nil
}

// collectStore records every saved doc.
type collectStore struct {
	fakeVectorStore
	docs []*domain.Document
}

func (c *collectStore) Save(_ context.Context, d *domain.Document) error {
	c.docs = append(c.docs, d)
	return nil
}

// QueryByMetadata filters the collected docs by metadata containment (string match),
// mirroring the pgvector @> semantics.
func (c *collectStore) QueryByMetadata(_ context.Context, filter map[string]string, _ int) ([]domain.Document, error) {
	var out []domain.Document
	for _, d := range c.docs {
		match := true
		for k, v := range filter {
			if got, _ := d.Metadata[k].(string); got != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, *d)
		}
	}
	return out, nil
}

type captureGraphStore struct{ edges []domain.DocumentEdge }

func (g *captureGraphStore) SaveEdge(_ context.Context, e domain.DocumentEdge) error {
	g.edges = append(g.edges, e)
	return nil
}
func (g *captureGraphStore) GetAdjacentEdges(context.Context, []string) ([]domain.DocumentEdge, error) {
	return nil, nil
}
func (g *captureGraphStore) UpdateEdgeWeight(context.Context, string, string, domain.EdgeType, float32) error {
	return nil
}

// fakeContentStore captures the offloaded body and returns a fixed cid.
type fakeContentStore struct{ putData []byte }

func (f *fakeContentStore) Put(_ context.Context, data []byte, _ string, _ []string, _ string) (domain.CID, error) {
	f.putData = data
	return domain.CID("cid-abc"), nil
}
func (f *fakeContentStore) Get(context.Context, domain.CID) (*domain.ContextNode, error) {
	return nil, nil
}
func (f *fakeContentStore) Has(context.Context, domain.CID) (bool, error) { return false, nil }
func (f *fakeContentStore) GC(context.Context, []domain.CID) error        { return nil }

func TestPlanIDFromTaskID(t *testing.T) {
	t.Parallel()
	if planIDFromTaskID("step-3-abc123") != "abc123" {
		t.Errorf("expected abc123; got %q", planIDFromTaskID("step-3-abc123"))
	}
	if planIDFromTaskID("garbage") != "" {
		t.Error("non-task id → empty")
	}
}

func actionDocs(store *collectStore) []*domain.Document {
	var out []*domain.Document
	for _, d := range store.docs {
		if d.DocumentType == domain.DocTypeMnemonicAction {
			out = append(out, d)
		}
	}
	return out
}

// ADR-0049 D1: a mutation output is saved DIRECTLY as a durable `mnemonic_action`,
// as the structured action line.
func TestRecordToolOutput_MutationSavesActionDoc(t *testing.T) {
	t.Parallel()
	store := &collectStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), 0.70, 5, 3)
	agent.RecordOutcomes = true

	err := agent.RecordToolOutput(context.Background(), domain.ToolOutputRecord{
		ToolName: "write_file", ArgsJSON: []byte(`{"path":"a.md"}`), Output: []byte(`{"ok":1}`), IsMutation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := actionDocs(store)
	if len(actions) != 1 {
		t.Fatalf("mutation must be saved directly as one mnemonic_action; got %+v", store.docs)
	}
	if !strings.HasPrefix(actions[0].Text, "write_file → ok") {
		t.Errorf("action text must be the structured line; got %q", actions[0].Text)
	}
	if len(actions[0].Embedding.Vector) == 0 {
		t.Error("action must be embedded")
	}
	if len(store.docs) != 1 {
		t.Errorf("a mutation writes its action record and nothing else; saved %d docs", len(store.docs))
	}
}

// The action record carries the tool's classification tags, so the write
// chokepoint classifies it from principal + tool domain instead of seeing a nil
// hint and committing the row untagged.
func TestRecordToolOutput_ActionCarriesClassificationTags(t *testing.T) {
	t.Parallel()
	store := &collectStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), 0.70, 5, 3)
	agent.RecordOutcomes = true

	err := agent.RecordToolOutput(context.Background(), domain.ToolOutputRecord{
		ToolName: "write_file", ArgsJSON: []byte(`{"path":"a.md"}`), Output: []byte(`{"ok":1}`),
		IsMutation: true, ClassificationTags: []string{"filesystem"},
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := actionDocs(store)
	if len(actions) != 1 {
		t.Fatalf("mutation must be saved as a mnemonic_action; got %+v", store.docs)
	}
	tags, _ := actions[0].Metadata["tags"].([]string)
	if len(tags) != 1 || tags[0] != "filesystem" {
		t.Errorf("action record must carry the tool's classification tags; got %v", actions[0].Metadata["tags"])
	}
}

// 2026-11-05 (decisions/scenes-cancelled.md): a READ tool output records nothing.
// Its payload used to be embedded whole as a fact for the tier-2 judge, and its
// entities accreted the plan scene; both paths were deleted.
func TestRecordToolOutput_ReadRecordsNothing(t *testing.T) {
	t.Parallel()
	store := &collectStore{}
	emb := &recordingEmbedder{}
	agent := NewAgent(NewMemoryManager(store, emb), 0.70, 5, 3)
	agent.RecordOutcomes = true

	err := agent.RecordToolOutput(context.Background(), domain.ToolOutputRecord{
		ToolName: "read_file", ArgsJSON: []byte(`{"path":"a.md"}`),
		Output: []byte(strings.Repeat("content ", 100)), TaskID: "step-0-plan1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.docs) != 0 || emb.lastText != "" {
		t.Errorf("a read must neither embed nor save anything; saved %d docs, embedded %q", len(store.docs), emb.lastText)
	}
}

// With the arm off, not even a mutation is recorded.
func TestRecordToolOutput_ArmOffRecordsNothing(t *testing.T) {
	t.Parallel()
	store := &collectStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), 0.70, 5, 3)

	_ = agent.RecordToolOutput(context.Background(), domain.ToolOutputRecord{
		ToolName: "write_file", ArgsJSON: []byte(`{"path":"a.md"}`), Output: []byte(`{"ok":1}`), IsMutation: true,
	})
	if len(store.docs) != 0 {
		t.Errorf("RecordOutcomes=false must write nothing; saved %+v", store.docs)
	}
}
