package config

import (
	"log/slog"
	"sort"
)

// inertConfigKeys are keys this kernel still DECLARES (a Config field, a default, in
// some cases a validation rule and a v1 migration route) but that no code reads: the
// value an operator sets changes nothing. AGENTS.md §3.4 calls a validated-but-unread
// setting a defect; each one is ticketed below.
//
// Why they are not simply deleted: removing a key is a change to the config schema
// contract (AGENTS.md §5.5) and to the frozen v1 schema the migration tests hold
// (testdata/execution_legacy_defaults.json), and wiring each needs a decision about
// what it should mean today (several configured mechanisms that ADR-0100 retired).
// Both wait for the owner. Until then the kernel says so at load, whenever a key is
// set by any layer above the defaults, instead of silently accepting it.
//
// Remove an entry in the same change that wires or retires its key;
// TestInertConfigKeys_AreDeclaredAndUnread fails if a key here gains a reader.
var inertConfigKeys = map[string]string{
	"execution.plan.max_recursion_depth":                   "T-20261113-execution-plan-max-recursion-depth-is-validated (nested sub-auctions: retired with the auction, ADR-0100; nesting depth today is execution.lane.subtasks)",
	"execution.plan.max_partial_context_bytes":             "T-20261113-eight-config-fields-are-defaulted-some",
	"execution.plan.budget_exhaustion_alarm_rate":          "T-20261113-eight-config-fields-are-defaulted-some",
	"execution.memory.min_gc_age_days":                     "T-20261113-eight-config-fields-are-defaulted-some",
	"execution.memory.circadian_stale_doc_warn_threshold":  "T-20261113-eight-config-fields-are-defaulted-some",
	"execution.memory.remember_default_activation":         "T-20261113-eight-config-fields-are-defaulted-some",
	"execution.llm.llm_gateway_retry_backoff_ms":           "T-20261113-eight-config-fields-are-defaulted-some",
	"execution.routing.proposal_timeout_ms":                "T-20261105-execution-routing-proposal-timeout-ms-is-parsed (its reader, the auction bid round, was removed by ADR-0100)",
	"execution.verification.verifier_pool_min_size":        "T-20261113-verifier-pool-health-guard-config-verifier-pool",
	"execution.verification.verifier_pool_threshold_step":  "T-20261113-verifier-pool-health-guard-config-verifier-pool",
	"execution.verification.verifier_pool_threshold_floor": "T-20261113-verifier-pool-health-guard-config-verifier-pool",
}

// setInertConfigKeys returns the inert keys some layer above the defaults (a file, the
// store or a CAMBRIAN_* variable) sets, each with the layer, sorted.
func setInertConfigKeys(prov Provenance) []string {
	var out []string
	for key := range inertConfigKeys {
		if src, ok := prov[key]; ok && src != SourceDefault {
			out = append(out, key+" ("+src+")")
		}
	}
	sort.Strings(out)
	return out
}

// warnInertConfigKeys logs one WARN when an operator sets a key that has no effect.
// No behaviour change: the values were ignored before and are ignored now.
func warnInertConfigKeys(prov Provenance) {
	set := setInertConfigKeys(prov)
	if len(set) == 0 {
		return
	}
	reasons := make([]string, 0, len(set))
	for key := range inertConfigKeys {
		if src, ok := prov[key]; ok && src != SourceDefault {
			reasons = append(reasons, key+": "+inertConfigKeys[key])
		}
	}
	sort.Strings(reasons)
	slog.Warn("config: these keys are accepted but no code reads them, so setting them has no effect",
		slog.Int("count", len(set)), slog.Any("keys", set), slog.Any("tickets", reasons))
}
