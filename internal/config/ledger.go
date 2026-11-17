package config

import (
	"fmt"
	"time"
)

// LedgerConfig is execution.ledger: the run ledger (ADR-0145 D1). Every field is
// read at the composition root (app.wireLedger → kernel.NewLedgerStack, the
// ToolExecutor and the server's DAG executor); zero values take the documented
// defaults through the methods below, so an existing tuning.json needs no new keys.
type LedgerConfig struct {
	// Enabled turns the ledger on. Default true (owner 2026-11-13, ADR-0150: the
	// composed runtime, run_subtasks and the chat transcript summary record into
	// it; the tables come from the embedded migrations 0034-0036/0044). On: every tool result and step
	// output is recorded, dependants read step results from the ledger, large tool
	// results are returned as ledger references, ledger_get/ledger_find are offered
	// to every agent, and the content store no longer holds step or tool results
	// (only worker scratch offloads). Off: the legacy plan-scoped content-store path.
	// No omitempty: an explicit false must survive a marshal of the effective config.
	Enabled bool `json:"enabled"`
	// RetentionHours is how long an entry lives (the conversation's lifetime, as
	// configured). 0 ⇒ 720 (30 days). Scratch entries go at turn end regardless.
	RetentionHours int `json:"retention_hours,omitempty"`
	// MaxEntryBytes caps the stored body of one entry; a larger result keeps its
	// first MaxEntryBytes (flagged truncated, with the original size and hash), its
	// preview and its provenance. 0 ⇒ 1 MiB.
	MaxEntryBytes int `json:"max_entry_bytes,omitempty"`
	// PreviewChars bounds an entry's preview. 0 ⇒ 300.
	PreviewChars int `json:"preview_chars,omitempty"`
	// ledger_get's page size is execution.content.page_bytes (content.go); an
	// explicit execution.ledger.page_bytes is migrated there on load.
	// SweepIntervalMinutes is how often expired entries are purged. 0 ⇒ 60.
	SweepIntervalMinutes int `json:"sweep_interval_minutes,omitempty"`
	// Artifacts are the ADR-0157 D5 caps on files that leave an agent through the
	// kernel (UploadArtifact, the scratch out/ collection, tool-written files).
	Artifacts LedgerArtifactsConfig `json:"artifacts,omitzero"`
}

// LedgerArtifactsConfig is execution.ledger.artifacts (ADR-0157 D5.2). An
// artifact entry is never truncated: a file over MaxFileBytes is refused by name.
// Zero values take the defaults below, so no existing file needs new keys.
type LedgerArtifactsConfig struct {
	// MaxFileBytes caps one artifact. 0 ⇒ 100 MiB, the vault's hard ceiling
	// (Q5: no streaming; a larger value is clamped to that ceiling).
	MaxFileBytes int64 `json:"max_file_bytes,omitempty"`
	// MaxCollectBytes caps what one instance's out/ collection records in total;
	// files past it are left uncollected and named in the log. 0 ⇒ 256 MiB.
	MaxCollectBytes int64 `json:"max_collect_bytes,omitempty"`
	// MaxCollectFiles caps how many out/ files one collection records. 0 ⇒ 64.
	MaxCollectFiles int `json:"max_collect_files,omitempty"`
}

// ArtifactVaultCeiling is the artifact vault's per-file hard limit
// (vault.NewArtifactVault); no artifact cap may exceed it.
const ArtifactVaultCeiling int64 = 100 << 20

// FileCap is MaxFileBytes with its default applied, never above the vault ceiling.
func (c LedgerArtifactsConfig) FileCap() int64 {
	if c.MaxFileBytes > 0 && c.MaxFileBytes < ArtifactVaultCeiling {
		return c.MaxFileBytes
	}
	return ArtifactVaultCeiling
}

// CollectBytesCap is MaxCollectBytes with its default applied.
func (c LedgerArtifactsConfig) CollectBytesCap() int64 {
	if c.MaxCollectBytes > 0 {
		return c.MaxCollectBytes
	}
	return 256 << 20
}

// CollectFilesCap is MaxCollectFiles with its default applied.
func (c LedgerArtifactsConfig) CollectFilesCap() int {
	if c.MaxCollectFiles > 0 {
		return c.MaxCollectFiles
	}
	return 64
}

// Retention is RetentionHours as a duration, with its default applied.
func (c LedgerConfig) Retention() time.Duration {
	if c.RetentionHours > 0 {
		return time.Duration(c.RetentionHours) * time.Hour
	}
	return 720 * time.Hour
}

// EntryCap is MaxEntryBytes with its default applied.
func (c LedgerConfig) EntryCap() int {
	if c.MaxEntryBytes > 0 {
		return c.MaxEntryBytes
	}
	return 1 << 20
}

// Preview is PreviewChars with its default applied.
func (c LedgerConfig) Preview() int {
	if c.PreviewChars > 0 {
		return c.PreviewChars
	}
	return 300
}

// SweepInterval is SweepIntervalMinutes as a duration, with its default applied.
func (c LedgerConfig) SweepInterval() time.Duration {
	if c.SweepIntervalMinutes > 0 {
		return time.Duration(c.SweepIntervalMinutes) * time.Minute
	}
	return time.Hour
}

// validate checks execution.ledger.
func (c LedgerConfig) validate() []string {
	var errs []string
	for _, f := range []struct {
		name string
		v    int
	}{
		{"retention_hours", c.RetentionHours},
		{"max_entry_bytes", c.MaxEntryBytes},
		{"preview_chars", c.PreviewChars},
		{"sweep_interval_minutes", c.SweepIntervalMinutes},
		{"artifacts.max_collect_files", c.Artifacts.MaxCollectFiles},
	} {
		if f.v < 0 {
			errs = append(errs, fmt.Sprintf("execution.ledger.%s must be >= 0 (got %d)", f.name, f.v))
		}
	}
	if c.Artifacts.MaxFileBytes < 0 || c.Artifacts.MaxCollectBytes < 0 {
		errs = append(errs, "execution.ledger.artifacts.max_file_bytes and max_collect_bytes must be >= 0")
	}
	if c.Artifacts.MaxFileBytes > ArtifactVaultCeiling {
		errs = append(errs, fmt.Sprintf("execution.ledger.artifacts.max_file_bytes must be <= %d (the artifact vault ceiling; ADR-0157 Q5: no streaming)", ArtifactVaultCeiling))
	}
	return errs
}
