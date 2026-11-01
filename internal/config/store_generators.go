package config

import (
	stdjson "encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ADR-0101 D9: generators are stored PER ID, not as one list.
//
// A stored `llm_provider.generators` value is a list, and koanf replaces a list
// wholesale, so one console edit to one generator hid every generator in every
// file layer (2026-11-01: a Gemini generator added to providers.json was
// invisible, and naming it llm_provider.default would have failed boot). Each
// stored generator now lives under its own key and is merged BY ID over the file
// layers' list at load, so a stored generator overrides only the file generator
// with the same id and a generator added to a file stays visible.

// GeneratorOverridePrefix + id is the store key of one stored generator. The id
// is everything after the prefix, so an id containing dots ("deepseek-v3.2") is
// kept intact: these keys are split out of the overrides BEFORE the dotted-key
// expansion that would otherwise read the dots as nesting.
const GeneratorOverridePrefix = "llm_provider.generator_overrides."

// GeneratorOverrideKey is the store key for generator id.
func GeneratorOverrideKey(id string) string { return GeneratorOverridePrefix + id }

// StoredGenerator is one stored generator entry: the full generator (it REPLACES
// the file entry with the same id, it does not patch fields of it), or a
// tombstone (Removed) that hides a file generator the console removed but could
// not delete from a non-writable file.
type StoredGenerator struct {
	GeneratorConfig
	Removed bool `json:"removed,omitempty"`
}

// SplitGeneratorOverrides separates the store's generator entries from its other
// overrides. It reads both layouts: per-id keys (D9) and the pre-D9 whole stored
// list under `llm_provider.generators`, each of whose elements becomes an entry
// unless a per-id key for the same id exists (the per-id key is newer by
// construction — nothing writes the list form any more). legacy reports whether
// the old list form was present.
func SplitGeneratorOverrides(overrides map[string]any) (gens map[string]StoredGenerator, rest map[string]any, legacy bool) {
	gens = map[string]StoredGenerator{}
	rest = make(map[string]any, len(overrides))
	var legacyRaw any
	for k, v := range overrides {
		switch {
		case strings.HasPrefix(k, GeneratorOverridePrefix):
			id := strings.TrimPrefix(k, GeneratorOverridePrefix)
			var sg StoredGenerator
			if decodeInto(v, &sg) == nil && id != "" {
				sg.ID = id // the key is the identity; a mismatched body cannot rename it
				gens[id] = sg
			}
		case k == GeneratorsKey:
			legacyRaw, legacy = v, true
		default:
			rest[k] = v
		}
	}
	if legacy {
		var list []GeneratorConfig
		if decodeInto(legacyRaw, &list) == nil {
			for _, g := range list {
				if g.ID == "" {
					continue
				}
				if _, newer := gens[g.ID]; !newer {
					gens[g.ID] = StoredGenerator{GeneratorConfig: g}
				}
			}
		}
	}
	return gens, rest, legacy
}

// MergeGenerators applies stored entries by id over a base (file) list: a stored
// generator replaces the base entry with its id in place, a tombstone drops it,
// and a stored generator with no base entry is appended (in id order, so the
// result is deterministic).
func MergeGenerators(base []GeneratorConfig, stored map[string]StoredGenerator) []GeneratorConfig {
	out := make([]GeneratorConfig, 0, len(base)+len(stored))
	seen := make(map[string]bool, len(base))
	for _, g := range base {
		seen[g.ID] = true
		if sg, ok := stored[g.ID]; ok {
			if sg.Removed {
				continue
			}
			out = append(out, sg.GeneratorConfig)
			continue
		}
		out = append(out, g)
	}
	ids := make([]string, 0, len(stored))
	for id := range stored {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if sg := stored[id]; !sg.Removed {
			out = append(out, sg.GeneratorConfig)
		}
	}
	return out
}

// MigrateStoredGenerators rewrites a pre-D9 stored generator list into per-id
// entries and deletes the list key (AGENTS.md §3.4: a schema change ships with its
// migration). Idempotent: a store without the list form is untouched. Returns the
// ids it migrated.
//
// Semantics change on purpose: the old list HID every file generator it did not
// name; per-id entries do not, so a generator added to a file after the list was
// stored becomes visible. That is the D9 fix, not a side effect — but the caller
// logs the ids so the change is observable.
func MigrateStoredGenerators(store Store) ([]string, error) {
	if store == nil {
		return nil, nil
	}
	overrides, err := store.Overrides()
	if err != nil {
		return nil, fmt.Errorf("migrate stored generators: read: %w", err)
	}
	if _, ok := overrides[GeneratorsKey]; !ok {
		return nil, nil
	}
	gens, _, _ := SplitGeneratorOverrides(overrides)
	ids := make([]string, 0, len(gens))
	for id, sg := range gens {
		if _, exists := overrides[GeneratorOverrideKey(id)]; exists {
			continue // a per-id entry already wins; leave it alone
		}
		if err := store.SetOverride(GeneratorOverrideKey(id), sg); err != nil {
			return nil, fmt.Errorf("migrate stored generators: write %s: %w", id, err)
		}
		ids = append(ids, id)
	}
	if err := store.DeleteOverride(GeneratorsKey); err != nil {
		return nil, fmt.Errorf("migrate stored generators: delete list: %w", err)
	}
	sort.Strings(ids)
	return ids, nil
}

func decodeInto(v any, out any) error {
	b, err := stdjson.Marshal(v)
	if err != nil {
		return err
	}
	return stdjson.Unmarshal(b, out)
}
