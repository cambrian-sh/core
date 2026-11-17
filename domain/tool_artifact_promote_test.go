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

type fakeArtLedger struct{ recs []LedgerRecord }

func (f *fakeArtLedger) Record(_ context.Context, rec LedgerRecord) (LedgerEntry, error) {
	f.recs = append(f.recs, rec)
	return LedgerEntry{ID: "led_art", Kind: rec.Kind}, nil
}

// A confined tool's swept file (surfaced in result "_artifacts") is recorded as a
// run-ledger artifact entry — the one artifact registry (ADR-0157 D5.2), so it
// survives content-store GC and is retrievable via GetArtifact by its ledger id —
// with the writer and the calling instance from the verified context, AND
// materialized to the output dir.
func TestToolExecutor_persistArtifacts_RecordsToLedgerAndMaterializes(t *testing.T) {
	t.Parallel()
	const cid = "cid-cambrian"
	content := []byte("hello cambrian summary")
	cs := &fakeArtCS{data: map[string][]byte{cid: content}}
	led := &fakeArtLedger{}
	outDir := t.TempDir()

	e := &ToolExecutor{ContentStore: cs, Ledger: led, ArtifactOutputDir: outDir}
	result := []byte(`{"written":"cambrian1.txt","_artifacts":[{"path":"cambrian1.txt","cid":"cid-cambrian","bytes":22}]}`)

	ctx := WithAgentInstance(context.Background(), "inst-7")
	e.persistArtifacts(ctx, ToolCallRequest{AgentID: "analyst_agent", ToolName: "write_file", SessionTokenID: "sess-1"}, result)

	if len(led.recs) != 1 {
		t.Fatalf("expected one ledger record, got %d", len(led.recs))
	}
	r := led.recs[0]
	if r.Kind != LedgerArtifact || string(r.Body) != string(content) || r.Writer != AgentPrincipal("analyst_agent") ||
		r.InstanceID != "inst-7" || r.ToolName != "write_file" {
		t.Errorf("artifact record not stamped correctly: %+v", r)
	}
	if r.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("expected text/plain content type for .txt, got %q", r.ContentType)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "cambrian1.txt"))
	if err != nil || string(got) != string(content) {
		t.Errorf("expected materialized file %q, got %q err=%v", content, got, err)
	}
}

// A system call (no principal's run) records nothing, but still materializes.
func TestToolExecutor_persistArtifacts_SystemCallNotRecorded(t *testing.T) {
	t.Parallel()
	cs := &fakeArtCS{data: map[string][]byte{"c": []byte("x")}}
	led := &fakeArtLedger{}
	e := &ToolExecutor{ContentStore: cs, Ledger: led}
	e.persistArtifacts(context.Background(), ToolCallRequest{ToolName: "write_file", System: true},
		[]byte(`{"_artifacts":[{"path":"x.txt","cid":"c","bytes":1}]}`))
	if len(led.recs) != 0 {
		t.Fatalf("a system call recorded %d artifact entries", len(led.recs))
	}
}

// A malicious "../" relative path cannot escape the output directory.
func TestToolExecutor_materialize_ContainsTraversal(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	e := &ToolExecutor{} // no ContentStore, no vault, no output dir
	// Must not panic and must do nothing.
	e.persistArtifacts(context.Background(), ToolCallRequest{ToolName: "write_file"},
		[]byte(`{"_artifacts":[{"path":"x.txt","cid":"c","bytes":1}]}`))
}
