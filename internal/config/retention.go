package config

import "fmt"

// RetentionConfig is execution.retention: how long the kernel keeps rows that
// nothing else ever prunes (ENG-128, owner decision 2026-11-15).
//
//   - Evidence and knowledge (evidence, events, entities/links, knowledge_items)
//     are kept: they are the record, and deletion goes through the erasure path
//     (T-20261103-build-a-principal-initiated-memory-erasure-path), never a TTL.
//   - Derived / cache tables get an opt-in TTL, default off.
//   - Operator audit is kept for a fixed window, 365 days by default.
//
// Every field is read at the composition root (app/retention.go) and drives
// internal/retention.Worker. 0 on a *Days field means "keep forever".
type RetentionConfig struct {
	// OperatorAuditDays prunes operator_audit rows older than this. Default 365;
	// 0 keeps them forever (e.g. a deployment whose regulator requires longer).
	OperatorAuditDays int `json:"operator_audit_days"`
	// ChunkTripletsDays prunes extracted KG triplets (chunk_triplets, derived from
	// chunks and re-extractable) older than this. Default 0 (off).
	ChunkTripletsDays int `json:"chunk_triplets_days"`
	// ExperiencesDays prunes episodic experience records (experiences, and through
	// the FK cascade their chunks — ADR-0095 D6, "forget an episode = one delete")
	// created before this. Default 0 (off). Turning it on forgets old episodes.
	ExperiencesDays int `json:"experiences_days"`
	// SweepIntervalSeconds is how often the retention worker runs. Default 3600;
	// 0 disables the worker entirely.
	SweepIntervalSeconds int `json:"sweep_interval_seconds"`
	// BatchSize bounds the rows one table loses per pass, so a large backlog drains
	// over several passes instead of one long-locking delete. Default 5000.
	BatchSize int `json:"batch_size"`
}

// DefaultRetentionConfig is the shipped retention policy.
func DefaultRetentionConfig() RetentionConfig {
	return RetentionConfig{
		OperatorAuditDays:    365,
		ChunkTripletsDays:    0,
		ExperiencesDays:      0,
		SweepIntervalSeconds: 3600,
		BatchSize:            5000,
	}
}

// validate checks execution.retention.
func (c RetentionConfig) validate() []string {
	var errs []string
	for _, f := range []struct {
		key string
		v   int
	}{
		{"operator_audit_days", c.OperatorAuditDays},
		{"chunk_triplets_days", c.ChunkTripletsDays},
		{"experiences_days", c.ExperiencesDays},
		{"sweep_interval_seconds", c.SweepIntervalSeconds},
		{"batch_size", c.BatchSize},
	} {
		if f.v < 0 {
			errs = append(errs, fmt.Sprintf("execution.retention.%s must be >= 0 (got %d)", f.key, f.v))
		}
	}
	return errs
}
