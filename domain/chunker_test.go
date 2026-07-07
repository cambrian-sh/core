package domain

import (
	"context"
	"errors"
	"testing"
)

// supportsCall records one invocation of a mockChunker's Supports method.
type supportsCall struct {
	sourceType string
	ext        string
}

// mockChunker is a hand-rolled Chunker used to drive the interface-contract
// tests below. It records every call so the runtime test can assert the
// contract at the call boundary, not just at the type level.
type mockChunker struct {
	name    string
	supports func(sourceType, ext string) bool
	chunk   func(ctx context.Context, doc *ExternalDocument) ([]Chunk, error)

	nameCalls    int
	supportsHits []supportsCall
	chunkCalls   []*ExternalDocument
}

func (m *mockChunker) Name() string {
	m.nameCalls++
	return m.name
}

func (m *mockChunker) Supports(sourceType string, ext string) bool {
	m.supportsHits = append(m.supportsHits, supportsCall{sourceType, ext})
	if m.supports == nil {
		return false
	}
	return m.supports(sourceType, ext)
}

func (m *mockChunker) Chunk(ctx context.Context, doc *ExternalDocument) ([]Chunk, error) {
	m.chunkCalls = append(m.chunkCalls, doc)
	if m.chunk == nil {
		return nil, errors.New("mockChunker: chunk func not set")
	}
	return m.chunk(ctx, doc)
}

// Compile-time assertion: *mockChunker must satisfy the Chunker port. If the
// port signature ever drifts in a way the mock doesn't follow, this file
// stops compiling — the test cannot silently regress.
var _ Chunker = (*mockChunker)(nil)

// TestChunkerInterface_CompileTime also surfaces the compile-time assertion
// to the test report so a reader can see the port contract is being verified.
func TestChunkerInterface_CompileTime(t *testing.T) {
	var c Chunker = &mockChunker{name: "compile_time"}
	if c == nil {
		t.Fatal("expected non-nil Chunker after assignment")
	}
	if got := c.Name(); got != "compile_time" {
		t.Errorf("Name() = %q, want %q", got, "compile_time")
	}
}

// TestChunkerInterface_RuntimeContract exercises Name, Supports, and Chunk
// through a real mock and asserts the recorded call shape: that the mock
// receives the values the port advertises, and that the returned chunks
// have the expected Body / Metadata shape.
func TestChunkerInterface_RuntimeContract(t *testing.T) {
	m := &mockChunker{
		name: "recursive_character",
		supports: func(sourceType, ext string) bool {
			return ext == ".go"
		},
		chunk: func(ctx context.Context, doc *ExternalDocument) ([]Chunk, error) {
			return []Chunk{
				{Body: "package x\n", Metadata: map[string]any{"i": 0}},
				{Body: "func A() {}\n", Metadata: map[string]any{"i": 1}},
			}, nil
		},
	}

	if got, want := m.Name(), "recursive_character"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}

	if !m.Supports("", ".go") {
		t.Error("Supports(\"\", \".go\") = false, want true")
	}
	if m.Supports("file_drop", ".txt") {
		t.Error("Supports(\"file_drop\", \".txt\") = true, want false")
	}
	// Both-empty is a valid input to Supports; the contract says either arg
	// may be empty, not that the answer must be true. Verify it doesn't panic.
	_ = m.Supports("", "")

	doc := &ExternalDocument{
		SourceURI:  "file:///tmp/x.go",
		SourceType: "file_drop",
		Body:       "package x\nfunc A() {}\n",
	}
	chunks, err := m.Chunk(context.Background(), doc)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks: want 2, got %d", len(chunks))
	}
	if chunks[0].Body != "package x\n" {
		t.Errorf("chunks[0].Body = %q, want %q", chunks[0].Body, "package x\n")
	}
	if chunks[1].Body != "func A() {}\n" {
		t.Errorf("chunks[1].Body = %q, want %q", chunks[1].Body, "func A() {}\n")
	}
	if got, want := chunks[0].Metadata["i"], 0; got != want {
		t.Errorf("chunks[0].Metadata[i] = %v, want %v", got, want)
	}

	if m.nameCalls != 1 {
		t.Errorf("Name() calls = %d, want 1", m.nameCalls)
	}
	if len(m.supportsHits) != 3 {
		t.Errorf("Supports() calls = %d, want 3", len(m.supportsHits))
	}
	if len(m.chunkCalls) != 1 || m.chunkCalls[0] != doc {
		t.Error("Chunk() did not record the expected doc pointer")
	}
}

// TestChunkerInterface_ChunkErrorPropagates asserts that a chunker
// implementation's error return path is honoured by the port contract.
func TestChunkerInterface_ChunkErrorPropagates(t *testing.T) {
	sentinel := errors.New("chunking failed")
	m := &mockChunker{
		name:    "broken",
		supports: func(string, string) bool { return true },
		chunk:   func(context.Context, *ExternalDocument) ([]Chunk, error) { return nil, sentinel },
	}
	if _, err := m.Chunk(context.Background(), &ExternalDocument{}); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}

// TestChunkerInterface_ChunkMetadataRequired asserts the nil-Metadata
// contract: a chunker is allowed to return a chunk with Metadata == nil
// (the IngestionManager fills in Metadata["chunk_relations"] afterwards),
// and reading a missing key from such a chunk must not panic.
func TestChunkerInterface_ChunkMetadataRequired(t *testing.T) {
	m := &mockChunker{
		name:    "sparse",
		supports: func(string, string) bool { return true },
		chunk: func(context.Context, *ExternalDocument) ([]Chunk, error) {
			return []Chunk{{Body: "hello", Metadata: nil}}, nil
		},
	}
	chunks, err := m.Chunk(context.Background(), &ExternalDocument{Body: "hello"})
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Body != "hello" {
		t.Errorf("Body = %q, want %q", chunks[0].Body, "hello")
	}
	if chunks[0].Metadata != nil {
		t.Errorf("Metadata: want nil, got %v", chunks[0].Metadata)
	}
	// Reading a missing key from a nil map is safe — this is the contract
	// the IngestionManager relies on. If this line panics, the test fails
	// loudly rather than crashing the suite.
	if v, ok := chunks[0].Metadata["chunk_relations"]; ok {
		t.Errorf("chunk_relations: want missing, got %v", v)
	}
}

// TestChunkerInterface_EmptyBody covers the malformed-input edge case:
// a doc with an empty body is a valid ExternalDocument, and the chunker
// must not panic on it. The expected behaviour is implementation-defined
// (a single empty chunk, or none); the contract is just "no panic, no
// error" so the caller can route the result through the normal pipeline.
func TestChunkerInterface_EmptyBody(t *testing.T) {
	m := &mockChunker{
		name:    "noop",
		supports: func(string, string) bool { return true },
		chunk: func(context.Context, *ExternalDocument) ([]Chunk, error) {
			return []Chunk{{Body: ""}}, nil
		},
	}
	chunks, err := m.Chunk(context.Background(), &ExternalDocument{Body: ""})
	if err != nil {
		t.Fatalf("Chunk on empty body: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Body != "" {
		t.Errorf("Body = %q, want empty", chunks[0].Body)
	}
}
