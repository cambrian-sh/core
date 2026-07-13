package memory

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0049 D8: equivalent spellings of one real path must canonicalize to one id —
// otherwise the entity store fragments. Windows case + separator forms collapse.
func TestCanonicalPath_CollapsesEquivalentForms(t *testing.T) {
	want := "c:/users/foo/a.md"
	for _, raw := range []string{
		`C:\Users\Foo\a.md`,
		`C:/Users/Foo/a.md`,
		`c:/users/foo/a.md`,
		`C:\Users\Foo\.\a.md`,
		`C:\Users\Bar\..\Foo\a.md`,
	} {
		if got := canonicalPath(raw); got != want {
			t.Errorf("canonicalPath(%q) = %q; want %q", raw, got, want)
		}
	}
	// A directory with/without a trailing slash is the same directory.
	if canonicalPath("docs/") != canonicalPath("docs") {
		t.Error("trailing slash must not fragment a directory")
	}
	if canonicalPath("./docs/a.md") != "docs/a.md" {
		t.Errorf("leading ./ must be cleaned; got %q", canonicalPath("./docs/a.md"))
	}
}

// ADR-0049 D8: an api is keyed by scheme://host; every endpoint under it collapses to
// the same id, with the path surfaced as an attribute. Default port carries no info.
func TestCanonicalAPI_CollapsesEndpointsAndPorts(t *testing.T) {
	id1, ep1, ok1 := canonicalAPI("https://api.example.com/v1/users")
	id2, ep2, _ := canonicalAPI("https://API.example.com:443/v1/orders")
	if !ok1 || id1 != "https://api.example.com" {
		t.Errorf("api id must be scheme://host; got %q ok=%v", id1, ok1)
	}
	if id1 != id2 {
		t.Errorf("default port + case must not fragment the api; %q vs %q", id1, id2)
	}
	if ep1 != "/v1/users" || ep2 != "/v1/orders" {
		t.Errorf("endpoints must surface as attributes; got %q, %q", ep1, ep2)
	}
	// A bare host parses to a host-only api id (no synthetic scheme leaks in).
	if id, _, ok := canonicalAPI("example.com"); !ok || id != "example.com" {
		t.Errorf("bare host must canonicalize to itself; got %q ok=%v", id, ok)
	}
}

// ADR-0049 D8: a file mutation yields the file AND its parent directory; granularity
// is file/dir, never one-entity-per-endpoint.
func TestEngagedEntities_FileYieldsFileAndParentDir(t *testing.T) {
	ents := engagedEntities("write_file", []byte(`{"path":"C:\\repo\\docs\\a.md","content":"x"}`))
	keys := map[string]bool{}
	for _, e := range ents {
		keys[e.Key()] = true
	}
	if !keys["file:c:/repo/docs/a.md"] {
		t.Errorf("expected the file entity; got %v", keys)
	}
	if !keys["dir:c:/repo/docs"] {
		t.Errorf("expected the parent dir entity; got %v", keys)
	}
	if len(ents) != 2 {
		t.Errorf("file mutation must yield exactly file + parent dir; got %d (%v)", len(ents), keys)
	}
}

// ADR-0049 D8: the `path` arg is ambiguous — a list/dir tool names a DIRECTORY, and the
// cwd (".") identifies nothing, so it must NOT become a "file ." entity.
func TestEngagedEntities_ListDirectoryPathIsDirNotFile(t *testing.T) {
	// path="." on a directory tool → degenerate cwd → NO entity at all.
	if ents := engagedEntities("mcp:filesystem/list_directory_with_sizes", []byte(`{"path":"."}`)); len(ents) != 0 {
		t.Errorf("listing the cwd must mint no entity (\".\" identifies nothing); got %v", ents)
	}
	// path="docs" on a directory tool → a DIR entity, not a file.
	ents := engagedEntities("mcp:filesystem/list_directory", []byte(`{"path":"docs"}`))
	if len(ents) != 1 || ents[0].Kind != "dir" || ents[0].ID != "docs" {
		t.Errorf("a list tool's path must be a dir entity; got %v", ents)
	}
	// path="a.md" on a read tool → a FILE entity.
	if ents := engagedEntities("mcp:filesystem/read_file", []byte(`{"path":"a.md"}`)); len(ents) != 1 || ents[0].Kind != "file" {
		t.Errorf("a read_file's path must be a file entity; got %v", ents)
	}
}

func TestEngagedEntities_NoResourceArgs(t *testing.T) {
	if engagedEntities("noop", []byte(`{"content":"x","reason":"y"}`)) != nil {
		t.Error("no resource args → no entities")
	}
}

// collectStore records every saved doc so a test can inspect the minted entities.
type collectStore struct {
	fakeVectorStore
	docs []*domain.Document
}

func (c *collectStore) Save(_ context.Context, d *domain.Document) error {
	c.docs = append(c.docs, d)
	return nil
}

func (c *collectStore) entityDocs() []*domain.Document {
	var out []*domain.Document
	for _, d := range c.docs {
		if d.DocumentType == domain.DocTypeMnemonicEntity {
			out = append(out, d)
		}
	}
	return out
}

// QueryByMetadata filters the collected docs by metadata containment (string match),
// mirroring the pgvector @> semantics enough for the scene action-path resolution.
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

// getByIDStore extends collectStore with a working GetByID (returns the latest doc saved
// under an id), so upsertEntity can read back the prior entity view — drift detection
// (ADR-0049 §A1.2) needs the cached fields.
type getByIDStore struct {
	collectStore
}

func (s *getByIDStore) GetByID(_ context.Context, id string) (*domain.Document, error) {
	var last *domain.Document
	for _, d := range s.docs {
		if d.ID == id {
			last = d
		}
	}
	return last, nil
}

// ADR-0049 §A1.2: a READ that finds a file's content changed from cache emits a passive
// world_delta (the drift signal); a first touch (discovery), an unchanged re-read, and a
// WRITE (intentional change) do NOT.
func TestRecordToolOutput_ReadDriftEmitsWorldDelta(t *testing.T) {
	store := &getByIDStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	bus := domain.NewInMemoryEventBus()
	var mu sync.Mutex
	var deltas []domain.WorldDeltaEvent
	bus.Subscribe(domain.EventTypeWorldDelta, func(e domain.DomainEvent) {
		mu.Lock()
		defer mu.Unlock()
		deltas = append(deltas, e.(domain.WorldDeltaEvent))
	})
	agent.EventBus = bus
	ctx := context.Background()

	read := func(body string) domain.ToolOutputRecord {
		return domain.ToolOutputRecord{ToolName: "read_file", ArgsJSON: []byte(`{"path":"docs/a.md"}`), Output: []byte(body), IsMutation: false, TaskID: "step-0-p1"}
	}

	_ = agent.RecordToolOutput(ctx, read("body-v1")) // first touch — discovery, not drift
	if len(deltas) != 0 {
		t.Fatalf("first read is discovery, must not drift; got %v", deltas)
	}
	_ = agent.RecordToolOutput(ctx, read("body-v1")) // same content — no drift
	if len(deltas) != 0 {
		t.Fatalf("unchanged re-read must not drift; got %v", deltas)
	}
	_ = agent.RecordToolOutput(ctx, read("body-v2")) // changed content — drift on content_ref
	if len(deltas) != 1 || deltas[0].Field != "content_ref" || deltas[0].EntityKey != "file:docs/a.md" {
		t.Fatalf("a changed re-read must emit one content_ref world_delta; got %v", deltas)
	}

	// A WRITE that changes content is intentional supersession — not drift.
	mu.Lock()
	deltas = nil
	mu.Unlock()
	_ = agent.RecordToolOutput(ctx, domain.ToolOutputRecord{ToolName: "write_file", ArgsJSON: []byte(`{"path":"docs/a.md"}`), Output: []byte("body-v3"), IsMutation: true, TaskID: "step-0-p1"})
	if len(deltas) != 0 {
		t.Fatalf("a write's change is intentional, not drift; got %v", deltas)
	}
}

// ADR-0049 §A1.1: every entity upsert stamps last_observed_at (the staleness signal).
func TestUpsertEntity_StampsLastObservedAt(t *testing.T) {
	store := &getByIDStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	_ = agent.RecordToolOutput(context.Background(), domain.ToolOutputRecord{ToolName: "read_file", ArgsJSON: []byte(`{"path":"docs/a.md"}`), Output: []byte("x"), IsMutation: false, TaskID: "step-0-p1"})
	for _, d := range store.entityDocs() {
		if _, ok := d.Metadata["last_observed_at"].(string); !ok || d.Metadata["last_observed_at"] == "" {
			t.Errorf("entity %s must carry a non-empty last_observed_at; got %v", d.ID, d.Metadata["last_observed_at"])
		}
	}
}

// ADR-0049 D8 end-to-end: a write_file mutation mints file + dir entities; writing the
// same path twice resolves to the same ids (no duplicate entities by key).
func TestRecordToolOutput_MutationMintsEntities(t *testing.T) {
	store := &collectStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	ctx := context.Background()

	rec := domain.ToolOutputRecord{ToolName: "write_file", ArgsJSON: []byte(`{"path":"docs/a.md"}`), Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "step-0-p1"}
	_ = agent.RecordToolOutput(ctx, rec)
	_ = agent.RecordToolOutput(ctx, rec) // same path again

	ids := map[string]int{}
	for _, d := range store.entityDocs() {
		ids[d.ID]++
	}
	if ids["file:docs/a.md"] == 0 || ids["dir:docs"] == 0 {
		t.Errorf("write_file must mint the file and dir entities; got %v", ids)
	}
	// Two writes of the same path upsert by id — both touches reuse the same two keys.
	if len(ids) != 2 {
		t.Errorf("same path twice must resolve to the same entities (2 distinct ids); got %v", ids)
	}
}

// ADR-0049 (revised): a READ is discovery — it mints/enriches the engaged entity too,
// recording exists=true + the observed content baseline. A read writes NO action event.
func TestRecordToolOutput_ReadMintsEntityWithBaseline(t *testing.T) {
	store := &collectStore{}
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.70, 5, 3, 64, 0, 0, 0)
	agent.ContentStore = &fakeContentStore{} // offloads the read content → cid-abc
	ctx := context.Background()

	_ = agent.RecordToolOutput(ctx, domain.ToolOutputRecord{ToolName: "read_file", ArgsJSON: []byte(`{"path":"docs/a.md"}`), Output: []byte(`file body`), IsMutation: false, TaskID: "step-0-p1"})

	var fileEnt *domain.Document
	for _, d := range store.entityDocs() {
		if d.ID == "file:docs/a.md" {
			fileEnt = d
		}
	}
	if fileEnt == nil {
		t.Fatalf("a read must mint the engaged file entity (discovery); got %v", store.entityDocs())
	}
	// The read's content is captured as the entity's baseline (by-reference cid).
	fields := fileEnt.Metadata["fields"].(string)
	if !strings.Contains(fields, `"exists"`) || !strings.Contains(fields, "cid-abc") {
		t.Errorf("read entity must record exists + the content baseline cid; got %s", fields)
	}
	// A read writes NO action event.
	for _, d := range store.docs {
		if d.DocumentType == domain.DocTypeMnemonicAction {
			t.Errorf("a read must not write an action event; got %+v", d)
		}
	}
}
