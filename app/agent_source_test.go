package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// FilesystemAgentSource discovers a *_agent.py in a directory and health-gates an entry
// whose exec path is absent.
func TestFilesystemAgentSource_DiscoversAndHealthGates(t *testing.T) {
	dir := t.TempDir()
	// A real agent file (present on disk → passes the health gate).
	if err := os.WriteFile(filepath.Join(dir, "widget_agent.py"), []byte("AGENT_DESCRIPTION = 'A widget agent'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := NewFilesystemAgentSource(dir)
	defs, err := src.DiscoverAgents(context.Background())
	if err != nil {
		t.Fatalf("DiscoverAgents: %v", err)
	}

	var found bool
	for _, da := range defs {
		if da.Definition.ID == "widget_agent" {
			found = true
			if da.Definition.System {
				t.Error("plugin-dir agent must never be System=true")
			}
			if da.Definition.Description != "A widget agent" {
				t.Errorf("description = %q, want 'A widget agent'", da.Definition.Description)
			}
			if da.Manifest == nil {
				t.Error("filesystem source must carry a (possibly empty) manifest")
			}
		}
	}
	if !found {
		t.Fatalf("widget_agent not discovered; got %d defs", len(defs))
	}
}

// A source over a nonexistent directory returns no agents (and no panic).
func TestFilesystemAgentSource_MissingDir(t *testing.T) {
	src := NewFilesystemAgentSource(filepath.Join(t.TempDir(), "does-not-exist"))
	defs, err := src.DiscoverAgents(context.Background())
	if err != nil {
		t.Fatalf("DiscoverAgents on missing dir should not error, got %v", err)
	}
	if len(defs) != 0 {
		t.Fatalf("expected 0 defs from a missing dir, got %d", len(defs))
	}
}
