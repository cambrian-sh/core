package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// lateChunkerStub is a stand-in for the LateChunker that T-2.4 will
// implement. It satisfies domain.Chunker so the registry can include
// "late" in the default chunker map (the spec's "5 known chunkers"),
// and its Supports always returns false so a routing step that would
// otherwise dispatch to "late" falls through to the default. This
// matches the spec: the registry just registers the name; the actual
// gate (embedder.supports_long_context, doc size, etc.) lives in
// LateChunker itself per ADR-0060 D6.
type lateChunkerStub struct{}

func (lateChunkerStub) Name() string { return "late" }

func (lateChunkerStub) Supports(sourceType, ext string) bool {
	_ = sourceType
	_ = ext
	return false
}

func (lateChunkerStub) Chunk(ctx context.Context, doc *domain.ExternalDocument) ([]domain.Chunk, error) {
	_ = ctx
	_ = doc
	return nil, errors.New("late chunker: not implemented in T-1.8 (T-2.4 wires the gate)")
}

// defaultChunkers returns the 5 known chunkers the v1 registry
// registers. Used by the tests below to construct a Registry without
// duplicating the wiring in every test. The map is a fresh copy on
// every call so tests cannot mutate each other's chunker sets.
func defaultChunkers() map[string]domain.Chunker {
	return map[string]domain.Chunker{
		"option_c":            OptionCChunker{},
		"recursive_character": NewRecursiveCharacterChunker(0, 0),
		"ast_go":              ASTGoChunker{},
		"markdown_header":     MarkdownHeaderChunker{},
		"late":                lateChunkerStub{},
	}
}

func TestNewRegistry_Default(t *testing.T) {
	cfg := ChunkerConfig{
		Default: "option_c",
	}
	reg, err := NewRegistry(defaultChunkers(), cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if reg == nil {
		t.Fatal("NewRegistry returned nil registry")
	}
	if reg.defaultChr != "option_c" {
		t.Errorf("defaultChr = %q, want %q", reg.defaultChr, "option_c")
	}
	if len(reg.routes) != 0 {
		t.Errorf("routes should be empty for default config, got %d entries", len(reg.routes))
	}

	// Any (sourceType, ext) resolves to OptionCChunker.
	for _, tc := range []struct{ st, ext string }{
		{"file_drop", ".md"},
		{"slack", ""},
		{"email", ".eml"},
		{"unknown", ".unknown"},
		{"", ""},
	} {
		got, err := reg.Resolve(tc.st, tc.ext)
		if err != nil {
			t.Errorf("Resolve(%q, %q): %v", tc.st, tc.ext, err)
			continue
		}
		if got.Name() != "option_c" {
			t.Errorf("Resolve(%q, %q) = %q, want %q", tc.st, tc.ext, got.Name(), "option_c")
		}
	}
}

func TestNewRegistry_InvalidDefault(t *testing.T) {
	cfg := ChunkerConfig{
		Default: "nonexistent",
	}
	_, err := NewRegistry(defaultChunkers(), cfg)
	if err == nil {
		t.Fatal("NewRegistry with invalid default returned nil error, want an error")
	}
}

func TestNewRegistry_InvalidRoute(t *testing.T) {
	cfg := ChunkerConfig{
		Default: "option_c",
		Routes:  map[string]string{"file_drop": "nonexistent"},
	}
	_, err := NewRegistry(defaultChunkers(), cfg)
	if err == nil {
		t.Fatal("NewRegistry with route to unknown chunker returned nil error, want an error")
	}
}

func TestNewRegistry_InvalidExtRoute(t *testing.T) {
	cfg := ChunkerConfig{
		Default:   "option_c",
		ExtRoutes: map[string]string{".go": "ghost"},
	}
	_, err := NewRegistry(defaultChunkers(), cfg)
	if err == nil {
		t.Fatal("NewRegistry with ext_route to unknown chunker returned nil error, want an error")
	}
}

func TestNewRegistry_NilChunkers(t *testing.T) {
	cfg := ChunkerConfig{Default: "option_c"}
	if _, err := NewRegistry(nil, cfg); err == nil {
		t.Fatal("NewRegistry with nil chunkers map returned nil error, want an error")
	}
}

func TestNewRegistry_EmptyDefault(t *testing.T) {
	cfg := ChunkerConfig{}
	if _, err := NewRegistry(defaultChunkers(), cfg); err == nil {
		t.Fatal("NewRegistry with empty Default returned nil error, want an error")
	}
}

func TestRegistry_Resolve_SourceTypeMatch(t *testing.T) {
	cfg := ChunkerConfig{
		Default: "option_c",
		Routes:  map[string]string{"file_drop": "markdown_header"},
	}
	reg, err := NewRegistry(defaultChunkers(), cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, err := reg.Resolve("file_drop", ".md")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name() != "markdown_header" {
		t.Errorf("Resolve(file_drop, .md) = %q, want %q", got.Name(), "markdown_header")
	}
}

func TestRegistry_Resolve_SourceTypeMiss_ExtHit(t *testing.T) {
	cfg := ChunkerConfig{
		Default:   "option_c",
		ExtRoutes: map[string]string{".go": "ast_go"},
	}
	reg, err := NewRegistry(defaultChunkers(), cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, err := reg.Resolve("file_drop", ".go")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name() != "ast_go" {
		t.Errorf("Resolve(file_drop, .go) = %q, want %q", got.Name(), "ast_go")
	}
}

func TestRegistry_Resolve_DefaultFallback(t *testing.T) {
	cfg := ChunkerConfig{
		Default: "option_c",
	}
	reg, err := NewRegistry(defaultChunkers(), cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, err := reg.Resolve("unknown", ".unknown")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name() != "option_c" {
		t.Errorf("Resolve(unknown, .unknown) = %q, want %q (default)", got.Name(), "option_c")
	}
}

func TestRegistry_Resolve_SupportsFalse(t *testing.T) {
	// Route "file_drop" → "markdown_header", but ask for a .go file:
	// MarkdownHeaderChunker.Supports(.go) = false, so the registry
	// must fall through to the default rather than dispatching to
	// markdown_header anyway.
	cfg := ChunkerConfig{
		Default: "option_c",
		Routes:  map[string]string{"file_drop": "markdown_header"},
	}
	reg, err := NewRegistry(defaultChunkers(), cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, err := reg.Resolve("file_drop", ".go")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name() != "option_c" {
		t.Errorf("Resolve(file_drop, .go) = %q, want %q (default after Supports-false fallthrough)", got.Name(), "option_c")
	}
}

func TestRegistry_Resolve_SourceTypeBeatsExt(t *testing.T) {
	// When both sourceType and ext would route somewhere, sourceType
	// wins (the precedence rule, ADR-0060 D5).
	cfg := ChunkerConfig{
		Default:   "option_c",
		Routes:    map[string]string{"file_drop": "markdown_header"},
		ExtRoutes: map[string]string{".md": "ast_go"},
	}
	reg, err := NewRegistry(defaultChunkers(), cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, err := reg.Resolve("file_drop", ".md")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name() != "markdown_header" {
		t.Errorf("Resolve(file_drop, .md) = %q, want %q (sourceType wins over ext)", got.Name(), "markdown_header")
	}
}

func TestRegistry_Resolve_RegistryMap(t *testing.T) {
	// The 5 known chunkers must all be present in the registry's
	// chunker map with their stable names.
	chunkers := defaultChunkers()
	reg, err := NewRegistry(chunkers, ChunkerConfig{Default: "option_c"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	want := []string{
		"option_c",
		"recursive_character",
		"ast_go",
		"markdown_header",
		"late",
	}
	if len(reg.chunkers) != len(want) {
		t.Errorf("registry chunker map size = %d, want %d", len(reg.chunkers), len(want))
	}
	for _, name := range want {
		c, ok := reg.chunkers[name]
		if !ok {
			t.Errorf("registry chunker map missing %q", name)
			continue
		}
		if c.Name() != name {
			t.Errorf("registry chunker %q has Name() = %q", name, c.Name())
		}
	}
}

func TestLateChunker_Gated(t *testing.T) {
	chunkers := map[string]domain.Chunker{
		"option_c":            OptionCChunker{},
		"recursive_character": NewRecursiveCharacterChunker(0, 0),
		"ast_go":              ASTGoChunker{},
		"markdown_header":     MarkdownHeaderChunker{},
		"late":                LateChunker{},
	}
	cfg := ChunkerConfig{
		Default: "option_c",
		Routes:  map[string]string{"file_drop": "late"},
	}

	t.Run("closed gate", func(t *testing.T) {
		reg, err := NewRegistry(chunkers, cfg)
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		reg.SetLateGate(func() bool { return false })
		got, err := reg.Resolve("file_drop", ".md")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Name() != "option_c" {
			t.Errorf("Resolve(file_drop, .md) with closed gate = %q, want %q (default)", got.Name(), "option_c")
		}
	})

	t.Run("open gate, no long-context", func(t *testing.T) {
		reg, err := NewRegistry(chunkers, cfg)
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		reg.SetLateGate(func() bool { return false })
		got, err := reg.Resolve("file_drop", ".md")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Name() != "option_c" {
			t.Errorf("Resolve(file_drop, .md) with gate=false (one condition fails) = %q, want %q (default)", got.Name(), "option_c")
		}
	})

	t.Run("open gate, long-context", func(t *testing.T) {
		reg, err := NewRegistry(chunkers, cfg)
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		reg.SetLateGate(func() bool { return true })
		got, err := reg.Resolve("file_drop", ".md")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Name() != "late" {
			t.Errorf("Resolve(file_drop, .md) with open gate = %q, want %q", got.Name(), "late")
		}
	})
}
