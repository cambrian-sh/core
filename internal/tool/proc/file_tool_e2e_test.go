package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/tool/discovery"
)

// fakeContentStore is a minimal in-memory domain.ContentStore for the jail-sweep
// test: it records every Put so we can assert a tool's jail-written file was
// offloaded before the jail was torn down.
type fakeContentStore struct {
	puts map[domain.CID][]byte
}

func newFakeContentStore() *fakeContentStore { return &fakeContentStore{puts: map[domain.CID][]byte{}} }

func (f *fakeContentStore) Put(_ context.Context, data []byte, _ string, _ []string, _ string) (domain.CID, error) {
	cid := domain.CID(fmt.Sprintf("cid-%d", len(f.puts)))
	cp := make([]byte, len(data))
	copy(cp, data)
	f.puts[cid] = cp
	return cid, nil
}
func (f *fakeContentStore) Get(_ context.Context, cid domain.CID) (*domain.ContextNode, error) {
	return &domain.ContextNode{CID: cid, Data: f.puts[cid]}, nil
}
func (f *fakeContentStore) Has(_ context.Context, cid domain.CID) (bool, error) {
	_, ok := f.puts[cid]
	return ok, nil
}
func (f *fakeContentStore) GC(_ context.Context, _ []domain.CID) error { return nil }

// A relative-path write lands in the per-call jail and would be lost to
// os.RemoveAll. With a ContentStore wired, the jail is swept on success: the file
// is offloaded to CAS and its CID surfaced in the result "_artifacts". Mutation-
// proof: drop the sweep and the file vanishes with the jail, failing this.
func TestFileTool_JailSweepToCAS(t *testing.T) {
	py := pythonOrSkip(t)
	root := repoRoot()
	if root == "" {
		t.Skip("repo root not found")
	}
	reg := domain.NewInMemoryToolRegistry()
	files, err := discovery.LoadRegistry(filepath.Join(root, "tools"), reg)
	if err != nil {
		t.Fatalf("discover tools: %v", err)
	}
	cas := newFakeContentStore()
	h := &ProcessHandler{PythonExec: py, ToolFiles: files, DefaultTimeout: 10 * time.Second, ContentStore: cas}

	// Relative path → resolves against the jail cwd (the lossy case).
	wargs, _ := json.Marshal(map[string]string{"path": "hello.txt", "content": "Hello from cambrian filesystem"})
	out, err := h.Execute(context.Background(), domain.ToolCall{ToolName: "write_file", ArgsJSON: wargs})
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}

	// The file's bytes must have reached CAS (persisted past jail teardown).
	found := false
	for _, data := range cas.puts {
		if string(data) == "Hello from cambrian filesystem" {
			found = true
		}
	}
	if !found {
		t.Fatalf("jail file was not swept into CAS; puts=%v", cas.puts)
	}

	// And the result must surface the artifact so the agent can find it.
	var res struct {
		Artifacts []artifactRef `json:"_artifacts"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON object: %v (%s)", err, out)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Path != "hello.txt" || res.Artifacts[0].CID == "" {
		t.Errorf("_artifacts = %+v, want one entry for hello.txt with a CID", res.Artifacts)
	}
}

// Capability-unlock demo (DoD): the real file_tool.py, discovered and run through
// the ProcessHandler, performs real file I/O an agent could not do before — and
// the ported binary guard rejects binary reads. Mutation-proof: gut file_tool.py
// and this fails.
func TestFileTool_RealIO(t *testing.T) {
	py := pythonOrSkip(t)
	root := repoRoot()
	if root == "" {
		t.Skip("repo root not found")
	}
	toolsDir := filepath.Join(root, "tools")

	reg := domain.NewInMemoryToolRegistry()
	files, err := discovery.LoadRegistry(toolsDir, reg)
	if err != nil {
		t.Fatalf("discover tools: %v", err)
	}
	if files["read_file"] == "" || files["write_file"] == "" {
		t.Fatalf("file tool not discovered from %s (got %v)", toolsDir, files)
	}
	h := &ProcessHandler{PythonExec: py, ToolFiles: files, DefaultTimeout: 10 * time.Second}

	work := filepath.Join(t.TempDir(), "note.txt")

	// write
	wargs, _ := json.Marshal(map[string]string{"path": work, "content": "hello tools"})
	if _, err := h.Execute(context.Background(), domain.ToolCall{ToolName: "write_file", ArgsJSON: wargs}); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if b, _ := os.ReadFile(work); string(b) != "hello tools" {
		t.Fatalf("file not written correctly: %q", b)
	}

	// read
	rargs, _ := json.Marshal(map[string]string{"path": work})
	out, err := h.Execute(context.Background(), domain.ToolCall{ToolName: "read_file", ArgsJSON: rargs})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	var res map[string]any
	_ = json.Unmarshal(out, &res)
	if res["content"] != "hello tools" {
		t.Errorf("read_file content = %v, want 'hello tools'", res["content"])
	}

	// binary guard: a .png is refused even though the path is allowed
	bin := filepath.Join(t.TempDir(), "img.png")
	os.WriteFile(bin, []byte{0, 1, 2, 3}, 0o644)
	bargs, _ := json.Marshal(map[string]string{"path": bin})
	bout, _ := h.Execute(context.Background(), domain.ToolCall{ToolName: "read_file", ArgsJSON: bargs})
	var bres map[string]any
	_ = json.Unmarshal(bout, &bres)
	if bres["error"] == nil {
		t.Errorf("binary read should be refused, got %v", bres)
	}
}
