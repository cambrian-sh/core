package kernel

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/ledger"
)

// LedgerStack is the run ledger (ADR-0145 D1), assembled in ONE place exactly like
// MemoryStack assembles memory (AGENTS.md §2.1):
//
//	raw store (*postgres.PgLedgerStore: Postgres metadata + artifact-vault bytes) — NO enforcement
//	 ├─ Writer  = ledger.NewRecorder(raw, authorizer)  — kernel classification at write
//	 └─ Reader  = authz.NewEnforcingLedger(raw)        — fail-closed scope on every read
//	      └─ Tools = ledger_get / ledger_find over Reader
//
// The raw store is not a field: the only things that leave this constructor are
// the classified writer (for the two choke points), the enforced reader (for every
// reader), the tools over the reader, and the sweeper.
type LedgerStack struct {
	Writer  domain.LedgerWriter
	Reader  domain.LedgerReader
	Tools   []domain.KernelTool
	Sweeper *ledger.Sweeper
}

// LedgerStackDeps are the kernel resources the ledger is built over.
type LedgerStackDeps struct {
	Pool *pgxpool.Pool
	// Bodies is the artifact vault's byte store, the same instance the artifact
	// plane (UploadArtifact/GetArtifact) stores into.
	Bodies postgres.LedgerBodyStore
	// ArtifactReferenced reports whether an artifact record names a vault hash, so
	// the retention purge never deletes bytes an artifact still uses.
	ArtifactReferenced func(hash string) bool
	Authorizer         domain.Authorizer
	PrivateByDefault   bool
}

// NewLedgerStack builds the ledger when execution.ledger.enabled is set and its
// table exists. It returns nil (the legacy content-store path) when the flag is
// off, and nil with an ERROR log when the flag is on but the ledger cannot be
// built — the operator asked for it, so its absence must be loud.
func NewLedgerStack(ctx context.Context, cfg config.LedgerConfig, content config.ContentConfig, deps LedgerStackDeps) *LedgerStack {
	if !cfg.Enabled {
		return nil
	}
	if deps.Pool == nil || deps.Bodies == nil {
		slog.Error("ADR-0145: execution.ledger.enabled is set but Postgres or the artifact vault is missing; the ledger is OFF")
		return nil
	}
	raw := postgres.NewPgLedgerStore(deps.Pool, deps.Bodies)
	raw.ArtifactReferenced = deps.ArtifactReferenced
	if !raw.Ready(ctx) {
		slog.Error("ADR-0145: execution.ledger.enabled is set but the ledger_entries table or its 0035 source_tags column is missing (run `orchestrator migrate up`); the ledger is OFF")
		return nil
	}
	reader := authz.NewEnforcingLedger(raw, nil)
	st := &LedgerStack{
		Writer: ledger.NewRecorder(raw, deps.Authorizer, ledger.RecorderConfig{
			Retention:        cfg.Retention(),
			MaxEntryBytes:    cfg.EntryCap(),
			PreviewChars:     cfg.Preview(),
			PrivateByDefault: deps.PrivateByDefault,
		}),
		Reader:  reader,
		Tools:   ledger.Tools(reader, content.Page()), // execution.content.page_bytes
		Sweeper: ledger.NewSweeper(raw, cfg.SweepInterval()),
	}
	slog.Info("ADR-0145: run ledger enabled", "retention", cfg.Retention(), "max_entry_bytes", cfg.EntryCap())
	return st
}
