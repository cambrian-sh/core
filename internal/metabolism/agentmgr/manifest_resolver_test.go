package agentmgr

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// manifestErrRegistry fails every manifest read.
type manifestErrRegistry struct{ *testRegistry }

func (manifestErrRegistry) GetManifest(context.Context, string) (*domain.AgentManifest, error) {
	return nil, errors.New("registry unavailable")
}

// A manifest read error falls back to the global memory cap, but never silently
// (AGENTS.md §3.2; T-20261113-agentmgr-drops-the-caller-s-ctx-and-an-error).
// Not parallel: it swaps the default slog logger.
func TestManifestResolver_ReadErrorIsLoggedNotDiscarded(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	m := NewAgentManager(manifestErrRegistry{newTestRegistry()}, "python", "", nil)
	if got := m.InstanceManager.manifestFor("worker-1"); got != nil {
		t.Fatalf("manifest = %+v, want nil fallback on a read error", got)
	}
	if out := buf.String(); !strings.Contains(out, "worker-1") || !strings.Contains(out, "registry unavailable") {
		t.Fatalf("read error was not logged; log = %q", out)
	}
}
