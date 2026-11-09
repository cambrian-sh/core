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
	// Enabled turns the ledger on. Default false. On: every tool result and step
	// output is recorded, dependants read step results from the ledger, large tool
	// results are returned as ledger references, ledger_get/ledger_find are offered
	// to every agent, and the content store no longer holds step or tool results
	// (only worker scratch offloads). Off: the legacy plan-scoped content-store path.
	Enabled bool `json:"enabled,omitempty"`
	// RetentionHours is how long an entry lives (the conversation's lifetime, as
	// configured). 0 ⇒ 720 (30 days). Scratch entries go at turn end regardless.
	RetentionHours int `json:"retention_hours,omitempty"`
	// MaxEntryBytes caps the stored body of one entry; a larger result keeps its
	// first MaxEntryBytes (flagged truncated, with the original size and hash), its
	// preview and its provenance. 0 ⇒ 1 MiB.
	MaxEntryBytes int `json:"max_entry_bytes,omitempty"`
	// PreviewChars bounds an entry's preview. 0 ⇒ 300.
	PreviewChars int `json:"preview_chars,omitempty"`
	// PageBytes is the page size ledger_get serves a text body in. 0 ⇒ 16000.
	PageBytes int `json:"page_bytes,omitempty"`
	// SweepIntervalMinutes is how often expired entries are purged. 0 ⇒ 60.
	SweepIntervalMinutes int `json:"sweep_interval_minutes,omitempty"`
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

// Page is PageBytes with its default applied.
func (c LedgerConfig) Page() int {
	if c.PageBytes > 0 {
		return c.PageBytes
	}
	return 16000
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
		{"page_bytes", c.PageBytes},
		{"sweep_interval_minutes", c.SweepIntervalMinutes},
	} {
		if f.v < 0 {
			errs = append(errs, fmt.Sprintf("execution.ledger.%s must be >= 0 (got %d)", f.name, f.v))
		}
	}
	return errs
}
