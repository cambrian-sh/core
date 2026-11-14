package config

import (
	"log/slog"
	"sort"
)

// inertConfigKeys are keys this kernel still DECLARES (a Config field, a default, in
// some cases a validation rule) but that no code reads: the value an operator sets
// changes nothing. AGENTS.md §3.4 calls a validated-but-unread setting a defect, so
// each entry names the ticket that will wire or retire it, and the kernel names any
// such key an operator sets at load instead of silently accepting it.
//
// Empty since 2026-11-14: the eleven keys listed here were retired in schema v3
// (owner decision, ENG-1502; retiredConfigKeys in execution_migrate.go). The map
// stays as the one place a newly found unread key is declared while it waits for a
// decision; TestEveryConfigKeyHasAConsumer fails for an unread key not listed here,
// and TestInertConfigKeys_AreDeclaredAndUnread fails if a listed key gains a reader.
var inertConfigKeys = map[string]string{}

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
