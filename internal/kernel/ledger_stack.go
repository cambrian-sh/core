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
	// Bodies is the artifact vault's byte store. Since ADR-0157 D5 the ledger is
	// the vault's only registry: every artifact (UploadArtifact, out/ collection,
	// tool-written files) is a kind=artifact entry naming its bytes here.
	Bodies postgres.LedgerBodyStore
	// Legacy is the retired pre-ADR-0157 bbolt artifact bucket (ADR-0157 Q6,
	// owner 2026-11-18: its records expire, they are not imported). nil ⇒ none.
	Legacy           LegacyArtifactSource
	Authorizer       domain.Authorizer
	PrivateByDefault bool
	// UntrustedOutput is the ENG-117 guard (execution.tools.output_scan) the
	// ledger tools rescan an MCP entry with before handing it back fenced
	// (ENG-2213). nil ⇒ fenced, not scanned.
	UntrustedOutput *domain.UntrustedOutputGuard
}

// LegacyArtifactSource is the retired pre-ADR-0157 artifact metadata store (the
// bbolt "artifacts" bucket). Nothing reads its records any more; the ledger only
// learns which vault bodies they named, to release them, and then drops it.
type LegacyArtifactSource interface {
	// LegacyArtifactHashes lists the vault hashes the bucket's records name, one
	// per record; present is false when the bucket is already gone.
	LegacyArtifactHashes() (hashes []string, present bool, err error)
	// DropLegacyArtifacts deletes the bucket. Absent is not an error.
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
	retireLegacyArtifacts(ctx, raw, deps.Legacy)
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
		Tools:   ledger.Tools(reader, content.Page(), deps.UntrustedOutput), // execution.content.page_bytes
		Sweeper: ledger.NewSweeper(raw, cfg.SweepInterval()),
	}
	slog.Info("ADR-0145: run ledger enabled", "retention", cfg.Retention(), "max_entry_bytes", cfg.EntryCap())
	return st
}

// legacyBodyReleaser is the part of the raw ledger store the retirement needs:
// the ledger's one body-release path (refcount on ledger_entries.body_hash, then
// vault delete).
type legacyBodyReleaser interface {
	ReleaseBodies(ctx context.Context, hashes []string) (deleted, failed int, err error)
}

// retireLegacyArtifacts retires the pre-ADR-0157 bbolt artifact bucket at boot.
// Owner decision 2026-11-18 (ADR-0157 Q6, ENG-1990): its records EXPIRE — they
// are not imported into the ledger (S4's import, ENG-1972, is removed). The
// records are dropped unread; only their keys, the vault hashes, are used, to
// release the bodies no ledger entry names: the vault has no retention of its
// own and every other delete reaches a body only through a ledger entry, so an
// unreleased body would stay forever, unreadable and unerasable. Bodies a ledger
// entry still names (the same bytes uploaded since) are kept. The bucket is
// dropped last, so a failed release leaves it for the next boot (a re-release is
// a no-op). The log carries counts only — no ids, no content.
//
// cambrian:destructive (bbolt + vault, not SQL): the bucket and the unreferenced
// bodies are deleted. An operator backup (`orchestrator backup run` copies the
// data dir) taken before the upgrade keeps both.
func retireLegacyArtifacts(ctx context.Context, raw legacyBodyReleaser, legacy LegacyArtifactSource) {
	if legacy == nil {
		return
	}
	hashes, present, err := legacy.LegacyArtifactHashes()
	if err != nil {
		slog.Error("ADR-0157 Q6: the retired bbolt artifact bucket could not be read; kept for the next boot", "err", err)
		return
	}
	if !present {
		return // already retired
	}
	released, failed, err := raw.ReleaseBodies(ctx, hashes)
	if err != nil {
		slog.Error("ADR-0157 Q6: vault bodies of the retired bbolt artifact records not released; the bucket is kept for the next boot",
			"records", len(hashes), "released", released, "failed", failed)
		return
	}
	if err := legacy.DropLegacyArtifacts(); err != nil {
		slog.Error("ADR-0157 Q6: the retired bbolt artifact bucket could not be dropped", "err", err)
		return
	}
	slog.Warn("ADR-0157 Q6: retired bbolt artifact records dropped without import (they expire)",
		"records_dropped", len(hashes), "vault_bodies_released", released)
}
