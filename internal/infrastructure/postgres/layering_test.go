package postgres

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// ENG-1104 (owner decision 2026-11-14): the Postgres adapter implements domain ports
// and must not depend on the internal/memory service (AGENTS.md §1.3). The memory
// types and ports it needs live in domain/memory_ports.go. This guard keeps the edge
// from coming back.
func TestPostgresAdapterDoesNotImportMemoryService(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "github.com/cambrian-sh/core/internal/memory" {
				t.Errorf("%s imports internal/memory; declare the type or port in domain instead (ENG-1104)", name)
			}
		}
	}
}
