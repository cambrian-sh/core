package config

import (
	stdjson "encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// updateLegacy regenerates testdata/execution_legacy_defaults.json:
//
//	go test ./internal/config/ -run TestExecutionLegacy -update-legacy
//
// Only do this deliberately. The file is the FROZEN pre-nesting schema — it is
// what every operator's existing config.json looks like, and the migration is
// judged against it. Regenerating it to make a test pass would be regenerating
// the definition of "correct".
var updateLegacy = flag.Bool("update-legacy", false, "rewrite the legacy execution-config snapshot")

const legacyGoldenPath = "testdata/execution_legacy_defaults.json"

// The legacy snapshot is the safety net for splitting the 198-field flat
// ExecutionConfig into named nested blocks.
//
// # Why a snapshot rather than assertions
//
// The refactor moves 198 fields and rewrites a 198-entry defaults literal. The
// failure mode that matters is not "it does not compile" — it is a single default
// silently changing or a field quietly disappearing during the move, which no
// compiler and no existing test would catch. This file pins every default VALUE
// against its flat key before the move, so the migration can be judged against
// the exact schema operators already have on disk.
//
// # What the keys are
//
// The FLAT keys, i.e. the pre-nesting `execution` block. After nesting, the
// migration maps each of these to its new path (e.g. `recall_top_k` →
// `retrieval.recall_top_k`), and TestExecutionLegacyConfigMigrates asserts that a
// config written against the old schema still produces exactly these values.

// flattenExecutionDefaults walks the struct by REFLECTION rather than marshalling
// it.
//
// Marshalling loses `omitempty` fields at their zero value — 58 of the 198 —
// and those are exactly the ones a careless move is most likely to drop, because
// nothing in the JSON would show their absence. Reflection pins the whole schema:
// every json key, present or zero.
func flattenExecutionDefaults(t *testing.T) map[string]any {
	t.Helper()
	def := DefaultConfig().Execution
	v := reflect.ValueOf(def)
	ty := v.Type()
	out := make(map[string]any, ty.NumField())
	for i := range ty.NumField() {
		f := ty.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		// Round-trip each value through JSON so the comparison is on the encoded
		// form (all numbers float64), matching what a config file actually holds.
		b, err := stdjson.Marshal(v.Field(i).Interface())
		if err != nil {
			t.Fatalf("marshal field %s: %v", f.Name, err)
		}
		var decoded any
		if err := stdjson.Unmarshal(b, &decoded); err != nil {
			t.Fatalf("unmarshal field %s: %v", f.Name, err)
		}
		out[name] = decoded
	}
	return out
}

// flattenNestedExecutionDefaults walks the v2 nested struct and returns the
// defaults keyed by their ORIGINAL flat name, so the v1 snapshot can be compared
// against them field for field.
func flattenNestedExecutionDefaults(t *testing.T) map[string]any {
	t.Helper()
	out := map[string]any{}
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		ty := v.Type()
		for i := range ty.NumField() {
			f := ty.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if name == "" {
				name = f.Name
			}
			fv := v.Field(i)
			// A group block is a struct whose json name is one of the nested block
			// names; recurse into it and keep using the LEAF key, which is what the
			// v1 snapshot holds.
			if fv.Kind() == reflect.Struct && isExecutionGroup(name) {
				walk(fv)
				continue
			}
			b, err := stdjson.Marshal(fv.Interface())
			if err != nil {
				t.Fatalf("marshal %s: %v", f.Name, err)
			}
			var decoded any
			if err := stdjson.Unmarshal(b, &decoded); err != nil {
				t.Fatalf("unmarshal %s: %v", f.Name, err)
			}
			out[name] = decoded
		}
	}
	walk(reflect.ValueOf(DefaultConfig().Execution))
	return out
}

// isExecutionGroup reports whether a json key names one of the v2 nested blocks.
// Derived from the migration table so the two can never disagree.
func isExecutionGroup(name string) bool {
	for _, block := range legacyExecutionKey {
		if block == name {
			return true
		}
	}
	return false
}

// TestExecutionDefaultsSurvivedNesting is the real point of the snapshot: every
// default that existed under the flat v1 schema must still exist, with the same
// value, after being moved into a nested block.
//
// It compares by LEAF key, so it is blind to which block a field ended up in and
// only fails when a value changed or a field vanished — which are the two things
// a 198-field move can silently get wrong.
// retiredExecutionFields are v1 fields that were DELIBERATELY removed after the
// nesting move, with the reason. They are exempt from the "was lost" check and
// nothing else — a field not listed here that vanishes is still a failure.
//
// The golden snapshot is deliberately NOT edited to drop them: it is the historical
// record of what v1 actually had, and rewriting history is the one thing that would
// make this guard meaningless.
var retiredExecutionFields = map[string]string{
	"hyde_enabled": "HyDE removed 2026-08-07: the hypothetical-passage lane was off by " +
		"default and prior benchmarking showed no recall gain.",

	// Scout retired 2026-08-07 (ADR pending): the whole pre-plan discovery organ —
	// deterministic probe registry, the opt-in LLM tier, and the scout_agent principal
	// with its discovery-safe tool ceiling.
	"disable_scout":            "Scout removed 2026-08-07.",
	"discovery_safe_tools":     "Scout removed 2026-08-07: the ceiling had no principal left to bind.",
	"scout_discovery_roots":    "Scout removed 2026-08-07.",
	"scout_enabled":            "Scout removed 2026-08-07.",
	"scout_http_allow_private": "Scout removed 2026-08-07.",
	"scout_http_probe_enabled": "Scout removed 2026-08-07.",
	"scout_llm_tier_enabled":   "Scout removed 2026-08-07.",
	"scout_model":              "Scout removed 2026-08-07.",
	"scout_scan_cap":           "Scout removed 2026-08-07.",

	// The auction and the EFE selector retired 2026-08-07: capability-typed
	// dispatch (ADR-0100) is now the only selection mechanism.
	"bid_round":             "Auction retired 2026-08-07; dispatch is unconditional.",
	"resource_selector":     "EFE selector retired 2026-08-07; it was never wired in any shipped config.",
	"efe_traffic_percent":   "EFE selector retired 2026-08-07.",
	"efe_exploration_bonus": "EFE selector retired 2026-08-07.",

	// Auction vocabulary retired 2026-08-07 with the mechanism itself.
	"auction_bid_timeout_ms": "Auction retired 2026-08-07; there is no bid round to time out.",
	"bypass_auction":         "Renamed bypass_selection 2026-08-07 — it now bypasses dispatch, not an auction.",

	// Bounded provisional exploration (ROUTE-06 / ADR-0069) retired 2026-08-08. The
	// budget bounded the provisional L2 bypass, and its ONLY recorder of wins was the
	// Auctioneer — deleted by ADR-0100 P3. From that moment `Allowed` always returned
	// true, so the bound was present, wired, validated and unable to bind. Removed
	// rather than re-wired: a bound that cannot bind is worse than no bound, because it
	// reads as a guarantee to whoever finds the key. The provisional bypass is now
	// unconditional in both arm positions — which is exactly what shipped all along.
	"provisional_exploration_budget": "Bounded provisional exploration retired 2026-08-08: " +
		"its only win-recorder was the Auctioneer (ADR-0100 P3), so the bound could never bind.",
	"provisional_exploration_window_seconds": "Bounded provisional exploration retired 2026-08-08: " +
		"sliding window for a budget that no longer exists.",

	// ROUTE-05 bid calibration (ADR-0068) retired 2026-08-07, superseded by ADR-0100.
	// Both keys configured the isotonic calibration of a BID; the only reader was
	// Auctioneer.Calibrator, deleted with the mechanism. Removed rather than left
	// declared-but-unread, because a key that silently does nothing is the trap that
	// `resource_selector: "efe"` already sprang once.
	"calibrated_bids":             "ROUTE-05 retired 2026-08-07 (ADR-0100 supersedes 0068): there is no bid to calibrate.",
	"bid_calibration_min_samples": "ROUTE-05 retired 2026-08-07: shrinkage threshold for a calibration curve nothing fits any more.",
}

// promotedExecutionDefaults are fields whose DEFAULT deliberately changed after
// the flat→nested move, so the golden v1 snapshot no longer states their value.
// Owner decision 2026-08-11: the operating config (the gitignored local tuning
// that had carried the measured-best retrieval stack, chat pool, and timeout
// values since the benchmark campaigns) was promoted wholesale into
// DefaultConfig(), so the product ships what was actually being run. The guard
// stays armed for every OTHER field: an unlisted change is still an accident.
var promotedExecutionDefaults = map[string]bool{
	"agentic_decompose_enabled":          true,
	"agentic_max_hops":                   true,
	"agentic_planner_model":              true,
	"agentic_retrieval_enabled":          true,
	"blend_enabled":                      true,
	"blend_weight_coherence":             true,
	"blend_weight_confidence":            true,
	"blend_weight_cosine":                true,
	"blend_weight_lexical":               true,
	"chat_pool_agent_id":                 true,
	"chat_pool_queue_size":               true,
	"chat_pool_size":                     true,
	"evidence_capture_enabled":           true,
	"hnsw_ef_search":                     true,
	"hybrid_lexical_weight":              true,
	"hybrid_rrf_k":                       true,
	"hybrid_search_enabled":              true,
	"kg_extractor_enabled":               true,
	"kg2rag_max_expanded":                true,
	"kg2rag_max_hops":                    true,
	"kg2rag_per_entity":                  true,
	"plan_timeout_ms":                    true,
	"procedure_induction_interval_hours": true,
	"query_entity_seeding_enabled":       true,
	"recall_over_fetch":                  true,
	"recall_similarity_floor":            true,
	"recall_top_k":                       true,
	"reranker_top_k":              true,
	"reranker_weight":             true,
	"step_timeout_base_buffer_ms": true,
	"drainer_enabled":             true,
	// trace_pipeline_payloads is deliberately absent: reviewed and REVERTED to
	// the off default on 2026-08-11 — see the field's doc comment.
}

func TestExecutionDefaultsSurvivedNesting(t *testing.T) {
	raw, err := os.ReadFile(legacyGoldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", legacyGoldenPath, err)
	}
	var want map[string]any
	if err := stdjson.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse snapshot: %v", err)
	}
	got := flattenNestedExecutionDefaults(t)

	var missing, changed []string
	for k, wantVal := range want {
		gotVal, ok := got[k]
		if !ok {
			if _, retired := retiredExecutionFields[k]; !retired {
				missing = append(missing, k)
			}
			continue
		}
		// promotedExecutionDefaults are DELIBERATE post-move changes (owner
		// 2026-08-11); everything else changing is still an accident this
		// guard exists to catch.
		if !jsonEqual(wantVal, gotVal) && !promotedExecutionDefaults[k] {
			changed = append(changed, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(changed)
	for _, k := range missing {
		t.Errorf("field %q was lost in the flat→nested move", k)
	}
	for _, k := range changed {
		t.Errorf("default for %q changed during the move: was %v, now %v", k, want[k], got[k])
	}
	if len(want) != len(got) {
		t.Logf("field count: v1 snapshot %d, v2 struct %d", len(want), len(got))
	}
}

// TestExecutionLegacyConfigMigrates is the operator-facing guarantee: a config
// written against the OLD flat schema still produces exactly the values it always
// did, without the operator editing anything.
func TestExecutionLegacyConfigMigrates(t *testing.T) {
	raw, err := os.ReadFile(legacyGoldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", legacyGoldenPath, err)
	}
	var flat map[string]any
	if err := stdjson.Unmarshal(raw, &flat); err != nil {
		t.Fatalf("parse snapshot: %v", err)
	}

	doc := map[string]any{"execution": flat}
	moved := migrateConfigMap(doc)
	if len(moved) == 0 {
		t.Fatal("migration moved nothing; a v1 document must be converted")
	}

	exec, _ := doc["execution"].(map[string]any)
	if exec == nil {
		t.Fatal("execution block disappeared")
	}
	// Every flat key must now live inside its block, and none may remain at the
	// top level of `execution` — a leftover is a key the struct will ignore.
	for k := range flat {
		block, ok := legacyExecutionKey[k]
		if !ok {
			continue // `graph` and friends stay put
		}
		if _, stillFlat := exec[k]; stillFlat {
			t.Errorf("key %q was left at the top level; it would be silently ignored", k)
		}
		nested, _ := exec[block].(map[string]any)
		if nested == nil {
			t.Errorf("block %q missing for key %q", block, k)
			continue
		}
		got, present := nested[k]
		if !present {
			t.Errorf("key %q did not arrive in block %q", k, block)
			continue
		}
		if !jsonEqual(flat[k], got) {
			t.Errorf("key %q changed value during migration: %v → %v", k, flat[k], got)
		}
	}
	if v, _ := doc["schema_version"].(int); v != CurrentSchemaVersion {
		t.Errorf("schema_version not stamped: got %v want %d", doc["schema_version"], CurrentSchemaVersion)
	}
}

// A v2 document must pass through untouched — migrating twice must not move
// anything, and must not rewrite bytes.
func TestExecutionMigrationIsIdempotent(t *testing.T) {
	doc := map[string]any{
		"execution": map[string]any{
			"retrieval": map[string]any{"recall_top_k": 25.0},
			"routing":   map[string]any{"bypass_auction": true},
		},
	}
	if moved := migrateConfigMap(doc); len(moved) != 0 {
		t.Errorf("a v2 document was modified: moved %v", moved)
	}

	v2 := []byte(`{"execution":{"retrieval":{"recall_top_k":25}}}`)
	if out := migrateJSON(v2, "test"); string(out) != string(v2) {
		t.Errorf("v2 bytes were rewritten:\n in: %s\nout: %s", v2, out)
	}
}

// An explicitly nested value wins over a flat one for the same key: a file
// mid-migration states the nested form deliberately.
func TestExecutionMigrationPrefersNestedOnConflict(t *testing.T) {
	doc := map[string]any{
		"execution": map[string]any{
			"recall_top_k": 10.0,
			"retrieval":    map[string]any{"recall_top_k": 99.0},
		},
	}
	migrateConfigMap(doc)
	exec := doc["execution"].(map[string]any)
	nested := exec["retrieval"].(map[string]any)
	if nested["recall_top_k"] != 99.0 {
		t.Errorf("nested value should win, got %v", nested["recall_top_k"])
	}
}

// TestExecutionLegacyDefaultsSnapshot pins every execution default against the
// key an operator's config.json uses today.
//
// Retained only to REGENERATE the frozen v1 snapshot (-update-legacy). It no
// longer verifies against the live struct, because the live struct is nested and
// the snapshot is deliberately flat — TestExecutionDefaultsSurvivedNesting is the
// verification.
func TestExecutionLegacyDefaultsSnapshot(t *testing.T) {
	if !*updateLegacy {
		t.Skip("frozen v1 snapshot; see TestExecutionDefaultsSurvivedNesting")
	}
	got := flattenExecutionDefaults(t)

	if *updateLegacy {
		if err := os.MkdirAll(filepath.Dir(legacyGoldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		encoded, err := stdjson.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		if err := os.WriteFile(legacyGoldenPath, append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("write snapshot: %v", err)
		}
		t.Logf("legacy snapshot rewritten: %s (%d keys)", legacyGoldenPath, len(got))
		return
	}

	raw, err := os.ReadFile(legacyGoldenPath)
	if err != nil {
		t.Fatalf("read %s: %v\nGenerate with: go test ./internal/config/ -run TestExecutionLegacy -update-legacy", legacyGoldenPath, err)
	}
	var want map[string]any
	if err := stdjson.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse snapshot: %v", err)
	}

	// Compare through JSON round-tripped values on both sides so numeric types
	// match (all numbers become float64) and the diff is about VALUES.
	var missing, changed []string
	for k, wantVal := range want {
		gotVal, ok := got[k]
		if !ok {
			missing = append(missing, k)
			continue
		}
		if !jsonEqual(wantVal, gotVal) {
			changed = append(changed, k)
		}
	}
	var added []string
	for k := range got {
		if _, ok := want[k]; !ok {
			added = append(added, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(changed)
	sort.Strings(added)

	for _, k := range missing {
		t.Errorf("execution default %q disappeared — an operator setting it now silently does nothing", k)
	}
	for _, k := range changed {
		t.Errorf("execution default %q changed: snapshot=%v now=%v", k, want[k], got[k])
	}
	for _, k := range added {
		t.Logf("new execution default %q = %v (regenerate the snapshot if intended)", k, got[k])
	}
}

func jsonEqual(a, b any) bool {
	ab, errA := stdjson.Marshal(a)
	bb, errB := stdjson.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ab) == string(bb)
}
