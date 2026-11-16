package config

import (
	"fmt"
	"time"
)

// ErasureConfig is execution.erasure: the ADR-0137 principal-initiated forget (D10;
// ENG-411 stage B). Every field is read at the composition root: Enabled, AgentErase
// and MaxTargetsPerRequest by kernel.NewMemoryStack (the enforcing eraser), the rest by
// app.go (the erasure store's deadline and the erasure worker). Zero values take the
// documented defaults through the methods below. PreviewTTLSeconds is read by app.go
// for the forget service (stage C).
type ErasureConfig struct {
	// Enabled composes the enforcing eraser (MemoryStack.Eraser) and starts the erasure
	// worker. Default false until the ADR-0137 gate passes; off, nothing can erase.
	// No omitempty: an explicit false must survive a marshal of the effective config.
	Enabled bool `json:"enabled"`
	// AgentEraseEnabled lets agent and person principals erase records they wrote
	// (writer-only within read scope). Default false: operator-only first (D10).
	AgentEraseEnabled bool `json:"agent_erase_enabled"`
	// MaxTargetsPerRequest bounds the ids one request may name. 0 ⇒ 1000.
	MaxTargetsPerRequest int `json:"max_targets_per_request,omitempty"`
	// AsyncDeadlineSeconds is how long a request's async tail (CAS deletes) may take
	// before it reads overdue. 0 ⇒ 3600.
	AsyncDeadlineSeconds int `json:"async_deadline_seconds,omitempty"`
	// TombstoneRetentionDays prunes per-unit tombstones older than this. 0 = forever
	// (the default): tombstones are content-free and ARE the no-resurrection fence, so a
	// retention shorter than the backup horizon lets a restore resurrect content.
	TombstoneRetentionDays int `json:"tombstone_retention_days,omitempty"`
	// WorkerPollSeconds is how often the erasure worker drains the outbox. 0 ⇒ 30.
	WorkerPollSeconds int `json:"worker_poll_seconds,omitempty"`
	// WorkerMaxAttempts bounds the retries of one async step; a step that fails this
	// many times stays pending and its request reads overdue. 0 ⇒ 5.
	WorkerMaxAttempts int `json:"worker_max_attempts,omitempty"`
	// PreviewTTLSeconds is how long a forget preview's token stays executable
	// (ADR-0137 D2; read by app.go for internal/memory/forget). 0 ⇒ 600.
	PreviewTTLSeconds int `json:"preview_ttl_seconds,omitempty"`
}

// PreviewTTL is PreviewTTLSeconds as a duration, with its default applied.
func (c ErasureConfig) PreviewTTL() time.Duration {
	if c.PreviewTTLSeconds > 0 {
		return time.Duration(c.PreviewTTLSeconds) * time.Second
	}
	return 10 * time.Minute
}

// MaxTargets is MaxTargetsPerRequest with its default applied.
func (c ErasureConfig) MaxTargets() int {
	if c.MaxTargetsPerRequest > 0 {
		return c.MaxTargetsPerRequest
	}
	return 1000
}

// AsyncDeadline is AsyncDeadlineSeconds as a duration, with its default applied.
func (c ErasureConfig) AsyncDeadline() time.Duration {
	if c.AsyncDeadlineSeconds > 0 {
		return time.Duration(c.AsyncDeadlineSeconds) * time.Second
	}
	return time.Hour
}

// TombstoneRetention is TombstoneRetentionDays as a duration; 0 = keep forever.
func (c ErasureConfig) TombstoneRetention() time.Duration {
	return time.Duration(c.TombstoneRetentionDays) * 24 * time.Hour
}

// WorkerPoll is WorkerPollSeconds as a duration, with its default applied.
func (c ErasureConfig) WorkerPoll() time.Duration {
	if c.WorkerPollSeconds > 0 {
		return time.Duration(c.WorkerPollSeconds) * time.Second
	}
	return 30 * time.Second
}

// WorkerAttempts is WorkerMaxAttempts with its default applied.
func (c ErasureConfig) WorkerAttempts() int {
	if c.WorkerMaxAttempts > 0 {
		return c.WorkerMaxAttempts
	}
	return 5
}

// validate checks execution.erasure.
func (c ErasureConfig) validate() []string {
	var errs []string
	for _, f := range []struct {
		name string
		v    int
	}{
		{"max_targets_per_request", c.MaxTargetsPerRequest},
		{"async_deadline_seconds", c.AsyncDeadlineSeconds},
		{"tombstone_retention_days", c.TombstoneRetentionDays},
		{"worker_poll_seconds", c.WorkerPollSeconds},
		{"worker_max_attempts", c.WorkerMaxAttempts},
		{"preview_ttl_seconds", c.PreviewTTLSeconds},
	} {
		if f.v < 0 {
			errs = append(errs, fmt.Sprintf("execution.erasure.%s must be >= 0 (got %d)", f.name, f.v))
		}
	}
	return errs
}
