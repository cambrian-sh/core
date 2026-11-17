package app

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/ledger"
	"github.com/cambrian-sh/core/internal/memory/vault"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// ledgerTurnEndTimeout bounds the scratch cleanup run when a chat turn ends. The
// turn has no request context left at that point (EndTurn runs on every exit,
// cancellation included), so the cleanup gets its own short deadline; it is
// best-effort — an undeleted scratch row still expires with its retention.
const ledgerTurnEndTimeout = 5 * time.Second

// wireLedger builds the ADR-0145 D1 run ledger (execution.ledger) and threads it to
// every consumer in one place: the ToolExecutor (the tool-result choke point and
// the kernel-served ledger_get / ledger_find tools), the server (the DAG executor's
// step-output choke point and dependants' reads, GetContextNode for led_… ids,
// the structured Execute result, the artifact plane: UploadArtifact / GetArtifact /
// ListStepArtifacts and the agent out/ collection, ADR-0157 D5) and the chat lane's
// turn end (scratch cleanup). legacy is the retired bbolt artifact bucket, imported
// into the ledger once (ADR-0157 Q6). Returns the sweeper for the kernel to start
// and stop, nil when the ledger is off.
func wireLedger(ctx context.Context, cfg config.LedgerConfig, content config.ContentConfig, pool *pgxpool.Pool, artifactVault *vault.ArtifactVault,
	legacy kernel.LegacyArtifactSource, authorizer domain.Authorizer, privateByDefault bool,
	server *subnetwork.Server, delegations *subnetwork.TurnDelegations, bus domain.EventBus) *ledger.Sweeper {
	deps := kernel.LedgerStackDeps{
		Pool:             pool,
		Authorizer:       authorizer,
		PrivateByDefault: privateByDefault,
		Legacy:           legacy,
	}
	if artifactVault != nil {
		deps.Bodies = artifactVault
	}
	st := kernel.NewLedgerStack(ctx, cfg, content, deps)
	if st == nil {
		return nil
	}
	server.Ledger = &subnetwork.LedgerAccess{Writer: st.Writer, Reader: st.Reader}
	// ADR-0157 D5.1: an exited sandboxed instance's out/ is recorded through the
	// server (lease provenance, classified writer) before its scratch goes.
	if server.Manager != nil && server.Manager.InstanceManager != nil {
		server.Manager.InstanceManager.SetOutputCollector(server)
	}
	if server.ToolExecutor != nil {
		// ENG-715: an external MCP tool result recorded here is also published as a
		// domain.ToolEvidenceRecordedEvent (digest + ledger id), so a provenance lane
		// can prove what evidence an answer used. A nil bus leaves the writer as is.
		server.ToolExecutor.Ledger = ledger.NewEvidenceNotifier(st.Writer, bus)
		server.ToolExecutor.KernelTools = append(server.ToolExecutor.KernelTools, st.Tools...)
	}
	sweeper := st.Sweeper
	delegations.SetTurnEndHook(func(_, originMessageID string) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ledgerTurnEndTimeout)
		defer cancel()
		sweeper.EndTurn(cctx, originMessageID)
	})
	return sweeper
}
