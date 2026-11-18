package kernel

import (
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// ENG-1504: NewMemoryBackend binds EVERY capability to the concrete adapter. If
// *postgres.PgVectorAdapter stopped implementing one of the ports this file and
// memory_backend.go would not compile; this test pins that nothing is left nil.
func TestNewMemoryBackend_BindsEveryCapabilityToTheAdapter(t *testing.T) {
	a := &postgres.PgVectorAdapter{}
	b := NewMemoryBackend(a)
	if m := b.Missing(); len(m) != 0 {
		t.Fatalf("a backend built from the adapter lacks %v", m)
	}
	v := reflect.ValueOf(b)
	for i := 0; i < v.NumField(); i++ {
		if v.Type().Field(i).Name == "Eraser" {
			// Built over the knowledge store, not the adapter; app.go sets it (ADR-0137 S2).
			continue
		}
		if got := v.Field(i).Interface(); got != any(a) {
			t.Errorf("field %s = %T, want the one adapter", v.Type().Field(i).Name, got)
		}
	}
}

func TestNewMemoryBackend_NilAdapterIsRefusedAtBoot(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewMemoryBackend(nil) must panic, not yield a backend of typed-nil stores")
		}
	}()
	NewMemoryBackend(nil)
}

// A hand-built partial backend (a test fake) names exactly what it lacks, and the
// stack leaves those features off instead of probing the vector store for them.
func TestMemoryBackend_PartialBackendNamesItsGapsAndStackLeavesThemOff(t *testing.T) {
	b := MemoryBackend{Vector: plainVec{}}
	want := []string{"Graph", "Experience", "ChunkTriplets", "Lexical", "Documents", "RankSignals", "Sections"}
	if got := b.Missing(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Missing() = %v, want %v", got, want)
	}
	cfg := config.DefaultConfig()
	mem := NewMemoryStack(b, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{})
	if mem.ChunkTripletsBatcher != nil || mem.DocumentStore != nil || mem.Agent.ExperienceStore != nil {
		t.Fatalf("absent capabilities must leave their features off: batcher=%v docs=%v exp=%v",
			mem.ChunkTripletsBatcher, mem.DocumentStore, mem.Agent.ExperienceStore)
	}
}

func TestNewMemoryStack_NilVectorIsRefused(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewMemoryStack with no Vector must panic at composition")
		}
	}()
	cfg := config.DefaultConfig()
	NewMemoryStack(MemoryBackend{}, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{})
}
