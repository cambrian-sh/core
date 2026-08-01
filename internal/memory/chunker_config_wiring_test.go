package memory

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// Every name the config layer will VALIDATE must be a name the registry can
// actually route to. config.ChunkerConfig.Validate accepts any name in
// KnownChunkerNames, so a name that validates but is not registered is a promise
// the kernel cannot keep — which is exactly what shipped: only "option_c" was
// registered, so `chunker.default: markdown_header` passed validation and then
// did nothing.
//
// This test is the link between the two lists. Adding a name to
// KnownChunkerNames without registering it now fails here.
func TestNewDefaultChunkers_CoversEveryKnownChunkerName(t *testing.T) {
	chunkers := NewDefaultChunkers(&mockEmbedder{vec: []float32{0.1}}, config.ChunkerConfig{})

	for name := range config.KnownChunkerNames {
		c, ok := chunkers[name]
		if !ok {
			t.Errorf("config validates chunker %q but the registry does not register it", name)
			continue
		}
		if c.Name() != name {
			t.Errorf("chunker registered under %q reports Name() = %q", name, c.Name())
		}
	}
}

// The late chunker needs an embedder. With none available it is left out
// deliberately, so a route naming it is rejected loudly by NewRegistry at startup
// rather than nil-panicking on the first document.
func TestNewDefaultChunkers_OmitsLateWithoutEmbedder(t *testing.T) {
	chunkers := NewDefaultChunkers(nil, config.ChunkerConfig{})
	if _, ok := chunkers[lateChunkerName]; ok {
		t.Error("late chunker must not be registered without an embedder")
	}
	// The rest are still available.
	if _, ok := chunkers["option_c"]; !ok {
		t.Error("option_c must be registered regardless of embedder availability")
	}

	_, err := NewRegistry(chunkers, config.ChunkerConfig{
		Default: "option_c",
		Routes:  map[string]string{"slack": lateChunkerName},
	})
	if err == nil {
		t.Error("a route naming an unregistered chunker must be rejected at startup")
	}
}

// The operator's configured default is the one that gets used. This is the
// regression bar for the actual defect: the config block was parsed, defaulted
// and validated, and then the ingestion manager built its registry from a Go
// literal that always said "option_c".
func TestNewIngestionManager_HonoursConfiguredDefault(t *testing.T) {
	im := NewIngestionManager(nil, &mockEmbedder{vec: []float32{0.1}}, testAgent(),
		IngestionConfig{}, config.ChunkerConfig{Default: "markdown_header"})

	got, err := im.registry.Resolve("some_source", ".md")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name() != "markdown_header" {
		t.Errorf("configured default ignored: routed to %q, want %q", got.Name(), "markdown_header")
	}
}

// A sourceType route takes precedence over the default — the routing table the
// `chunker.routes` block describes is real.
func TestNewIngestionManager_HonoursConfiguredRoutes(t *testing.T) {
	im := NewIngestionManager(nil, &mockEmbedder{vec: []float32{0.1}}, testAgent(),
		IngestionConfig{}, config.ChunkerConfig{
			Default:   "option_c",
			Routes:    map[string]string{"repo_scan": "ast_go"},
			ExtRoutes: map[string]string{".md": "markdown_header"},
		})

	if got, _ := im.registry.Resolve("repo_scan", ".go"); got.Name() != "ast_go" {
		t.Errorf("sourceType route ignored: got %q, want ast_go", got.Name())
	}
	if got, _ := im.registry.Resolve("unrouted_source", ".md"); got.Name() != "markdown_header" {
		t.Errorf("ext route ignored: got %q, want markdown_header", got.Name())
	}
	// Neither route matches ⇒ the configured default.
	if got, _ := im.registry.Resolve("unrouted_source", ".txt"); got.Name() != "option_c" {
		t.Errorf("default ignored: got %q, want option_c", got.Name())
	}
}

// ADR-0060 D6: the late chunker is opt-in. While the gate is closed a route
// naming it degrades to the default rather than being refused.
func TestNewIngestionManager_LateGateFollowsConfig(t *testing.T) {
	cfg := config.ChunkerConfig{
		Default: "option_c",
		Routes:  map[string]string{"bulk": lateChunkerName},
	}

	off := NewIngestionManager(nil, &mockEmbedder{vec: []float32{0.1}}, testAgent(), IngestionConfig{}, cfg)
	if got, _ := off.registry.Resolve("bulk", ".txt"); got.Name() != "option_c" {
		t.Errorf("late gate closed should fall back to default, got %q", got.Name())
	}

	cfg.Late.Enabled = true
	on := NewIngestionManager(nil, &mockEmbedder{vec: []float32{0.1}}, testAgent(), IngestionConfig{}, cfg)
	if got, _ := on.registry.Resolve("bulk", ".txt"); got.Name() != lateChunkerName {
		t.Errorf("late gate open should route to late, got %q", got.Name())
	}
}

// An unusable config is logged and degraded, never a nil registry: refusing to
// ingest anything is a worse answer to one bad route than ingesting with the
// documented default.
func TestNewIngestionManager_InvalidConfigFallsBackToOptionC(t *testing.T) {
	im := NewIngestionManager(nil, &mockEmbedder{vec: []float32{0.1}}, testAgent(),
		IngestionConfig{}, config.ChunkerConfig{Default: "no_such_chunker"})

	if im.registry == nil {
		t.Fatal("registry must never be nil")
	}
	if got, _ := im.registry.Resolve("any", ".txt"); got.Name() != "option_c" {
		t.Errorf("expected fallback to option_c, got %q", got.Name())
	}
}
