package config

import (
	"bytes"
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzLoadConfigJSON drives the operator-facing config loader with arbitrary
// bytes as configs/config.json. Two properties:
//
//  1. Neither the v1→v2 migration (migrateJSON) nor LoadConfig may panic on any
//     input — a malformed file must surface as an error, never as a crash at
//     boot.
//  2. The migration is idempotent: migrating already-migrated bytes returns them
//     unchanged. That is what "loading never rewrites an operator's file" rests
//     on — a second boot must see the same document the first one did.
//
// The file lives in its own temp dir so no sibling layer (tuning.json,
// providers.json, ...) leaks in, and the primary path is the only input.
func FuzzLoadConfigJSON(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"schema_version": 2}`))
	f.Add([]byte(`{"schema_version": 1, "execution": {"recall_top_k": 5, "plan_timeout_ms": 1000}}`))
	f.Add([]byte(`{"execution": {"plan": 5, "plan_timeout_ms": 1}}`)) // block already taken by a scalar
	f.Add([]byte(`{"execution": {"plan": {"plan_timeout_ms": 7}, "plan_timeout_ms": 1}}`))
	f.Add([]byte(`{"execution": []}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))
	f.Add([]byte(`{"execution": {"max_recursion_depth": "not a number"}}`))
	if defaults, err := stdjson.Marshal(DefaultConfig()); err == nil {
		f.Add(defaults)
	}
	// The frozen v1 schema snapshot wrapped as a legacy file: the migration's
	// hardest real input.
	if raw, err := os.ReadFile(legacyGoldenPath); err == nil {
		f.Add([]byte(`{"execution": ` + string(raw) + `}`))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		once := migrateJSON(data, "fuzz")
		twice := migrateJSON(once, "fuzz-again")
		if !bytes.Equal(once, twice) {
			t.Fatalf("migration is not idempotent:\n first=%s\nsecond=%s", once, twice)
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		// Must not panic. An error is a legitimate answer for garbage input.
		_, _ = LoadConfig(path)
	})
}
