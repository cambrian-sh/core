package mcp

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// newTestToolExecutor builds an executor through the one constructor
// (domain.NewToolExecutor; AGENTS.md §2.3), defaulting the required controls a
// test does not exercise: an empty registry, empty grants, the OSS allow-all
// authorizer.
func newTestToolExecutor(t testing.TB, c domain.ToolExecutorControls, f domain.ToolExecutor) *domain.ToolExecutor {
	t.Helper()
	if c.Registry == nil {
		c.Registry = domain.NewInMemoryToolRegistry()
	}
	if c.Grants == nil {
		c.Grants = domain.NewInMemoryGrantsStore()
	}
	if c.Authz == nil {
		c.Authz = domain.AllowAllAuthorizer{}
	}
	te, err := domain.NewToolExecutor(c, f)
	if err != nil {
		t.Fatal(err)
	}
	return te
}
