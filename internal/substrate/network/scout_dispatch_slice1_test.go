package network

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/discovery"
)

// TestScoutDispatcher_DeterministicFirst proves the ADR-0078 wiring: with the LLM tier off
// and no auctioneer, the dispatcher still produces structured findings from the deterministic
// registry, and persists them to session memory (D5).
func TestScoutDispatcher_DeterministicFirst(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "helicopter"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"intro.md", "rotor.md"} {
		if err := os.WriteFile(filepath.Join(root, "helicopter", n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	d := &AgentScoutDispatcher{
		Registry:       discovery.NewRegistry(discovery.NewFilesystemSource(root)),
		LLMTierEnabled: false, // deterministic only — no auctioneer needed/called
		// Auctioneer intentionally nil: Discover must never touch it when the tier is off.
	}
	ctx := domain.WithSessionID(context.Background(), "sess-1")
	report := d.Discover(ctx, "continue the helicopter folder where we left off")

	if report.Environment == nil {
		t.Error("env grounding missing")
	}
	var found bool
	for _, e := range report.Entities {
		if e.ID == "helicopter" && e.Exists && e.Kind == "dir" {
			found = true
			if !strings.Contains(e.Summary, "2 docs") {
				t.Errorf("dir summary missing doc count: %q", e.Summary)
			}
		}
	}
	if !found {
		t.Fatalf("deterministic discovery did not observe the helicopter dir: %+v", report.Entities)
	}

	// Session memory (D5): findings cached under the session id, and clearable.
	if cached, ok := d.SessionDiscovery("sess-1"); !ok || cached == nil {
		t.Error("findings not persisted to session memory")
	}
	d.ClearSession("sess-1")
	if _, ok := d.SessionDiscovery("sess-1"); ok {
		t.Error("ClearSession did not drop the session findings")
	}
}

// TestScoutDispatcher_EmptyRegistryEnvOnly proves that with no registry and the LLM tier
// off, Discover degrades to an env-only report (ADR-0051 D8 kept).
func TestScoutDispatcher_EmptyRegistryEnvOnly(t *testing.T) {
	d := &AgentScoutDispatcher{}
	report := d.Discover(context.Background(), "just make a new thing from scratch")
	if report.Environment == nil {
		t.Error("env grounding must still be present")
	}
	if len(report.Entities) != 0 {
		t.Errorf("no registry ⇒ no entities, got %+v", report.Entities)
	}
}
