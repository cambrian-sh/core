package config

import (
	"log/slog"
	"sort"
	"strings"
)

// unknownConfigKeys narrows the keys the decode left unconsumed (mapstructure
// Metadata.Unused over the merged koanf tree) to the ones an operator should be told
// about: keys a FILE layer or the ADR-0101 store supplied that no Config field reads.
//
// Why a warning and not an error: the loader has always tolerated unknown keys, and a
// retired key is deliberately left loadable (execution_migrate.go: "tolerated, never
// an error") so an upgrade never refuses an existing deployment's file. But tolerated
// must not mean silent — a key that is accepted and then ignored tells the operator
// their setting took (AGENTS.md §3.4). The comments on retired fields in config.go
// promise exactly this warning (T-20261103-config-loader-silently-ignores-unknown-removed).
//
// Excluded:
//   - keys under the environment layer: CAMBRIAN_* is a namespace shared with
//     variables that are not config at all (CAMBRIAN_PYTHON, CAMBRIAN_AGENTS_DIR,
//     CAMBRIAN_CONFIG_STORE, …), so an unconsumed env key is not evidence of a typo;
//   - any path segment starting with "_": the shipped files carry "_comment" keys;
//   - keys attributed to the default layer: those come from DefaultConfig() itself.
//
// unused uses mapstructure's spelling ("a.b.c", "list[0].x"); prov uses koanf's flat
// keys ("a.b.c", lists whole). A key's source is that of its longest dotted prefix
// present in prov.
func unknownConfigKeys(unused []string, prov Provenance) []string {
	var out []string
	for _, key := range unused {
		if hasCommentSegment(key) {
			continue
		}
		src, ok := sourceOf(key, prov)
		if !ok || src == SourceDefault || strings.HasPrefix(src, SourceEnvPrefix) {
			continue
		}
		out = append(out, key+" ("+src+")")
	}
	sort.Strings(out)
	return out
}

func hasCommentSegment(key string) bool {
	for _, seg := range strings.Split(key, ".") {
		if strings.HasPrefix(seg, "_") {
			return true
		}
	}
	return false
}

// sourceOf returns the provenance label of key, falling back to its longest dotted
// prefix (a list element "a.b[0].c" resolves through "a.b").
func sourceOf(key string, prov Provenance) (string, bool) {
	k := key
	if i := strings.IndexByte(k, '['); i >= 0 {
		k = k[:i]
	}
	for k != "" {
		if src, ok := prov[k]; ok {
			return src, true
		}
		i := strings.LastIndexByte(k, '.')
		if i < 0 {
			break
		}
		k = k[:i]
	}
	// An unknown BLOCK is reported at its root ("no_such_block") while koanf records
	// its leaves ("no_such_block.a"). An env leaf under the block wins (so it suppresses
	// the warning, as for a single key); otherwise the source of the first leaf in key
	// order, so the label is deterministic.
	prefix := key + "."
	best, bestKey, found := "", "", false
	for pk, src := range prov {
		if !strings.HasPrefix(pk, prefix) {
			continue
		}
		if strings.HasPrefix(src, SourceEnvPrefix) {
			return src, true
		}
		if !found || pk < bestKey {
			best, bestKey, found = src, pk, true
		}
	}
	return best, found
}

// warnUnknownConfigKeys logs one WARN naming every unknown key, or nothing. No
// behaviour change: the keys were ignored before and are ignored now.
func warnUnknownConfigKeys(keys []string) {
	if len(keys) == 0 {
		return
	}
	slog.Warn("config: keys set in a config file or the config store are not read by this kernel "+
		"(unknown, misspelled or retired) and have no effect",
		slog.Int("count", len(keys)), slog.Any("keys", keys))
}
