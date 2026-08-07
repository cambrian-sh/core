package config

import (
	stdjson "encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
)

// SchemaV1Flat is the original config schema: one flat `execution` block holding
// all 198 tuning fields.
//
// SchemaV2Nested groups those fields into named blocks (`execution.retrieval`,
// `execution.routing`, …). The fields, their types and their leaf json names are
// unchanged — only the nesting is new — so a v1 file is mechanically convertible
// and this package converts it on load rather than making operators migrate by hand.
const (
	SchemaV1Flat   = 1
	SchemaV2Nested = 2

	// CurrentSchemaVersion is what this build writes and expects.
	CurrentSchemaVersion = SchemaV2Nested
)

// migrateExecutionBlock rewrites a v1 flat `execution` map into v2 nested form,
// in place. It returns the flat keys it moved, in sorted order, so the caller can
// report exactly what changed; an empty result means the block was already v2
// (or absent) and nothing was touched.
//
// # Why migrate rather than require operators to edit their files
//
// The flat→nested split is a pure regrouping: every leaf key keeps its name, its
// type and its meaning. A config that cannot be loaded because of a change that
// carries no new information is a cost with no benefit, and the failure mode is
// the worst kind — a kernel that will not start after an upgrade, at the moment
// nobody wants to be reading a schema diff.
//
// # Why unknown keys are preserved rather than dropped
//
// A key this build does not recognise is far more likely to be a field from a
// NEWER build (a rollback, a mixed fleet) than a typo. Dropping it would silently
// discard an operator's setting; leaving it in place means the worst case is that
// it is ignored by the struct decode, which is what would have happened anyway.
func migrateExecutionBlock(exec map[string]any) []string {
	var moved []string
	for key, val := range exec {
		block, ok := legacyExecutionKey[key]
		if !ok {
			// Already-nested blocks (they map to themselves in no direction),
			// `graph`, and anything unrecognised: leave alone.
			continue
		}
		dst, _ := exec[block].(map[string]any)
		if dst == nil {
			dst = map[string]any{}
			exec[block] = dst
		}
		// A value already present in the nested block WINS. A file that carries
		// both forms is mid-migration, and the explicitly-nested one is the more
		// recent statement of intent.
		if _, exists := dst[key]; !exists {
			dst[key] = val
		}
		delete(exec, key)
		moved = append(moved, key)
	}
	sort.Strings(moved)
	return moved
}

// migrateConfigMap applies migrateExecutionBlock to a decoded config document and
// stamps schema_version. Returns the keys moved.
func migrateConfigMap(doc map[string]any) []string {
	exec, ok := doc["execution"].(map[string]any)
	if !ok {
		return nil
	}
	moved := migrateExecutionBlock(exec)
	if len(moved) > 0 {
		doc["schema_version"] = CurrentSchemaVersion
	}
	return moved
}

// migrateJSON converts one config document's bytes from v1 to v2 when needed.
//
// It returns the original bytes unchanged when there is nothing to do, so a v2
// file is never rewritten, reformatted, or reordered on the way through — the
// bytes koanf parses are byte-identical to the bytes on disk.
func migrateJSON(b []byte, origin string) []byte {
	if len(b) == 0 {
		return b
	}
	var doc map[string]any
	if err := stdjson.Unmarshal(b, &doc); err != nil {
		// Not our problem to diagnose: koanf's own parser will report the syntax
		// error against the real file with a real position. Passing the bytes
		// through unchanged keeps that error message intact.
		return b
	}
	moved := migrateConfigMap(doc)
	if len(moved) == 0 {
		return b
	}
	out, err := stdjson.Marshal(doc)
	if err != nil {
		return b
	}
	slog.Info("config: migrated legacy flat execution block to nested schema",
		slog.String("origin", origin),
		slog.Int("from_schema", SchemaV1Flat),
		slog.Int("to_schema", SchemaV2Nested),
		slog.Int("keys_moved", len(moved)),
		slog.String("example", moved[0]))
	return out
}

// migrateFile reads a config file and returns its migrated bytes. A missing file
// yields nil, which the caller treats as "skip this layer" exactly as before.
func migrateFile(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return migrateJSON(b, path)
}

// MigrateFileInPlace rewrites a v1 config file on disk to v2, returning the keys
// it moved. It is NOT called during load — loading migrates in memory and leaves
// the operator's file alone. This exists for an explicit, opt-in
// `cambrian config migrate`, where rewriting the file is the point.
//
// A file that is already v2 is left untouched and reports no moves, so running it
// twice is safe.
func MigrateFileInPlace(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config migrate: read %s: %w", path, err)
	}
	var doc map[string]any
	if err := stdjson.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("config migrate: parse %s: %w", path, err)
	}
	moved := migrateConfigMap(doc)
	if len(moved) == 0 {
		return nil, nil
	}
	out, err := stdjson.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("config migrate: encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("config migrate: write %s: %w", path, err)
	}
	return moved, nil
}

// CanonicalKey maps a PRE-v2 flat key ("execution.ewma_alpha") to its v2 nested
// path ("execution.supervision.ewma_alpha"). A key that is not a legacy flat
// execution key — including one already nested — is returned unchanged.
//
// The flat spelling is not merely historical: it is the operator contract. The
// console's tunable catalogue, SetConfig/DeleteConfig requests and the durable
// store all speak flat keys, while every migrated layer claims provenance under
// the nested path. This is the one translation point between the two, so that
// "the old name keeps working" holds for keys exactly as it does for env vars.
func CanonicalKey(key string) string {
	if rest, ok := strings.CutPrefix(key, "execution."); ok {
		if block, known := legacyExecutionKey[rest]; known {
			return "execution." + block + "." + rest
		}
	}
	return key
}

// legacyExecutionKey maps a PRE-v2 flat `execution.<key>` name to the nested
// block that now owns it. Generated from the group structs; every field appears
// exactly once.
var legacyExecutionKey = map[string]string{
	"activation_threshold":                     "memory",
	"agent_env_passthrough":                    "agents",
	"agent_memory_limit_mb":                    "agents",
	"agent_pinning":                            "routing",
	"agentic_decompose_enabled":                "retrieval",
	"agentic_ircot_enabled":                    "retrieval",
	"agentic_max_hops":                         "retrieval",
	"agentic_planner_model":                    "retrieval",
	"agentic_retrieval_enabled":                "retrieval",
	"anchor_constraint_enabled":                "retrieval",
	"bid_calibration_min_samples":              "routing",
	"bypass_selection":                         "routing",
	"blend_enabled":                            "retrieval",
	"blend_weight_activation":                  "retrieval",
	"blend_weight_coherence":                   "retrieval",
	"blend_weight_confidence":                  "retrieval",
	"blend_weight_cosine":                      "retrieval",
	"blend_weight_lexical":                     "retrieval",
	"blend_weight_pagerank":                    "retrieval",
	"blend_weight_recency":                     "retrieval",
	"budget_exhaustion_alarm_rate":             "plan",
	"calibrated_bids":                          "routing",
	"canonical_vocab":                          "capability",
	"capability_aliases":                       "capability",
	"capability_cluster_epsilon":               "capability",
	"capability_cluster_interval_seconds":      "capability",
	"capability_cluster_min_agents":            "capability",
	"capability_cluster_threshold":             "capability",
	"capability_contract":                      "routing",
	"capability_resolution":                    "capability",
	"capture_llm_exchanges":                    "llm",
	"chat_pool_acquire_timeout_seconds":        "chat",
	"chat_pool_agent_id":                       "chat",
	"chat_pool_queue_size":                     "chat",
	"chat_pool_size":                           "chat",
	"circadian_stale_doc_warn_threshold":       "memory",
	"classification_vocabulary":                "router",
	"cold_start_penalty_multiplier":            "gatekeeper",
	"consolidation_threshold_bytes":            "memory",
	"consolidation_threshold_doc_count":        "memory",
	"context_growth_k":                         "plan",
	"context_ref_snippet_chars":                "plan",
	"cross_verify_rate":                        "verification",
	"daemon_restart_base_backoff_ms":           "agents",
	"daemon_restart_max_attempts":              "agents",
	"daemon_restart_max_backoff_ms":            "agents",
	"daemon_restart_window_seconds":            "agents",
	"disable_interviews":                       "agents",
	"discovery_safe_tools":                     "tools",
	"dispatch_cheap_energy_max":                "routing",
	"dispatch_merit_floor":                     "routing",
	"edge_extraction_batch_size":               "ingestion",
	"edge_extraction_llm_timeout_ms":           "ingestion",
	"edge_extraction_max_idle_ms":              "ingestion",
	"edge_extraction_queue_size":               "ingestion",
	"ewma_alpha":                               "supervision",
	"experience_records_enabled":               "memory",
	"experience_surprise_floor":                "memory",
	"experiential_memory_enabled":              "memory",
	"exploration_rate":                         "routing",
	"fallback_confidence_threshold":            "plan",
	"fallback_enabled":                         "plan",
	"gatekeeper_max_candidates":                "gatekeeper",
	"gatekeeper_w1":                            "gatekeeper",
	"gatekeeper_w2":                            "gatekeeper",
	"gatekeeper_w3":                            "gatekeeper",
	"gatekeeper_w4":                            "gatekeeper",
	"hebbian_base_weight":                      "retrieval",
	"hebbian_coactivation_floor":               "retrieval",
	"hebbian_decay_per_day":                    "retrieval",
	"hebbian_enabled":                          "retrieval",
	"hebbian_learning_rate":                    "retrieval",
	"hebbian_max_weight":                       "retrieval",
	"hebbian_top_n":                            "retrieval",
	"hippocampus_default_policy":               "hippocampus",
	"hippocampus_policies":                     "hippocampus",
	"histogram_alpha":                          "supervision",
	"histogram_min_samples":                    "supervision",
	"hnsw_ef_search":                           "retrieval",
	"hybrid_lexical_weight":                    "retrieval",
	"hybrid_rrf_k":                             "retrieval",
	"hybrid_search_enabled":                    "retrieval",
	"inbox_dir":                                "ingestion",
	"ingest_token":                             "ingestion",
	"ingestion_batch_size":                     "ingestion",
	"ingestion_batch_wait_ms":                  "ingestion",
	"ingestion_http_port":                      "ingestion",
	"ingestion_queue_size":                     "ingestion",
	"ingestion_workers":                        "ingestion",
	"k_anonymity_floor":                        "router",
	"kg2rag_enabled":                           "retrieval",
	"kg2rag_max_entities":                      "retrieval",
	"kg2rag_max_expanded":                      "retrieval",
	"kg2rag_max_hops":                          "retrieval",
	"kg2rag_per_entity":                        "retrieval",
	"kg_extractor_enabled":                     "ingestion",
	"latency_window_size":                      "supervision",
	"learned_scorer":                           "routing",
	"learned_scorer_model_path":                "routing",
	"llm_gateway_max_concurrency":              "llm",
	"llm_gateway_retry_backoff_ms":             "llm",
	"max_context_slots":                        "plan",
	"max_fanout_width":                         "plan",
	"max_memory_results":                       "memory",
	"max_neighbor_expansion":                   "memory",
	"max_orphaned_documents":                   "memory",
	"max_partial_context_bytes":                "plan",
	"max_plan_cost":                            "plan",
	"max_recursion_depth":                      "plan",
	"max_replan_attempts":                      "plan",
	"max_session_tokens":                       "session",
	"max_step_energy":                          "plan",
	"memory_relevance_threshold":               "memory",
	"min_auction_confidence":                   "gatekeeper",
	"min_gc_age_days":                          "memory",
	"min_step_energy":                          "plan",
	"min_verified_events":                      "verification",
	"neighbor_window_enabled":                  "retrieval",
	"per_capability_merit":                     "routing",
	"plan_drift_days":                          "plan",
	"plan_preview_only":                        "plan",
	"plan_timeout_ms":                          "plan",
	"procedure_deprecate_below":                "procedure",
	"procedure_induction_interval_hours":       "procedure",
	"procedure_max_episodes_per_pass":          "procedure",
	"procedure_min_samples":                    "procedure",
	"profile_aggregator_interval_seconds":      "supervision",
	"proposal_timeout_ms":                      "routing",
	"provisional_exploration_budget":           "routing",
	"provisional_exploration_window_seconds":   "routing",
	"query_entity_seeding_enabled":             "retrieval",
	"recall_over_fetch":                        "retrieval",
	"recall_similarity_floor":                  "retrieval",
	"recall_spreading_enabled":                 "retrieval",
	"recall_top_k":                             "retrieval",
	"remember_default_activation":              "memory",
	"require_explicit_session":                 "session",
	"reranker_enabled":                         "retrieval",
	"reranker_top_k":                           "retrieval",
	"reranker_weight":                          "retrieval",
	"retrieval_floor":                          "retrieval",
	"router_classification_body_chars":         "router",
	"router_min_classification_confidence":     "router",
	"routing_trace_enabled":                    "routing",
	"scene_gen_on_ingest_enabled":              "ingestion",
	"session_idle_sweep_interval_seconds":      "session",
	"session_idle_timeout_minutes":             "session",
	"session_retention_days":                   "session",
	"session_retention_sweep_interval_seconds": "session",
	"session_token_sweep_interval_seconds":     "session",
	"session_token_ttl_multiplier":             "session",
	"session_ttl_days":                         "session",
	"signal_noise_threshold":                   "supervision",
	"signal_noise_window_secs":                 "supervision",
	"single_agent_id":                          "routing",
	"step_cache_policies":                      "plan",
	"step_timeout_base_buffer_ms":              "plan",
	"step_timeout_multiplier":                  "plan",
	"structure_graph_enabled":                  "retrieval",
	"tier1_channel_capacity":                   "memory",
	"tier2_batch_size":                         "memory",
	"tier2_llm_timeout":                        "memory",
	"tier2_max_idle_seconds":                   "memory",
	"tool_effects_strict":                      "tools",
	"tool_retrieval_floor":                     "tools",
	"tools_auto_approve":                       "tools",
	"tools_unrestricted":                       "tools",
	"trust_boost_threshold":                    "verification",
	"trust_score_abs_weight":                   "verification",
	"trust_score_cal_weight":                   "verification",
	"use_global_workspace":                     "workspace",
	"verification_queue_capacity":              "verification",
	"verifier_pool_min_size":                   "verification",
	"verifier_pool_threshold":                  "verification",
	"verifier_pool_threshold_floor":            "verification",
	"verifier_pool_threshold_step":             "verification",
	"verifier_recency_window":                  "verification",
	"workspace_drift_threshold":                "workspace",
	"workspace_enable_drift_guard":             "workspace",
	"workspace_execution_slots":                "workspace",
	"workspace_lru_cache_capacity":             "workspace",
	"workspace_min_fact_cosine":                "workspace",
	"workspace_planning_slots":                 "workspace",
}
