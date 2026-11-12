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
// the structured Execute result) and the chat lane's turn end (scratch cleanup).
// Returns the sweeper for the kernel to start and stop, nil when the ledger is off.
func wireLedger(ctx context.Context, cfg config.LedgerConfig, content config.ContentConfig, pool *pgxpool.Pool, artifactVault *vault.ArtifactVault,
	artifacts subnetwork.ArtifactMetaStore, authorizer domain.Authorizer, privateByDefault bool,
	server *subnetwork.Server, delegations *subnetwork.TurnDelegations) *ledger.Sweeper {
	deps := kernel.LedgerStackDeps{
		Pool:             pool,
		Authorizer:       authorizer,
		PrivateByDefault: privateByDefault,
	}
	if artifactVault != nil {
		deps.Bodies = artifactVault
	}
	if artifacts != nil {
		deps.ArtifactReferenced = func(hash string) bool {
			a, err := artifacts.GetArtifact(hash)
			// An error is "cannot tell": keep the bytes.
			return err != nil || a != nil
		}
	}
	st := kernel.NewLedgerStack(ctx, cfg, content, deps)
	if st == nil {
		return nil
	}
	server.Ledger = &subnetwork.LedgerAccess{Writer: st.Writer, Reader: st.Reader}
	if server.ToolExecutor != nil {
		server.ToolExecutor.Ledger = st.Writer
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
