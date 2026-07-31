package config

import (
	"sort"
	"strings"
)

// Store is the read/write port for the embedded config layer (ADR-0101 D1). It
// sits ABOVE the JSON files and BELOW the CAMBRIAN_* environment, so an
// operator's write outlives a restart and beats the shipped defaults, while a
// deployment configured by environment still wins.
//
// The port lives here and the bbolt implementation lives in internal/storage, so
// this package keeps no infrastructure dependency.
//
// A nil Store is valid everywhere and means "no store layer" — which is exactly
// how the kernel behaved before ADR-0101, and how a benchmark arm that must
// ignore operator writes is configured.
type Store interface {
	// Overrides returns every stored config override, keyed by flat dotted path
	// ("execution.ewma_alpha"). Values are JSON-representable scalars or slices.
	Overrides() (map[string]any, error)

	// SetOverride durably records one override. The caller is responsible for
	// the D3 shadow check; the store records intent either way, because a value
	// pinned by an env var today takes effect the moment that var is removed.
	SetOverride(key string, value any) error

	// DeleteOverride removes one override, reverting the key to whatever the
	// layers below the store supply. Deleting an absent key is not an error —
	// the post-condition ("the store does not pin this key") already holds.
	DeleteOverride(key string) error
}

// SecretStore is the write-only credential port (ADR-0101 D5). Deliberately
// separate from Store and deliberately without a getter: there is no method here
// that returns a secret value, so no read path can be made to leak one by
// forgetting a filter.
//
// The kernel's own consumers resolve secrets through Resolve, which is not on
// this interface — see storage.BoltSecretStore.Resolve. Nothing on the operator
// plane can reach it.
type SecretStore interface {
	// SetSecret stores a credential under a logical name
	// ("generator:<id>:api_key", "telemetry:secret_key", "telegram:bot_token").
	SetSecret(name, value string) error

	// ClearSecret removes a credential. Clearing an absent secret is not an error.
	ClearSecret(name string) error

	// Configured reports whether a credential is stored under name. It answers
	// the console's "key set / not set" without carrying the value.
	Configured(name string) bool

	// LastFour returns the last four characters of the stored credential, or ""
	// when none is stored. Four characters identify which key is installed
	// without being enough to use it — the same trade every provider dashboard
	// makes.
	LastFour(name string) string
}

// expand turns flat dotted keys into the nested map shape a JSON parser
// produces, so the store layer can be merged through exactly the same
// rawbytes+JSON path as the defaults layer. Doing it this way means the store
// adds NO new Koanf provider dependency and cannot merge by different rules
// than the layers around it.
//
// A key whose prefix collides with an existing scalar is skipped rather than
// silently reshaping the tree: {"a": 1} plus "a.b" = 2 has no coherent merge,
// and guessing one would corrupt a neighbouring value.
//
// THE SCALAR WINS, and it wins deterministically. Keys are processed
// shallowest-first (then lexically) rather than in Go map order, because map
// iteration is randomised: {"a": 1, "a.b": 2} previously produced {"a":1} or
// {"a":{"b":2}} depending on which key the runtime happened to visit first, so
// the same config file could expand two different ways in the same process. The
// collision rule was already written down — this makes the code actually obey it
// every time instead of roughly half the time.
//
// Shallowest-first is what enforces it: "a" is placed before "a.b" is considered,
// so the deeper key hits the existing scalar and is skipped by the prefix walk
// below. Sorting also makes the output stable, which is what lets a caller diff
// two expansions and believe the result.
func expand(flat map[string]any) map[string]any {
	keys := make([]string, 0, len(flat))
	for k := range flat {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		di, dj := strings.Count(keys[i], "."), strings.Count(keys[j], ".")
		if di != dj {
			return di < dj
		}
		return keys[i] < keys[j]
	})

	out := map[string]any{}
	for _, key := range keys {
		val := flat[key]
		parts := strings.Split(key, ".")
		cur := out
		ok := true
		for i, p := range parts[:len(parts)-1] {
			next, exists := cur[p]
			if !exists {
				m := map[string]any{}
				cur[p] = m
				cur = m
				continue
			}
			m, isMap := next.(map[string]any)
			if !isMap {
				_ = i
				ok = false
				break
			}
			cur = m
		}
		if !ok {
			continue
		}
		leaf := parts[len(parts)-1]
		if _, exists := cur[leaf]; exists {
			if _, isMap := cur[leaf].(map[string]any); isMap {
				continue // a scalar must not overwrite a subtree
			}
		}
		cur[leaf] = val
	}
	return out
}
