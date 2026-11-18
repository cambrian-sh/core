package config

import (
	stdjson "encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
)

// SchemaV1Flat is the original config schema: one flat `execution` block holding
// all 198 tuning fields.
//
// SchemaV2Nested groups those fields into named blocks (`execution.retrieval`,
// `execution.routing`, …). The fields, their types and their leaf json names are
// unchanged — only the nesting is new — so a v1 file is mechanically convertible
// and this package converts it on load rather than making operators migrate by hand.
//
// SchemaV3Retired is v2 minus the keys retiredConfigKeys lists (owner decision
// 2026-11-14, ENG-1502 / ENG-203): keys that were declared, defaulted and in some
// cases validated, but read by nothing. Every layer is migrated v2→v3 on load
// (retireConfigKeys for files and the ADR-0101 store, retiredEnvKey for CAMBRIAN_*
// variables): a retired key is dropped with one WARN naming it, so an existing
// deployment keeps starting and learns the setting never had an effect.
//
// SchemaV4PythonRetired is v3 minus metabolism.python_executable (ADR-0157 D7.14,
// owner A1-Q13, ENG-2008): Python agents and the Python system tools run only on
// the kernel-managed sandbox Python env, so the operator-chosen interpreter is
// gone. Unlike the v3 keys it HAD an effect; every layer is migrated v3→v4 the
// same way (dropped with one WARN naming D7), so a deployment that still sets it
// keeps starting — on the managed interpreter.
const (
	SchemaV1Flat          = 1
	SchemaV2Nested        = 2
	SchemaV3Retired       = 3
	SchemaV4PythonRetired = 4

	// CurrentSchemaVersion is what this build writes and expects.
	CurrentSchemaVersion = SchemaV4PythonRetired
)

// retiredV4ConfigKeys are the keys schema v4 removed, with the reason.
var retiredV4ConfigKeys = map[string]string{
	"metabolism.python_executable": "ADR-0157 D7: Python agents and Python system tools run only on the kernel-managed sandbox Python env (execution.agents.python; `cambrian-orchestrator python-env status` names it); the kernel launches no other interpreter",
}

// retiredKeyReason looks a path up in both retirements: its reason, and the
// schema version that retired it.
func retiredKeyReason(path string) (reason string, schema int, ok bool) {
	if r, ok := retiredConfigKeys[path]; ok {
		return r, SchemaV3Retired, true
	}
	if r, ok := retiredV4ConfigKeys[path]; ok {
		return r, SchemaV4PythonRetired, true
	}
	return "", 0, false
}

// allRetiredKeys lists every retired path (v3 and v4).
func allRetiredKeys() []string {
	out := make([]string, 0, len(retiredConfigKeys)+len(retiredV4ConfigKeys))
	for k := range retiredConfigKeys {
		out = append(out, k)
	}
	for k := range retiredV4ConfigKeys {
		out = append(out, k)
	}
	return out
}

// retiredConfigKeys are the keys schema v3 removed, by their v2 path, with the
// reason. A v1 flat spelling ("execution.max_recursion_depth") is retired with its
// nested one. None of them had a reader, so dropping the value changes no behaviour.
//
// Why drop them in the migration rather than leave them to the unknown-key warning:
// that warning skips the environment layer (CAMBRIAN_* is shared with non-config
// variables) and calls the key "unknown, misspelled or retired", while these are
// known to be retired and each has a reason an operator can act on.
var retiredConfigKeys = map[string]string{
	"execution.plan.max_recursion_depth":                   "nested sub-auctions retired with the auction (ADR-0100); nesting depth is execution.lane.subtasks",
	"execution.plan.max_partial_context_bytes":             "no reader",
	"execution.plan.budget_exhaustion_alarm_rate":          "no reader",
	"execution.memory.min_gc_age_days":                     "no reader",
	"execution.memory.circadian_stale_doc_warn_threshold":  "no reader; Ebbinghaus decay it guarded was deleted (owner decision, ENG-74)",
	"execution.memory.remember_default_activation":         "no reader",
	"execution.llm.llm_gateway_retry_backoff_ms":           "no reader",
	"execution.routing.proposal_timeout_ms":                "the auction bid round it timed was removed by ADR-0100",
	"execution.verification.verifier_pool_min_size":        "the D15 pool health guard was never built",
	"execution.verification.verifier_pool_threshold_step":  "the D15 pool health guard was never built",
	"execution.verification.verifier_pool_threshold_floor": "the D15 pool health guard was never built",
	"telemetry.otlp_endpoint":                              "no OTLP exporter was ever built; telemetry export is a future feature",
	"telemetry.prometheus_port":                            "no Prometheus exporter was ever built; telemetry export is a future feature",
	// Retired 2026-11-14 (owner decision, third round; ENG-1301) with the ADR-0027
	// procedural plan memory: they tuned a Hippocampus read whose writer had no
	// production caller since 2026-07-18, so they never changed a plan. A map key's
	// sub-paths (hippocampus_policies.<name>.<field>) retire with it.
	"execution.hippocampus.hippocampus_policies":       "the ADR-0027 procedural plan memory it tuned was deleted (ENG-1301); learned routines are ADR-0139 D13 skills",
	"execution.hippocampus.hippocampus_default_policy": "the ADR-0027 procedural plan memory it tuned was deleted (ENG-1301); learned routines are ADR-0139 D13 skills",
	// Retired 2026-11-14 (owner decision, ENG-1595): it gated the ADR-0049 A2.3
	// failure record, a raw Manager.Save of a failed plan's output that is no longer
	// written.
	"execution.memory.experience_surprise_floor": "the A2.3 failure record it gated is no longer written (owner decision, ENG-1595)",
	// Retired 2026-11-15 (owner decision, ENG-1321): IRCoT was recorded as a disqualified
	// lever and its reason-then-retrieve branch is deleted, as HyDE's was.
	"execution.retrieval.agentic_ircot_enabled": "IRCoT is a disqualified retrieval lever; its branch was deleted (owner decision, ENG-1321)",
}

// retiredLogged makes each retired key's WARN appear once per process and origin.
var retiredLogged sync.Map

func logRetiredKey(path, origin string) {
	if _, seen := retiredLogged.LoadOrStore(path+"|"+origin, true); seen {
		return
	}
	reason, schema, _ := retiredKeyReason(path)
	if schema == SchemaV4PythonRetired {
		slog.Warn("config: retired key ignored (schema v4); Python agents run on the kernel-managed sandbox Python env (ADR-0157 D7), remove it",
			slog.String("key", path),
			slog.String("origin", origin),
			slog.String("reason", reason),
			slog.String("ticket", "ENG-2008"))
		return
	}
	slog.Warn("config: retired key ignored (schema v3); it never had an effect, remove it",
		slog.String("key", path),
		slog.String("origin", origin),
		slog.String("reason", reason),
		slog.String("ticket", "ENG-1502"))
}

// retiredFlatSpelling returns the v1 flat path of a retired execution key
// ("execution.plan.x" → "execution.x"), or "" for a key outside execution.
func retiredFlatSpelling(path string) string {
	rest, ok := strings.CutPrefix(path, "execution.")
	if !ok {
		return ""
	}
	_, leaf, ok := strings.Cut(rest, ".")
	if !ok {
		return ""
	}
	return "execution." + leaf
}

// retireConfigKeys deletes every retiredConfigKeys entry (and its v1 flat
// spelling) from a decoded config document, in place, logging each, and returns the
// paths it removed, sorted. It runs after the v1→v2 steps, per layer.
func retireConfigKeys(doc map[string]any, origin string) []string {
	var removed []string
	drop := func(path, reportAs string) {
		segs := strings.Split(path, ".")
		m := doc
		chain := []map[string]any{doc}
		for _, seg := range segs[:len(segs)-1] {
			next, ok := m[seg].(map[string]any)
			if !ok {
				return
			}
			m = next
			chain = append(chain, m)
		}
		leaf := segs[len(segs)-1]
		if _, present := m[leaf]; !present {
			return
		}
		delete(m, leaf)
		// A block emptied by the retirement goes too (execution.hippocampus once both
		// of its keys are dropped), so it is not reported as an unknown key. Never
		// the top-level document, and never a block the layer left non-empty.
		for i := len(chain) - 1; i >= 1 && len(chain[i]) == 0; i-- {
			delete(chain[i-1], segs[i-1])
		}
		logRetiredKey(reportAs, origin)
		removed = append(removed, path)
	}
	for _, path := range allRetiredKeys() {
		drop(path, path)
		if flat := retiredFlatSpelling(path); flat != "" {
			drop(flat, path)
		}
	}
	sort.Strings(removed)
	return removed
}

// retiredEnvKey reports whether an environment-derived config path names a retired
// key (nested or v1 flat spelling), logging it once. The caller drops the variable.
func retiredEnvKey(path string) bool {
	if _, _, ok := retiredKeyReason(path); ok {
		logRetiredKey(path, "environment")
		return true
	}
	// A variable may also name a path INSIDE a retired map key
	// (CAMBRIAN_EXECUTION__HIPPOCAMPUS__HIPPOCAMPUS_POLICIES__CODEGEN__SIMILARITY).
	under := func(p, key string) bool { return p == key || strings.HasPrefix(p, key+".") }
	for _, retired := range allRetiredKeys() {
		if under(path, retired) {
			logRetiredKey(retired, "environment")
			return true
		}
		if flat := retiredFlatSpelling(retired); flat != "" && under(path, flat) {
			logRetiredKey(retired, "environment")
			return true
		}
	}
	return false
}

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

// deprecatedExecutionKey is a key that was REPLACED rather than retired: an
// operator's explicit value still means something, so it is carried to its
// successor instead of being ignored. Keyed by v2 block, then the old leaf name.
//
// plan.plan_timeout_ms (the fixed whole-plan ceiling, 120 s by default) was
// replaced on 2026-11-05 by a progress-aware pair, plan_idle_timeout_ms and
// plan_max_duration_ms (owner decision,
// T-20261105-owner-decision-needed-120-s-plan-timeout-ms). An operator who set
// plan_timeout_ms chose a HARD CAP, so the value becomes plan_max_duration_ms; the
// idle watchdog runs at its default on top. A layer that states both keeps its
// plan_max_duration_ms — the newer statement of intent, as in
// migrateExecutionBlock's nested-wins rule.
var deprecatedExecutionKey = map[string]map[string]string{
	"plan": {"plan_timeout_ms": "plan_max_duration_ms"},
}

// relocatedExecutionKey is a key that MOVED to another v2 block, unchanged in
// meaning and unit: "block.leaf" of the old place → "block.leaf" of the new one. It
// is carried exactly like a deprecatedExecutionKey (a value already set at the new
// place in the same layer wins), for files, the durable store and CAMBRIAN_*
// variables alike.
//
// The content-size limits were gathered into execution.content on 2026-11-12
// (owner decision, T-20261112-worker-resolve-cid-inline-has-no-size-cap-a):
//   - ledger.page_bytes → content.page_bytes (ledger_get's page, now also
//     resolve_cid's window and the dependency outline's page numbers);
//   - plan.dependency_results_max_chars → content.dependency_max_bytes. The old key
//     WAS the whole budget (bytes, despite its name); the budget is now a token
//     budget sized from the receiving worker, and an operator's explicit value
//     stays what it always was — a byte ceiling on it.
var relocatedExecutionKey = map[string]string{
	"ledger.page_bytes":                 "content.page_bytes",
	"plan.dependency_results_max_chars": "content.dependency_max_bytes",
}

// deprecationLogged makes each deprecated key's INFO line appear once per process,
// not once per layer per (re)load.
var deprecationLogged sync.Map

func logDeprecatedKey(block, oldKey, newKey, origin string, overridden bool) {
	if _, seen := deprecationLogged.LoadOrStore(block+"."+oldKey, true); seen {
		return
	}
	msg := "config: deprecated key loaded as its successor"
	if overridden {
		msg = "config: deprecated key ignored; its successor is set in the same layer"
	}
	slog.Info(msg,
		slog.String("deprecated", "execution."+block+"."+oldKey),
		slog.String("successor", "execution."+block+"."+newKey),
		slog.String("origin", origin),
		slog.String("ticket", "T-20261105-owner-decision-needed-120-s-plan-timeout-ms"))
}

func logRelocatedKey(from, to, origin string, overridden bool) {
	if _, seen := deprecationLogged.LoadOrStore(from, true); seen {
		return
	}
	msg := "config: moved key loaded at its new place"
	if overridden {
		msg = "config: moved key ignored; its new place is set in the same layer"
	}
	ticket := "T-20261112-worker-resolve-cid-inline-has-no-size-cap-a"
	if strings.HasPrefix(from, "routing.") {
		ticket = "T-20261113-adr-0150-slice-1-single-agent-default-lane" // ADR-0150 D10
	}
	slog.Info(msg,
		slog.String("old", "execution."+from),
		slog.String("new", "execution."+to),
		slog.String("origin", origin),
		slog.String("ticket", ticket))
}

// renameDeprecatedExecutionKeys carries each relocatedExecutionKey and each
// deprecatedExecutionKey in a v2
// `execution` block to its successor, in place, and returns the deprecated keys it
// touched ("plan.plan_timeout_ms"), sorted.
// It runs AFTER migrateExecutionBlock, so a v1 flat plan_timeout_ms has already
// been routed into `plan` and is handled by the same code path as a v2 one.
func renameDeprecatedExecutionKeys(doc map[string]any, origin string) []string {
	exec, ok := doc["execution"].(map[string]any)
	if !ok {
		return nil
	}
	var renamed []string
	for from, to := range relocatedExecutionKey {
		fromBlock, fromLeaf, _ := strings.Cut(from, ".")
		toBlock, toLeaf, _ := strings.Cut(to, ".")
		src, ok := exec[fromBlock].(map[string]any)
		if !ok {
			continue
		}
		val, present := src[fromLeaf]
		if !present {
			continue
		}
		dst, ok := exec[toBlock].(map[string]any)
		if !ok {
			if _, taken := exec[toBlock]; taken {
				continue // the new block is a scalar: leave the layer to the decoder's error
			}
			dst = map[string]any{}
			exec[toBlock] = dst
		}
		_, overridden := dst[toLeaf]
		if !overridden {
			dst[toLeaf] = val
		}
		delete(src, fromLeaf)
		logRelocatedKey(from, to, origin, overridden)
		renamed = append(renamed, from)
	}
	for block, renames := range deprecatedExecutionKey {
		dst, ok := exec[block].(map[string]any)
		if !ok {
			continue
		}
		for oldKey, newKey := range renames {
			val, present := dst[oldKey]
			if !present {
				continue
			}
			_, overridden := dst[newKey]
			if !overridden {
				dst[newKey] = val
			}
			delete(dst, oldKey)
			logDeprecatedKey(block, oldKey, newKey, origin, overridden)
			renamed = append(renamed, block+"."+oldKey)
		}
	}
	sort.Strings(renamed)
	return renamed
}

// laneMigration carries the ADR-0050 D1 single-agent keys of
// execution.routing into execution.lane (ADR-0150 D10), in place, and returns
// the old keys it touched ("routing.bypass_selection"), sorted. It runs after
// migrateExecutionBlock, so a v1 flat key is already in `routing`.
//
//   - routing.bypass_selection: true loads as lane.default: single_agent; false
//     is dropped (it only ever meant "not the control arm", and the planner lane
//     is no longer the default — owner decision 2026-11-13). A lane.default set
//     in the same layer wins.
//   - routing.single_agent_id loads as lane.single_agent.agent_id, unless that
//     is set in the same layer.
func laneMigration(doc map[string]any, origin string) []string {
	exec, ok := doc["execution"].(map[string]any)
	if !ok {
		return nil
	}
	routing, ok := exec["routing"].(map[string]any)
	if !ok {
		return nil
	}
	var touched []string
	laneBlock := func() map[string]any {
		lane, ok := exec["lane"].(map[string]any)
		if !ok {
			if _, taken := exec["lane"]; taken {
				return nil // a scalar: leave the layer to the decoder's error
			}
			lane = map[string]any{}
			exec["lane"] = lane
		}
		return lane
	}
	if v, present := routing["bypass_selection"]; present {
		delete(routing, "bypass_selection")
		touched = append(touched, "routing.bypass_selection")
		if truthy(v) {
			if lane := laneBlock(); lane != nil {
				_, overridden := lane["default"]
				if !overridden {
					lane["default"] = LaneSingleAgent
				}
				logRelocatedKey("routing.bypass_selection", "lane.default", origin, overridden)
			}
		} else {
			logRelocatedKey("routing.bypass_selection", "lane.default", origin, true)
		}
	}
	if v, present := routing["single_agent_id"]; present {
		delete(routing, "single_agent_id")
		touched = append(touched, "routing.single_agent_id")
		if lane := laneBlock(); lane != nil {
			sa, ok := lane["single_agent"].(map[string]any)
			if !ok {
				sa = map[string]any{}
				lane["single_agent"] = sa
			}
			_, overridden := sa["agent_id"]
			if !overridden {
				sa["agent_id"] = v
			}
			logRelocatedKey("routing.single_agent_id", "lane.single_agent.agent_id", origin, overridden)
		}
	}
	sort.Strings(touched)
	return touched
}

// truthy reads a JSON or environment boolean the way the struct decoder's
// weak typing does ("true", "1", true).
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "t", "yes":
			return true
		}
	case float64:
		return t != 0
	case int:
		return t != 0
	}
	return false
}

// laneEnvKey is laneMigration for one environment variable: the config path it
// loads as and its value, or ("", nil) when it is dropped.
// CAMBRIAN_EXECUTION__ROUTING__BYPASS_SELECTION=true loads as lane.default
// single_agent and CAMBRIAN_EXECUTION__ROUTING__SINGLE_AGENT_ID as
// lane.single_agent.agent_id, unless the new variable is set too (it wins).
func laneEnvKey(path, value string, lookupEnv func(string) (string, bool)) (string, any) {
	switch path {
	case "execution.routing.bypass_selection":
		if _, set := lookupEnv("CAMBRIAN_EXECUTION__LANE__DEFAULT"); set || !truthy(value) {
			logRelocatedKey("routing.bypass_selection", "lane.default", "environment", true)
			return "", nil
		}
		logRelocatedKey("routing.bypass_selection", "lane.default", "environment", false)
		return "execution.lane.default", LaneSingleAgent
	case "execution.routing.single_agent_id":
		if _, set := lookupEnv("CAMBRIAN_EXECUTION__LANE__SINGLE_AGENT__AGENT_ID"); set {
			logRelocatedKey("routing.single_agent_id", "lane.single_agent.agent_id", "environment", true)
			return "", nil
		}
		logRelocatedKey("routing.single_agent_id", "lane.single_agent.agent_id", "environment", false)
		return "execution.lane.single_agent.agent_id", value
	}
	return path, value
}

// deprecatedEnvKey maps an environment-derived config path naming a deprecated
// key to the path it now loads as, or returns it unchanged. lookupEnv reports
// whether a variable is set, so a deployment that sets both the old and the new
// variable keeps the new one (the old path is then left as-is, where the struct
// decoder ignores it).
func deprecatedEnvKey(path string, lookupEnv func(string) (string, bool)) string {
	rest, ok := strings.CutPrefix(path, "execution.")
	if !ok {
		return path
	}
	block, leaf, ok := strings.Cut(rest, ".")
	if !ok {
		return path
	}
	if to, moved := relocatedExecutionKey[block+"."+leaf]; moved {
		toBlock, toLeaf, _ := strings.Cut(to, ".")
		newVar := "CAMBRIAN_EXECUTION__" + strings.ToUpper(toBlock) + "__" + strings.ToUpper(toLeaf)
		if _, set := lookupEnv(newVar); set {
			logRelocatedKey(block+"."+leaf, to, "environment", true)
			return path
		}
		logRelocatedKey(block+"."+leaf, to, "environment", false)
		return "execution." + to
	}
	newKey, ok := deprecatedExecutionKey[block][leaf]
	if !ok {
		return path
	}
	newVar := "CAMBRIAN_EXECUTION__" + strings.ToUpper(block) + "__" + strings.ToUpper(newKey)
	if _, set := lookupEnv(newVar); set {
		logDeprecatedKey(block, leaf, newKey, "environment", true)
		return path
	}
	logDeprecatedKey(block, leaf, newKey, "environment", false)
	return "execution." + block + "." + newKey
}

// migrateJSON converts one config document's bytes to the current schema when
// needed (v1→v2 nesting, deprecated and relocated keys, v2→v3 retirements).
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
	renamed := renameDeprecatedExecutionKeys(doc, origin)
	renamed = append(renamed, laneMigration(doc, origin)...)
	// v2→v3: retired keys are dropped from this layer (each logged once).
	if retired := retireConfigKeys(doc, origin); len(retired) > 0 {
		doc["schema_version"] = CurrentSchemaVersion
		renamed = append(renamed, retired...)
	}
	if len(moved) == 0 && len(renamed) == 0 {
		return b
	}
	out, err := stdjson.Marshal(doc)
	if err != nil {
		return b
	}
	if len(moved) > 0 {
		slog.Info("config: migrated legacy flat execution block to nested schema",
			slog.String("origin", origin),
			slog.Int("from_schema", SchemaV1Flat),
			slog.Int("to_schema", SchemaV2Nested),
			slog.Int("keys_moved", len(moved)),
			slog.String("example", moved[0]))
	}
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

// MigrateFileInPlace rewrites an older config file on disk to the current schema
// (v1 flat → v2 nested, deprecated keys to their successors, retired v3 keys
// removed), returning the keys it moved or removed. It is NOT called during load — loading migrates in memory and leaves
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
	// Deprecated keys are carried to their successors too: an explicit migrate
	// should leave a file that loads without a deprecation notice.
	moved = append(moved, renameDeprecatedExecutionKeys(doc, path)...)
	moved = append(moved, laneMigration(doc, path)...)
	if retired := retireConfigKeys(doc, path); len(retired) > 0 {
		doc["schema_version"] = CurrentSchemaVersion
		moved = append(moved, retired...)
	}
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
//
// A RETIRED key has no entry here on purpose: a v1 file that still carries it is
// not routed into a block, so the key stays at the top of `execution`, where the
// struct decoder ignores it (unknown keys are tolerated, never an error). The
// retired set and the reason for each is retiredExecutionFields in
// execution_migration_test.go. Keys retired in schema v3 are removed outright, with
// a WARN, by retireConfigKeys / retiredEnvKey (retiredConfigKeys above).
var legacyExecutionKey = map[string]string{
	"agent_env_passthrough":                    "agents",
	"agent_memory_limit_mb":                    "agents",
	"agent_pinning":                            "routing",
	"agentic_decompose_enabled":                "retrieval",
	"agentic_max_hops":                         "retrieval",
	"agentic_planner_model":                    "retrieval",
	"agentic_retrieval_enabled":                "retrieval",
	"anchor_constraint_enabled":                "retrieval",
	"bypass_selection":                         "routing",
	"blend_enabled":                            "retrieval",
	"blend_weight_activation":                  "retrieval",
	"blend_weight_coherence":                   "retrieval",
	"blend_weight_confidence":                  "retrieval",
	"blend_weight_cosine":                      "retrieval",
	"blend_weight_lexical":                     "retrieval",
	"blend_weight_pagerank":                    "retrieval",
	"blend_weight_recency":                     "retrieval",
	"canonical_vocab":                          "capability",
	"capability_aliases":                       "capability",
	"capability_contract":                      "routing",
	"capability_resolution":                    "capability",
	"capture_llm_exchanges":                    "llm",
	"chat_pool_acquire_timeout_seconds":        "chat",
	"chat_pool_agent_id":                       "chat",
	"chat_pool_queue_size":                     "chat",
	"chat_pool_size":                           "chat",
	"classification_vocabulary":                "router",
	"context_growth_k":                         "plan",
	"context_ref_snippet_chars":                "plan",
	"cross_verify_rate":                        "verification",
	"daemon_restart_base_backoff_ms":           "agents",
	"daemon_restart_max_attempts":              "agents",
	"daemon_restart_max_backoff_ms":            "agents",
	"daemon_restart_window_seconds":            "agents",
	"discovery_safe_tools":                     "tools",
	"dispatch_cheap_energy_max":                "routing",
	"dispatch_merit_floor":                     "routing",
	"edge_extraction_batch_size":               "ingestion",
	"edge_extraction_llm_timeout_ms":           "ingestion",
	"edge_extraction_max_idle_ms":              "ingestion",
	"edge_extraction_queue_size":               "ingestion",
	"ewma_alpha":                               "supervision",
	"experience_records_enabled":               "memory",
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
	"histogram_alpha":                          "supervision",
	"histogram_min_samples":                    "supervision",
	"hnsw_ef_search":                           "retrieval",
	"hybrid_lexical_weight":                    "retrieval",
	"hybrid_rrf_k":                             "retrieval",
	"hybrid_search_enabled":                    "retrieval",
	"ingest_token":                             "ingestion",
	"ingestion_batch_size":                     "ingestion",
	"ingestion_batch_wait_ms":                  "ingestion",
	"ingestion_http_port":                      "ingestion",
	"ingestion_queue_size":                     "ingestion",
	"ingestion_workers":                        "ingestion",
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
	"max_fanout_width":                         "plan",
	"max_memory_results":                       "memory",
	"max_neighbor_expansion":                   "memory",
	"max_plan_cost":                            "plan",
	"max_replan_attempts":                      "plan",
	"max_session_tokens":                       "session",
	"max_step_energy":                          "plan",
	"memory_relevance_threshold":               "memory",
	"min_step_energy":                          "plan",
	"min_verified_events":                      "verification",
	"neighbor_window_enabled":                  "retrieval",
	"per_capability_merit":                     "routing",
	"plan_drift_days":                          "plan",
	"plan_preview_only":                        "plan",
	"plan_timeout_ms":                          "plan",
	"procedure_deprecate_below":                "procedure",
	"profile_aggregator_interval_seconds":      "supervision",
	"query_entity_seeding_enabled":             "retrieval",
	"recall_over_fetch":                        "retrieval",
	"recall_similarity_floor":                  "retrieval",
	"recall_spreading_enabled":                 "retrieval",
	"recall_top_k":                             "retrieval",
	"require_explicit_session":                 "session",
	"reranker_enabled":                         "retrieval",
	"reranker_top_k":                           "retrieval",
	"reranker_weight":                          "retrieval",
	"retrieval_floor":                          "retrieval",
	"router_classification_body_chars":         "router",
	"router_min_classification_confidence":     "router",
	"routing_trace_enabled":                    "routing",
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
	"tool_effects_strict":                      "tools",
	"tool_retrieval_floor":                     "tools",
	"tools_auto_approve":                       "tools",
	"tools_unrestricted":                       "tools",
	"trust_boost_threshold":                    "verification",
	"trust_score_abs_weight":                   "verification",
	"trust_score_cal_weight":                   "verification",
	"use_global_workspace":                     "workspace",
	"verification_queue_capacity":              "verification",
	"verifier_pool_threshold":                  "verification",
	"verifier_recency_window":                  "verification",
	"workspace_execution_slots":                "workspace",
	"workspace_min_fact_cosine":                "workspace",
	"workspace_planning_slots":                 "workspace",
}
