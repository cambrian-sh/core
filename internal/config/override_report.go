package config

import (
	stdjson "encoding/json"
	"sort"
	"strings"
)

// StoreOverride describes one key the embedded store holds, and the file layer
// (if any) it overrides. It is the shared answer behind the boot WARN and
// `orchestrator config overrides list` (ADR-0101 D8), so the two cannot disagree.
type StoreOverride struct {
	// Key is the exact store key (what `overrides unset` takes).
	Key        string
	StoreValue any
	// File is the file layer that also states the key ("" when none does), with
	// its path and value.
	File      string
	FilePath  string
	FileValue any
	// Differs is true when a file states the key with a DIFFERENT value — the
	// case where an operator's file edit is silently ineffective.
	Differs bool
	// Secret marks a secret-shaped key whose values must never be printed.
	Secret bool
}

// StoreOverrideReport lists every store override against the file layers as they
// are on disk now. Generator entries (D9) are compared with the file generator of
// the same id; the pre-D9 whole list is reported under its own key.
func StoreOverrideReport(configPath string, overrides map[string]any) []StoreOverride {
	gens, rest, legacy := SplitGeneratorOverrides(overrides)
	out := make([]StoreOverride, 0, len(overrides))

	for key, val := range rest {
		r := StoreOverride{Key: key, StoreValue: val, Secret: IsSecretKey(key)}
		if o, ok := ResolveFileSetting(configPath, key); ok {
			r.File, r.FilePath, r.FileValue = o.Source, o.Path, o.Value
			r.Differs = !SameValue(o.Value, val)
		}
		out = append(out, r)
	}

	for id, sg := range gens {
		key := GeneratorOverrideKey(id)
		if _, perID := overrides[key]; !perID {
			continue // came from the legacy list; reported below as one entry
		}
		var val any = sg
		if sg.Removed {
			val = "removed"
		}
		r := StoreOverride{Key: key, StoreValue: val}
		if o, ok := ResolveGeneratorFile(configPath, id); ok {
			r.File, r.FilePath, r.FileValue = o.Source, o.Path, o.Value
			r.Differs = sg.Removed || !SameValue(o.Value, sg.GeneratorConfig)
		}
		out = append(out, r)
	}

	if legacy {
		r := StoreOverride{Key: GeneratorsKey, StoreValue: overrides[GeneratorsKey]}
		if o, ok := ResolveFileSetting(configPath, GeneratorsKey); ok {
			r.File, r.FilePath, r.FileValue = o.Source, o.Path, o.Value
			r.Differs = !SameValue(o.Value, overrides[GeneratorsKey])
		}
		out = append(out, r)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// DisplayValue renders a value for a log line or the CLI: compact JSON, or
// "<redacted>" for a secret-shaped key. Long values are truncated — a generator
// object is evidence, not something to read in full in a log line.
func DisplayValue(v any, secret bool) string {
	if secret {
		return "<redacted>"
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := stdjson.Marshal(v)
	if err != nil {
		return "?"
	}
	s := string(b)
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	return strings.TrimSpace(s)
}
