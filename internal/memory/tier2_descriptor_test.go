package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// recordingEmbedder captures the last text embedded so a test can assert WHICH
// string was turned into the stored vector (descriptor vs raw payload).
type recordingEmbedder struct{ lastText string }

func (r *recordingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	r.lastText = text
	return []float32{1, 0, 0}, nil
}

// sceneCollectStore records the DocumentType of every saved doc.
type sceneCollectStore struct {
	fakeVectorStore
	types []string
}

func (c *sceneCollectStore) Save(_ context.Context, d *domain.Document) error {
	c.types = append(c.types, d.DocumentType)
	return nil
}

func (c *sceneCollectStore) saved(docType string) bool {
	for _, ty := range c.types {
		if ty == docType {
			return true
		}
	}
	return false
}

// ADR-0049 D3: when the eager WriteScene already produced a scene (SceneID set),
// the Tier-2 commit must NOT write a second scene of the same snapshot.
func TestCommitItem_SkipsTier2SceneWhenEagerSceneExists(t *testing.T) {
	store := &sceneCollectStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	s := scoredItem{
		Item: pendingItem{
			SceneID: "scene-1", // eager scene already written
			Doc:     &domain.Document{ID: "f1", Text: "x", Metadata: map[string]interface{}{"snapshot": `{"k":"v"}`}},
		},
		Tier: "FULL",
	}
	agent.commitItem(context.Background(), s)
	if store.saved(domain.DocTypeMnemonicScene) {
		t.Errorf("no Tier-2 scene must be written when an eager scene exists; saved %v", store.types)
	}
	if !store.saved(domain.DocTypeMnemonicFact) {
		t.Errorf("the fact must still be saved; saved %v", store.types)
	}
}

// With no eager scene (SceneID empty), the Tier-2 scene is the fallback.
func TestCommitItem_WritesTier2SceneWhenNoEagerScene(t *testing.T) {
	store := &sceneCollectStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	s := scoredItem{
		Item: pendingItem{
			SceneID: "",
			Doc:     &domain.Document{ID: "f2", Text: "x", Metadata: map[string]interface{}{"snapshot": `{"k":"v"}`}},
		},
		Tier: "FULL",
	}
	agent.commitItem(context.Background(), s)
	if !store.saved(domain.DocTypeMnemonicScene) {
		t.Errorf("a Tier-2 scene must be written as fallback when no eager scene; saved %v", store.types)
	}
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

func TestPlanIDFromTaskID(t *testing.T) {
	if planIDFromTaskID("step-3-abc123") != "abc123" {
		t.Errorf("expected abc123; got %q", planIDFromTaskID("step-3-abc123"))
	}
	if planIDFromTaskID("garbage") != "" {
		t.Error("non-task id → empty")
	}
}

// A plan's actions accrete engaged refs; WritePlanScene writes ONE immutable scene.
func TestWritePlanScene_AccretesAndWritesOneScene(t *testing.T) {
	store := &captureSaveStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	ctx := context.Background()
	_ = agent.RecordToolOutput(ctx, domain.ToolOutputRecord{ToolName: "write_file", ArgsJSON: []byte(`{"path":"a.md"}`), Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "step-0-p1"})
	_ = agent.RecordToolOutput(ctx, domain.ToolOutputRecord{ToolName: "write_file", ArgsJSON: []byte(`{"path":"b.md"}`), Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "step-1-p1"})

	_ = agent.WritePlanScene(ctx, "p1", "build docs", true)

	if store.savedDoc == nil || store.savedDoc.DocumentType != domain.DocTypeMnemonicScene {
		t.Fatalf("expected a mnemonic_scene; got %+v", store.savedDoc)
	}
	if store.savedDoc.ID != "scene-p1" {
		t.Errorf("scene id must be scene-{planID}; got %q", store.savedDoc.ID)
	}
	txt := store.savedDoc.Text
	if !strings.Contains(txt, "build docs") || !strings.Contains(txt, "file:a.md") || !strings.Contains(txt, "file:b.md") {
		t.Errorf("scene must carry goal + accreted engaged refs; got %q", txt)
	}
	if len(agent.planEntities["p1"]) != 0 {
		t.Error("plan accumulator must be cleared after WritePlanScene")
	}
}

// ADR-0049: WritePlanScene wires the world model into the graph — one FK-safe
// scene→entity `engaged` edge per engaged thing (both endpoints persisted).
func TestWritePlanScene_WritesSceneEntityEdges(t *testing.T) {
	gs := &captureGraphStore{}
	agent := NewAgent(NewMemoryManager(&captureSaveStore{}, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	agent.GraphStore = gs
	ctx := context.Background()

	_ = agent.RecordToolOutput(ctx, domain.ToolOutputRecord{ToolName: "write_file", ArgsJSON: []byte(`{"path":"docs/a.md"}`), Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "step-0-pe"})
	_ = agent.WritePlanScene(ctx, "pe", "build docs", true)

	want := map[string]bool{"file:docs/a.md": false, "dir:docs": false}
	for _, e := range gs.edges {
		if e.EdgeType == domain.EdgeEngaged && e.SourceID == "scene-pe" {
			if _, ok := want[e.TargetID]; ok {
				want[e.TargetID] = true
			}
		}
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("expected scene→entity edge to %s; got %+v", key, gs.edges)
		}
	}
}

// ADR-0049: the scene embeds an inline ACTION SUMMARY (its "what I did" path) resolved
// from the plan's action records, so a scene reads as a self-contained transition.
func TestWritePlanScene_EmbedsInlineActionSummary(t *testing.T) {
	store := &collectStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	ctx := context.Background()

	// Two mutations under plan p9 → two action records (resolvable by plan_id).
	_ = agent.RecordToolOutput(ctx, domain.ToolOutputRecord{ToolName: "write_file", ArgsJSON: []byte(`{"path":"a.md"}`), Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "step-0-p9"})
	_ = agent.RecordToolOutput(ctx, domain.ToolOutputRecord{ToolName: "write_file", ArgsJSON: []byte(`{"path":"b.md"}`), Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "step-1-p9"})

	_ = agent.WritePlanScene(ctx, "p9", "build docs", true)

	var scene *domain.Document
	for _, d := range store.docs {
		if d.DocumentType == domain.DocTypeMnemonicScene {
			scene = d
		}
	}
	if scene == nil {
		t.Fatal("expected a scene")
	}
	if !strings.Contains(scene.Text, "actions:") || !strings.Contains(scene.Text, "write_file") {
		t.Errorf("scene must embed the inline action path; got %q", scene.Text)
	}
	lines, ok := scene.Metadata["actions"].([]string)
	if !ok || len(lines) != 2 {
		t.Errorf("scene metadata must carry the action lines; got %v", scene.Metadata["actions"])
	}
}

// ADR-0049 D5/D7: a plan that engaged NO entity transitioned no world state — it must
// NOT write a contentless goal-only scene (which would pollute situational retrieval).
func TestWritePlanScene_SkipsContentlessScene(t *testing.T) {
	store := &captureSaveStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)

	// No engagements accreted for this plan (e.g. a pure-reasoning plan, or a replan
	// whose work all happened under the original planID).
	_ = agent.WritePlanScene(context.Background(), "p-empty", "Replan: think hard about X", true)

	if store.savedDoc != nil {
		t.Errorf("a plan with no engaged entities must write no scene; got %+v", store.savedDoc)
	}
}

// ADR-0049 D6: a first-touched entity's baseline is offloaded to CAS; the scene
// stores the ref → cid (by reference), and the scene text shows the cid pointer.
func TestWritePlanScene_CapturesBaselineCIDs(t *testing.T) {
	store := &captureSaveStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	agent.ContentStore = &fakeContentStore{}
	ctx := context.Background()

	_ = agent.RecordToolOutput(ctx, domain.ToolOutputRecord{ToolName: "write_file", ArgsJSON: []byte(`{"path":"a.md"}`), Output: []byte(`baseline body`), IsMutation: true, TaskID: "step-0-p2"})
	_ = agent.WritePlanScene(ctx, "p2", "read docs", true)

	engaged, ok := store.savedDoc.Metadata["engaged"].(map[string]string)
	if !ok || engaged["file:a.md"] != "cid-abc" {
		t.Errorf("engaged ref must carry its baseline cid; got %v", store.savedDoc.Metadata["engaged"])
	}
	if !strings.Contains(store.savedDoc.Text, "baseline cid:cid-abc") {
		t.Errorf("scene text must show the baseline cid pointer; got %q", store.savedDoc.Text)
	}
}

// ADR-0049 D10: the follows-edge derivation is pure — skips empties and self-loops.
func TestFollowsEdges(t *testing.T) {
	edges := followsEdges("d1", []string{"d0", "", "d1", "d2"})
	if len(edges) != 2 {
		t.Fatalf("expected 2 follows edges (d0, d2), skipping empty + self; got %d", len(edges))
	}
	for _, e := range edges {
		if e.SourceID != "d1" || e.EdgeType != domain.EdgeFollows {
			t.Errorf("bad follows edge %+v", e)
		}
	}
	if followsEdges("", []string{"d0"}) != nil {
		t.Error("empty step docID → no edges")
	}
}

// ADR-0049 D10 FK-safety: a follows edge is BUFFERED at RecordExecution (the step record
// is only pending) and written ONLY once both endpoints have committed via Tier-2 —
// never against a not-yet-persisted (or dropped) row.
func TestRecordExecution_DefersFollowsEdgeUntilBothCommitted(t *testing.T) {
	gs := &captureGraphStore{}
	agent := NewAgent(NewMemoryManager(&captureSaveStore{}, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	agent.GraphStore = gs
	ctx := context.Background()

	_ = agent.RecordExecution(ctx, domain.StepResult{Index: 0, Output: "did A", TaskID: "t0"})
	step0Doc := agent.stepDocs["t0"]
	_ = agent.RecordExecution(ctx, domain.StepResult{Index: 1, Output: "did B", TaskID: "t1", DependsOnTaskIDs: []string{"t0"}})
	step1Doc := agent.stepDocs["t1"]
	if step0Doc == "" || step1Doc == "" {
		t.Fatal("both steps must record their docIDs for the follows chain")
	}

	// Nothing is committed yet → NO edge may be written (it would FK-violate).
	if len(gs.edges) != 0 {
		t.Fatalf("follows edge must not be written before the rows exist; got %+v", gs.edges)
	}

	// Commit the SOURCE only — still must not write (target row absent).
	agent.commitFollows(ctx, step1Doc)
	if len(gs.edges) != 0 {
		t.Fatalf("edge must wait for BOTH endpoints; got %+v after source-only commit", gs.edges)
	}

	// Commit the TARGET — now both rows exist, the edge is flushed.
	agent.commitFollows(ctx, step0Doc)
	found := false
	for _, e := range gs.edges {
		if e.EdgeType == domain.EdgeFollows && e.SourceID == step1Doc && e.TargetID == step0Doc {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a follows edge step1→step0 once both committed; got %+v", gs.edges)
	}
}

// ADR-0049 D3: the dedup decision is a pure function of action count.
func TestDropStepSynthesis(t *testing.T) {
	if !dropStepSynthesis(1) {
		t.Error("exactly one action → drop the synthesis")
	}
	if dropStepSynthesis(0) || dropStepSynthesis(2) || dropStepSynthesis(-1) {
		t.Error("zero, multi, or uncorrelated → keep the synthesis")
	}
}

// A single-action step's prose synthesis is dropped (it restates the one action).
func TestRecordExecution_DropsSingleActionSynthesis(t *testing.T) {
	agent := NewAgent(NewMemoryManager(&captureSaveStore{}, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	ctx := context.Background()
	_ = agent.RecordToolOutput(ctx, domain.ToolOutputRecord{ToolName: "write_file", Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "t1"})
	_ = agent.RecordExecution(ctx, domain.StepResult{Index: 0, Output: "appended ok", TaskID: "t1"})
	if len(agent.pendingItems) != 0 {
		t.Errorf("single-action step synthesis must be dropped; pending=%d", len(agent.pendingItems))
	}
}

// A multi-action step keeps its synthesis, tagged with the step's task_id.
func TestRecordExecution_KeepsMultiActionSynthesis(t *testing.T) {
	agent := NewAgent(NewMemoryManager(&captureSaveStore{}, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_ = agent.RecordToolOutput(ctx, domain.ToolOutputRecord{ToolName: "write_file", Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "t2"})
	}
	_ = agent.RecordExecution(ctx, domain.StepResult{Index: 0, Output: "did two things", TaskID: "t2"})
	if len(agent.pendingItems) != 1 {
		t.Fatalf("multi-action step must keep its synthesis; pending=%d", len(agent.pendingItems))
	}
	if agent.pendingItems[0].Doc.Metadata["task_id"] != "t2" {
		t.Errorf("kept synthesis must carry the step's task_id; got %v", agent.pendingItems[0].Doc.Metadata["task_id"])
	}
}

// An uncorrelated step (no task_id — dedup off) keeps its synthesis.
func TestRecordExecution_UncorrelatedKeepsSynthesis(t *testing.T) {
	agent := NewAgent(NewMemoryManager(&captureSaveStore{}, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	_ = agent.RecordExecution(context.Background(), domain.StepResult{Index: 0, Output: "x", TaskID: ""})
	if len(agent.pendingItems) != 1 {
		t.Errorf("uncorrelated step keeps its synthesis; pending=%d", len(agent.pendingItems))
	}
}

// ADR-0049 D1: a mutation output is saved DIRECTLY as a durable `mnemonic_action`
// (bypassing the Tier-2 keep/drop pipeline), as the structured action line.
func TestRecordToolOutput_MutationSavesActionDoc(t *testing.T) {
	store := &collectStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)

	err := agent.RecordToolOutput(context.Background(), domain.ToolOutputRecord{
		ToolName: "write_file", ArgsJSON: []byte(`{"path":"a.md"}`), Output: []byte(`{"ok":1}`), IsMutation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The mutation is saved directly as a mnemonic_action (alongside the D8 entity mint).
	var action *domain.Document
	for _, d := range store.docs {
		if d.DocumentType == domain.DocTypeMnemonicAction {
			action = d
		}
	}
	if action == nil {
		t.Fatalf("mutation must be saved directly as a mnemonic_action; got %+v", store.docs)
	}
	if !strings.HasPrefix(action.Text, "write_file → ok") {
		t.Errorf("action text must be the structured line; got %q", action.Text)
	}
	if len(action.Embedding.Vector) == 0 {
		t.Error("action must be embedded")
	}
}

// A read output flows into the Tier-1 fact pending channel (not a direct save).
func TestRecordToolOutput_ReadGoesToFactPending(t *testing.T) {
	store := &captureSaveStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)

	err := agent.RecordToolOutput(context.Background(), domain.ToolOutputRecord{
		ToolName: "web_search", Output: []byte(`{"results":["x"]}`), IsMutation: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.savedDoc != nil {
		t.Error("a read fact must not be direct-saved — it flows through Tier-2")
	}
	if len(agent.pendingItems) != 1 || agent.pendingItems[0].Doc.DocumentType != domain.DocTypeMnemonicFact {
		t.Errorf("read output must be a pending mnemonic_fact; got %+v", agent.pendingItems)
	}
}

func TestHeuristicDescriptor(t *testing.T) {
	got := heuristicDescriptor(`tool[mcp:firecrawl/firecrawl_search]: [{"type":"text","huge":"payload..."}]`)
	if got != `Output of tool "mcp:firecrawl/firecrawl_search" (74 chars).` && !strings.HasPrefix(got, `Output of tool "mcp:firecrawl/firecrawl_search"`) {
		t.Errorf("tool envelope should collapse to a named one-liner; got %q", got)
	}
	if d := heuristicDescriptor("short note"); d != "short note" {
		t.Errorf("short text kept verbatim; got %q", d)
	}
	long := strings.Repeat("x", 500)
	if d := heuristicDescriptor(long); len(d) >= len(long) || !strings.HasSuffix(d, "…") {
		t.Errorf("long text head-truncated with ellipsis; got len=%d", len(d))
	}
}

func TestIsToolOutputItem(t *testing.T) {
	tool := &domain.Document{Metadata: map[string]interface{}{"source_agent": "ToolOutput"}}
	step := &domain.Document{Metadata: map[string]interface{}{"source_agent": "System"}}
	if !isToolOutputItem(tool) {
		t.Error("ToolOutput-sourced doc must be detected")
	}
	if isToolOutputItem(step) || isToolOutputItem(&domain.Document{}) || isToolOutputItem(nil) {
		t.Error("non-ToolOutput docs must NOT be descriptor-indexed")
	}
}

// A promoted tool output is stored as its DESCRIPTOR (embedded + as Text), with the
// raw payload preserved in metadata — ADR-0048 #2.
func TestCommitItem_ToolOutputIndexedByDescriptor(t *testing.T) {
	store := &captureSaveStore{}
	emb := &recordingEmbedder{}
	mgr := NewMemoryManager(store, emb)
	agent := NewAgent(mgr, nil, 0.70, 5, 3, 64, 0, 0, 0)

	raw := `tool[mcp:firecrawl/firecrawl_search]: [{"rivers":"Nile, Amazon, ..."}]`
	s := scoredItem{
		Item: pendingItem{
			Doc: &domain.Document{
				ID:           "tier1-tool-1",
				DocumentType: domain.DocTypeMnemonicFact,
				Text:         raw,
				Metadata:     map[string]interface{}{"source_agent": "ToolOutput"},
			},
			Embedding: []float32{0, 0, 9}, // the stale raw-text vector
		},
		Tier:       "FACT_ONLY",
		Descriptor: "Web search results listing the world's longest rivers.",
	}

	agent.commitItem(context.Background(), s)

	if store.savedDoc == nil {
		t.Fatal("expected a saved doc")
	}
	// ADR-0048 #1: the descriptor is the Summary column; the full body stays in Text.
	if store.savedDoc.Summary != s.Descriptor {
		t.Errorf("Summary must be the descriptor; got %q", store.savedDoc.Summary)
	}
	if store.savedDoc.Text != raw {
		t.Errorf("Text must keep the full body; got %q", store.savedDoc.Text)
	}
	if emb.lastText != s.Descriptor {
		t.Errorf("embedding must be recomputed on the descriptor; embedded %q", emb.lastText)
	}
	if len(store.savedDoc.Embedding.Vector) == 0 || store.savedDoc.Embedding.Vector[0] != 1 {
		t.Errorf("stored vector must be the re-embedded descriptor vector; got %v", store.savedDoc.Embedding.Vector)
	}
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

// With a ContentStore wired, a promoted tool output offloads its full body to CAS
// and records the cid in metadata for {summary + cid} recall (ADR-0048 #1).
func TestCommitItem_OffloadsFullBodyAndRecordsCID(t *testing.T) {
	store := &captureSaveStore{}
	cs := &fakeContentStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	agent.ContentStore = cs

	raw := `tool[web_search]: {"big":"body"}`
	s := scoredItem{
		Item: pendingItem{Doc: &domain.Document{
			ID: "t1", DocumentType: domain.DocTypeMnemonicFact, Text: raw,
			Metadata: map[string]interface{}{"source_agent": "ToolOutput"},
		}},
		Tier:       "FACT_ONLY",
		Descriptor: "Web search results.",
	}
	agent.commitItem(context.Background(), s)

	if string(cs.putData) != raw {
		t.Errorf("full body must be offloaded to CAS; got %q", string(cs.putData))
	}
	if store.savedDoc.Metadata["content_cid"] != "cid-abc" {
		t.Errorf("content_cid must be recorded for drill-down; got %v", store.savedDoc.Metadata["content_cid"])
	}
	if store.savedDoc.Summary != "Web search results." || store.savedDoc.Text != raw {
		t.Errorf("Summary=descriptor, Text=full body expected; got summary=%q text=%q",
			store.savedDoc.Summary, store.savedDoc.Text)
	}
}

// A non-tool memory (step record) now ALSO gets its Summary filled (ADR-0048 #1 —
// every promoted memory carries a gist), but keeps its full Text and is NOT offloaded
// to CAS (no content_cid) — the offload is reserved for heavy tool-output bodies.
func TestCommitItem_StepRecordGetsSummaryKeepsTextNoOffload(t *testing.T) {
	store := &captureSaveStore{}
	cs := &fakeContentStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	agent.ContentStore = cs

	s := scoredItem{
		Item: pendingItem{
			Doc: &domain.Document{
				ID:           "step-1",
				DocumentType: domain.DocTypeMnemonicFact,
				Text:         "step_0: a real curated fact with all the detail",
				Metadata:     map[string]interface{}{"source_agent": "System"},
			},
			Embedding: []float32{0.5},
		},
		Tier:       "FACT_ONLY",
		Descriptor: "The agent wrote the project README.",
	}

	agent.commitItem(context.Background(), s)

	if store.savedDoc.Summary != "The agent wrote the project README." {
		t.Errorf("step record must get its Summary filled; got %q", store.savedDoc.Summary)
	}
	if store.savedDoc.Text != "step_0: a real curated fact with all the detail" {
		t.Errorf("step record must keep its full Text; got %q", store.savedDoc.Text)
	}
	if cs.putData != nil {
		t.Error("a non-tool memory must NOT be offloaded to CAS")
	}
	if _, ok := store.savedDoc.Metadata["content_cid"]; ok {
		t.Error("a non-tool memory must NOT get a content_cid")
	}
}
