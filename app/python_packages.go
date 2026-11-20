package app

import (
	"context"
	"log/slog"
	"runtime"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/pyenv"
	"github.com/cambrian-sh/core/internal/pypackages"
	"github.com/cambrian-sh/core/internal/sandbox"
	"github.com/cambrian-sh/core/internal/substrate/operator"
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
	hub *domain.AddressedHub, manifests func(ctx context.Context, agentID string) (*domain.AgentManifest, error),
	onLayerChange func(agentID string)) *pypackages.Service {
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
		ApprovalTTL:   pc.ApprovalTTL(),
		Prepare:       prepare,
		Base:          env.Interpreter,
		OnLayerChange: onLayerChange,
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

// leaseResolver is the gateway's lease lookup (SubstrateLLMGateway).
type leaseResolver interface {
	ResolveLease(domain.LeaseID) (domain.LeaseBinding, bool)
}

// wirePythonPackageTool registers request_python_package (ADR-0157 D7.9, P4)
// as a kernel tool when the package service exists. It is offered only to
// Python agents whose profile allowlist names it, never to a subtask worker;
// each call raises (or reuses) the python_package card, so the Operator's
// approval is what installs anything.
func wirePythonPackageTool(pc config.AgentPythonConfig, svc *pypackages.Service, exec *domain.ToolExecutor,
	gateway any, reg domain.AgentRegistry) {
	if svc == nil || exec == nil || reg == nil {
		return
	}
	lr, ok := gateway.(leaseResolver)
	if !ok {
		slog.Warn("ADR-0157 D7.9: request_python_package not offered: the LLM gateway resolves no leases")
		return
	}
	subtasks := exec.Subtasks()
	tool, err := pypackages.NewTool(svc, pc.RequestWait(), pypackages.ToolDeps{
		LeaseAgent: func(l domain.LeaseID) (string, bool) {
			b, ok := lr.ResolveLease(l)
			return b.AgentID, ok
		},
		IsChild: subtasks.IsChild,
		Agent: func(ctx context.Context, agentID string) (*domain.AgentDefinition, *domain.AgentManifest, error) {
			def, err := reg.GetAgentByName(ctx, agentID)
			if err != nil {
				return nil, nil, err
			}
			m, err := reg.GetManifest(ctx, agentID)
			if err != nil {
				return def, nil, err
			}
			return def, m, nil
		},
	})
	if err != nil {
		slog.Error("ADR-0157 D7.9: request_python_package not wired", "err", err)
		return
	}
	exec.KernelTools = append(exec.KernelTools, tool)
	slog.Info("ADR-0157 D7.9: request_python_package offered to allowlisted Python agents", "wait", pc.RequestWait().String())
}

// pythonPackageAdminOrNil keeps a nil *Admin from becoming a non-nil
// interface (the operator plane reports Unimplemented for a nil seam).
func pythonPackageAdminOrNil(a *pypackages.Admin) operator.PythonPackageAdmin {
	if a == nil {
		return nil
	}
	return a
}

// pythonGCDeps is what the env's GC reads of the running kernel.
type pythonGCDeps struct {
	// inUse lists the base generations and layers live instances run on
	// (agentmgr.InstanceManager.PythonGenerationsInUse).
	inUse func() []string
	// agents lists the registered agents (a removed agent's layers go).
	agents domain.AgentLister
}

// wirePythonGC builds the env's garbage collector (ADR-0157 D7.12, P5) and the
// operator plane's package seam over it. GC runs once the env is first ready
// (at boot), after a background build switched the base generation, after an
// agent's layer changed and after an instance that ran on a recorded
// generation is gone — so a recycled instance's superseded layer is collected
// right after the recycle. Returns nils when the env is unavailable on this
// host or the package service is not built.
func wirePythonGC(pc config.AgentPythonConfig, env pythonEnvGate, svc *pypackages.Service, deps pythonGCDeps) (*pyenv.Collector, *pypackages.Admin) {
	b := env.Builder()
	if b == nil || svc == nil || deps.inUse == nil {
		return nil, nil
	}
	keep := func(ctx context.Context) pyenv.KeepSet {
		layers, release := svc.GCKeep()
		ks := pyenv.KeepSet{InUse: deps.inUse(), Layers: layers, Release: release}
		if deps.agents != nil {
			// A registry read error keeps every agent's layers (fail safe:
			// GC never deletes on a guess).
			if defs, err := deps.agents.GetAllAgents(ctx); err == nil {
				ks.KnownAgents = make(map[string]bool, len(defs))
				for _, d := range defs {
					ks.KnownAgents[d.ID] = true
				}
			}
		}
		return ks
	}
	gc, err := pyenv.NewCollector(b, pyenv.GCPolicy{KeepPrevious: pc.GCKeepPrevious(), PruneCache: pc.GCPruneCache()}, keep, env.ReadyC())
	if err != nil {
		slog.Error("ADR-0157 D7.12: sandbox Python env GC not wired", "err", err)
		return nil, nil
	}
	env.SetOnBuilt(gc.Trigger)
	admin, err := pypackages.NewAdmin(svc, b, gc)
	if err != nil {
		slog.Error("ADR-0157 D7.12: python package operator seam not wired", "err", err)
		return gc, nil
	}
	slog.Info("ADR-0157 D7.12: sandbox Python env GC wired", "keep_previous", pc.GCKeepPrevious(), "prune_cache", pc.GCPruneCache())
	return gc, admin
}
