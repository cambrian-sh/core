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

// ENG-1595 (owner decision 2026-11-14): not even a MUTATION is recorded any more.
// It used to be embedded and Manager.Save'd as a mnemonic_action row past the
// ingestion pipeline; the owner rejected raw saves of tool outputs.
func TestRecordToolOutput_MutationRecordsNothing(t *testing.T) {
	t.Parallel()
	store := &collectStore{}
	emb := &recordingEmbedder{}
	agent := NewAgent(NewMemoryManager(store, emb), 3)
	agent.RecordOutcomes = true

	err := agent.RecordToolOutput(context.Background(), domain.ToolOutputRecord{
		ToolName: "write_file", ArgsJSON: []byte(`{"path":"a.md"}`), Output: []byte(`{"ok":1}`),
		IsMutation: true, TaskID: "step-0-plan1", ClassificationTags: []string{"finance"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.docs) != 0 || emb.lastText != "" {
		t.Errorf("a mutation must neither embed nor save anything; saved %d docs, embedded %q", len(store.docs), emb.lastText)
	}
}

// 2026-11-05 (decisions/scenes-cancelled.md): a READ tool output records nothing.
// Its payload used to be embedded whole as a fact for the tier-2 judge, and its
// entities accreted the plan scene; both paths were deleted.
func TestRecordToolOutput_ReadRecordsNothing(t *testing.T) {
	t.Parallel()
	store := &collectStore{}
	emb := &recordingEmbedder{}
	agent := NewAgent(NewMemoryManager(store, emb), 3)
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
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), 3)

	_ = agent.RecordToolOutput(context.Background(), domain.ToolOutputRecord{
		ToolName: "write_file", ArgsJSON: []byte(`{"path":"a.md"}`), Output: []byte(`{"ok":1}`), IsMutation: true,
	})
	if len(store.docs) != 0 {
		t.Errorf("RecordOutcomes=false must write nothing; saved %+v", store.docs)
	}
}
