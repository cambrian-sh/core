package agentmgr

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ctxRecordingRegistry records the ctx of the registry read SpawnDaemon makes.
type ctxRecordingRegistry struct {
	*testRegistry
	got context.Context
}

func (r *ctxRecordingRegistry) GetAgentByName(ctx context.Context, name string) (*domain.AgentDefinition, error) {
	r.got = ctx
	return r.testRegistry.GetAgentByName(ctx, name)
}

// ENG-1467 item 1 (AGENTS.md §2.7): SpawnDaemon runs on the caller's ctx — the
// registry read sees the caller's values (here the principal), where it used to
// run on context.Background().
func TestSpawnDaemon_UsesTheCallersContext(t *testing.T) {
	reg := &ctxRecordingRegistry{testRegistry: newTestRegistry()}
	m := withPythonM(NewAgentManager(reg, "", nil), "python")
	caller := domain.AgentPrincipal("operator-x")
	ctx := domain.WithPrincipal(context.Background(), caller)

	// "missing" is not registered: the spawn fails at the registry read, before any
	// process exists, which is all this test needs.
	if _, err := m.SpawnDaemon(ctx, "missing", "stream-1", nil); err == nil {
		t.Fatal("spawn of an unregistered agent succeeded")
	}
	if reg.got == nil || domain.PrincipalFromContext(reg.got) != caller {
		t.Fatalf("registry read ran without the caller's ctx values: %v", reg.got)
	}
}
