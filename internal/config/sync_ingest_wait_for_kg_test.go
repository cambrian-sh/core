package config

import "testing"

// T-20261102-async-kg-triplet-extraction-30-chunks-s-lags: read-your-writes for
// the KG lane is an option, default OFF with a 30 s bound (owner decision
// 2026-11-02), so product ingest latency is unchanged unless it is turned on.
func TestDefaultConfig_SyncIngestWaitForKGOff(t *testing.T) {
	ing := DefaultConfig().Execution.Ingestion
	if ing.SyncIngestWaitForKG {
		t.Fatal("sync_ingest_wait_for_kg must default false")
	}
	if ing.SyncIngestKGWaitTimeoutMs != 30000 {
		t.Fatalf("sync_ingest_kg_wait_timeout_ms default = %d, want 30000", ing.SyncIngestKGWaitTimeoutMs)
	}
}

// The keys live in the Ingestion block (AGENTS.md §3.4) and reach the fields
// from a file and from their CAMBRIAN_* env vars (the benchmark arm sets the env).
func TestLoadConfig_SyncIngestWaitForKGKeys(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"schema_version":2,"execution":{"ingestion":{"sync_ingest_wait_for_kg":true,"sync_ingest_kg_wait_timeout_ms":1234}}}`)
	cfg, _ := loadFrom(t, dir, nil)
	if !cfg.Execution.Ingestion.SyncIngestWaitForKG || cfg.Execution.Ingestion.SyncIngestKGWaitTimeoutMs != 1234 {
		t.Fatalf("file keys did not reach the fields: %+v", cfg.Execution.Ingestion)
	}

	dir = baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__INGESTION__SYNC_INGEST_WAIT_FOR_KG", "true")
	t.Setenv("CAMBRIAN_EXECUTION__INGESTION__SYNC_INGEST_KG_WAIT_TIMEOUT_MS", "4321")
	cfg, _ = loadFrom(t, dir, nil)
	if !cfg.Execution.Ingestion.SyncIngestWaitForKG {
		t.Fatal("CAMBRIAN_EXECUTION__INGESTION__SYNC_INGEST_WAIT_FOR_KG did not reach the field")
	}
	if cfg.Execution.Ingestion.SyncIngestKGWaitTimeoutMs != 4321 {
		t.Fatalf("CAMBRIAN_EXECUTION__INGESTION__SYNC_INGEST_KG_WAIT_TIMEOUT_MS = %d, want 4321", cfg.Execution.Ingestion.SyncIngestKGWaitTimeoutMs)
	}
}

// T-20261102-kg-extractor-cold-start-drops-the-first-kg: the kg_extractor retry
// settings default to 3 attempts, 2000 ms backoff and 1 re-queue, and reach the
// fields from a file and from their CAMBRIAN_* env vars, including an explicit 0.
func TestKgExtractorRetryKeys(t *testing.T) {
	ing := DefaultConfig().Execution.Ingestion
	if ing.KgExtractorCallAttempts != 3 || ing.KgExtractorRetryBackoffMs != 2000 || ing.KgTripletsMaxRequeues != 1 {
		t.Fatalf("defaults = %d/%d/%d, want 3/2000/1", ing.KgExtractorCallAttempts, ing.KgExtractorRetryBackoffMs, ing.KgTripletsMaxRequeues)
	}
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"schema_version":2,"execution":{"ingestion":{"kg_extractor_call_attempts":5,"kg_extractor_retry_backoff_ms":250,"kg_triplets_max_requeues":0}}}`)
	cfg, _ := loadFrom(t, dir, nil)
	got := cfg.Execution.Ingestion
	if got.KgExtractorCallAttempts != 5 || got.KgExtractorRetryBackoffMs != 250 || got.KgTripletsMaxRequeues != 0 {
		t.Fatalf("file keys did not reach the fields: %d/%d/%d", got.KgExtractorCallAttempts, got.KgExtractorRetryBackoffMs, got.KgTripletsMaxRequeues)
	}
	dir = baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__INGESTION__KG_EXTRACTOR_CALL_ATTEMPTS", "7")
	t.Setenv("CAMBRIAN_EXECUTION__INGESTION__KG_EXTRACTOR_RETRY_BACKOFF_MS", "10")
	t.Setenv("CAMBRIAN_EXECUTION__INGESTION__KG_TRIPLETS_MAX_REQUEUES", "4")
	cfg, _ = loadFrom(t, dir, nil)
	got = cfg.Execution.Ingestion
	if got.KgExtractorCallAttempts != 7 || got.KgExtractorRetryBackoffMs != 10 || got.KgTripletsMaxRequeues != 4 {
		t.Fatalf("env keys did not reach the fields: %d/%d/%d", got.KgExtractorCallAttempts, got.KgExtractorRetryBackoffMs, got.KgTripletsMaxRequeues)
	}
}

// execution.agents.agent_boot_timeout_ms defaults to 30000 and reaches the field
// from its env var (T-20261102-kg-extractor-cold-start-drops-the-first-kg).
func TestAgentBootTimeoutKey(t *testing.T) {
	if got := DefaultConfig().Execution.Agents.AgentBootTimeoutMs; got != 30000 {
		t.Fatalf("agent_boot_timeout_ms default = %d, want 30000", got)
	}
	dir := baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__AGENTS__AGENT_BOOT_TIMEOUT_MS", "45000")
	cfg, _ := loadFrom(t, dir, nil)
	if cfg.Execution.Agents.AgentBootTimeoutMs != 45000 {
		t.Fatalf("env did not reach the field: %d", cfg.Execution.Agents.AgentBootTimeoutMs)
	}
}
