package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0049 D8: equivalent spellings of one real path must canonicalize to one id —
// otherwise a scene's engaged scope fragments. Windows case + separator forms collapse.
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
