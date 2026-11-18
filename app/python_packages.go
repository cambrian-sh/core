package app

import (
	"context"
	"log/slog"
	"runtime"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/pypackages"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// wirePythonPackages builds the per-agent package layer service (ADR-0157
// D7.6-D7.8, P3) over the managed env's builder, THE approval hub and the 0057
// tables, and registers the python_package kind handler on the hub. It returns
// nil (logged) when the env is unavailable on this host; the agent manager
// then refuses every agent that declares python_packages (fail closed), and
// agents that declare none are unaffected.
//
// Without the 0057 tables the service is still built: an agent that declares
// packages is refused naming the missing migration, because an approval that
// would not survive a restart is not the approval D7.8 requires.
func wirePythonPackages(ctx context.Context, pc config.AgentPythonConfig, pool *pgxpool.Pool, env pythonEnvGate,
	hub *domain.AddressedHub, manifests func(ctx context.Context, agentID string) (*domain.AgentManifest, error)) *pypackages.Service {
	b := env.Builder()
	if b == nil {
		slog.Warn("ADR-0157 D7: no managed Python env on this host; agents that declare python_packages will be refused")
		return nil
	}
	var store domain.PythonPackageStore
	if pool != nil {
		pg := postgres.NewPgPythonPackageStore(pool)
		if pg.Ready(ctx) {
			store = pg
		} else {
			slog.Warn("ADR-0157 D7: python package approval tables missing (run `orchestrator migrate up`, migration 0057); agents that declare python_packages will be refused")
		}
	}
	// ADR-0157 D7.7.5: on Windows a new layer gets its agent's own AppContainer
	// read ACE while it is still empty, so no other agent's container can read
	// it and the launch path never walks it. The Unix runners bind paths.
	var prepare func(agentID, dir string) error
	if runtime.GOOS == "windows" {
		prepare = sandbox.GrantAgentPrivateRead
	}
	svc, err := pypackages.New(store, hub, b, pypackages.Config{
		ApprovalTTL: pc.ApprovalTTL(),
		Prepare:     prepare,
		Base:        env.Interpreter,
		Declared: func(ctx context.Context, agentID string) []domain.PythonPackage {
			m, err := manifests(ctx, agentID)
			if err != nil || m == nil {
				return nil
			}
			return m.PythonPackages
		},
	})
	if err != nil {
		slog.Error("ADR-0157 D7: python package service not built; agents that declare python_packages will be refused", "err", err)
		return nil
	}
	slog.Info("ADR-0157 D7: per-agent python package layers wired", "durable_approvals", store != nil,
		"approval_ttl", pc.ApprovalTTL().String())
	return svc
}
