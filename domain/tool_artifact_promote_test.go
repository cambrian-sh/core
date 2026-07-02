package domain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type fakeArtCS struct{ data map[string][]byte }

func (f *fakeArtCS) Put(context.Context, []byte, string, []string, string) (CID, error) {
	return "", nil
}
func (f *fakeArtCS) Get(_ context.Context, cid CID) (*ContextNode, error) {
	if d, ok := f.data[string(cid)]; ok {
		return &ContextNode{CID: cid, Data: d}, nil
	}
	return nil, fmt.Errorf("cid not found: %s", cid)
}
func (f *fakeArtCS) Has(context.Context, CID) (bool, error) { return false, nil }
func (f *fakeArtCS) GC(context.Context, []CID) error        { return nil }

type fakeArtVault struct {
	calls     int
	lastBytes []byte
}

func (f *fakeArtVault) Store(content []byte) (string, error) {
	f.calls++
	f.lastBytes = content
	return "vaulthash", nil
}

type fakeArtRecorder struct{ saved []Artifact }

func (f *fakeArtRecorder) SaveArtifact(a Artifact) error {
	f.saved = append(f.saved, a)
	return nil
}

// A confined tool's swept file (surfaced in result "_artifacts") is promoted into
// the durable vault + metadata record AND materialized to the output dir — so it
// survives content-store GC, is retrievable via GetArtifact, and lands on disk.
func TestToolExecutor_persistArtifacts_PromotesAndMaterializes(t *testing.T) {
	const cid = "cid-cambrian"
	content := []byte("hello cambrian summary")
	cs := &fakeArtCS{data: map[string][]byte{cid: content}}
	vault := &fakeArtVault{}
	rec := &fakeArtRecorder{}
	outDir := t.TempDir()

	e := &ToolExecutor{
		ContentStore:      cs,
		ArtifactBytes:     vault,
		ArtifactMeta:      rec,
		ArtifactTags:      func(context.Context, string) []string { return []string{"public_kb"} },
		ArtifactOutputDir: outDir,
	}
	result := []byte(`{"written":"cambrian1.txt","_artifacts":[{"path":"cambrian1.txt","cid":"cid-cambrian","bytes":22}]}`)

	e.persistArtifacts(context.Background(),
		ToolCallRequest{AgentID: "analyst_agent", ToolName: "write_file", SessionTokenID: "sess-1"}, result)

	// Durable vault + metadata.
	if vault.calls != 1 {
		t.Fatalf("expected vault.Store called once, got %d", vault.calls)
	}
	if len(rec.saved) != 1 {
		t.Fatalf("expected one SaveArtifact, got %d", len(rec.saved))
	}
	a := rec.saved[0]
	if a.Hash != "vaulthash" || a.SessionID != "sess-1" || len(a.Tags) != 1 || a.Tags[0] != "public_kb" {
		t.Errorf("artifact metadata not stamped correctly: %+v", a)
	}
	if a.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("expected text/plain content type for .txt, got %q", a.ContentType)
	}

	// Materialized to disk at the requested path.
	got, err := os.ReadFile(filepath.Join(outDir, "cambrian1.txt"))
	if err != nil || string(got) != string(content) {
		t.Errorf("expected materialized file %q, got %q err=%v", content, got, err)
	}
}

// A malicious "../" relative path cannot escape the output directory.
func TestToolExecutor_materialize_ContainsTraversal(t *testing.T) {
	outDir := t.TempDir()
	e := &ToolExecutor{ArtifactOutputDir: outDir}

	e.materialize("write_file", "../escape.txt", []byte("x"))

	if _, err := os.Stat(filepath.Join(filepath.Dir(outDir), "escape.txt")); err == nil {
		t.Error("artifact escaped the output dir via ../ traversal")
	}
	// It is written inside the output dir instead (path anchored).
	if _, err := os.Stat(filepath.Join(outDir, "escape.txt")); err != nil {
		t.Errorf("expected the file contained within outputDir, got err=%v", err)
	}
}

// No-op and nil-safe when artifact promotion is not wired (backward compatible).
func TestToolExecutor_persistArtifacts_NoopWhenUnwired(t *testing.T) {
	e := &ToolExecutor{} // no ContentStore, no vault, no output dir
	// Must not panic and must do nothing.
	e.persistArtifacts(context.Background(), ToolCallRequest{ToolName: "write_file"},
		[]byte(`{"_artifacts":[{"path":"x.txt","cid":"c","bytes":1}]}`))
}
