package kernel

import (
	"context"
	"log/slog"
	"time"

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
	// Bodies is the artifact vault's byte store. Since ADR-0157 D5 the ledger is
	// the vault's only registry: every artifact (UploadArtifact, out/ collection,
	// tool-written files) is a kind=artifact entry naming its bytes here.
	Bodies postgres.LedgerBodyStore
	// Legacy is the retired bbolt artifact bucket (ADR-0157 Q6): its records are
	// imported as artifact entries once, then the bucket is dropped. nil ⇒ none.
	Legacy           LegacyArtifactSource
	Authorizer       domain.Authorizer
	PrivateByDefault bool
}

// LegacyArtifactSource is the retired pre-ADR-0157 artifact metadata store (the
// bbolt "artifacts" bucket), read once for the Q6 import and then dropped.
type LegacyArtifactSource interface {
	// LegacyArtifacts lists every record still in the bucket (nil when it is gone).
	LegacyArtifacts() ([]postgres.LegacyArtifact, error)
	// DropLegacyArtifacts deletes the bucket. Called only after every record was
	// imported into the ledger.
	DropLegacyArtifacts() error
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
	if !raw.Ready(ctx) {
		slog.Error("ADR-0145: execution.ledger.enabled is set but the ledger_entries table or its 0035/0053 columns are missing (run `orchestrator migrate up`); the ledger is OFF")
		return nil
	}
	importLegacyArtifacts(ctx, raw, deps.Legacy, cfg.Retention())
	reader := authz.NewEnforcingLedger(raw, nil)
	st := &LedgerStack{
		Writer: ledger.NewRecorder(raw, deps.Authorizer, ledger.RecorderConfig{
			Retention:        cfg.Retention(),
			MaxEntryBytes:    cfg.EntryCap(),
			MaxArtifactBytes: cfg.Artifacts.FileCap(),
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

// importLegacyArtifacts is the ADR-0157 Q6 one-time move of the retired bbolt
// artifact records into the ledger (owner-approved answer: import them with their
// tags rather than let them become unreachable). Each record becomes an artifact
// entry with its own tags, session and step over the vault bytes it already
// names; the bucket is dropped only after every record was written, so a failure
// leaves it for the next boot (the import is idempotent: deterministic ids,
// ON CONFLICT DO NOTHING).
//
// cambrian:destructive (bbolt, not SQL): DropLegacyArtifacts deletes the bucket
// after the import; its content lives on in ledger_entries. An operator backup
// (`orchestrator backup run` copies the data dir) taken before the upgrade keeps
// the original bucket.
func importLegacyArtifacts(ctx context.Context, raw *postgres.PgLedgerStore, legacy LegacyArtifactSource, retention time.Duration) {
	if legacy == nil {
		return
	}
	recs, err := legacy.LegacyArtifacts()
	if err != nil {
		slog.Error("ADR-0157 Q6: the legacy bbolt artifact records could not be read; kept for the next boot", "err", err)
		return
	}
	if recs == nil {
		return // bucket already retired
	}
	n, err := raw.ImportLegacyArtifacts(ctx, recs, time.Now(), retention)
	if err != nil {
		slog.Error("ADR-0157 Q6: legacy artifact import stopped; the bbolt bucket is kept and the import retried next boot",
			"imported", n, "of", len(recs), "err", err)
		return
	}
	if err := legacy.DropLegacyArtifacts(); err != nil {
		slog.Error("ADR-0157 Q6: legacy artifacts imported but the bbolt bucket could not be dropped", "err", err)
		return
	}
	slog.Warn("ADR-0157 Q6: legacy bbolt artifact records imported into the run ledger and the bucket retired",
		"records", len(recs), "imported", n)
}
