package app

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-257 (contract 0104): FileOverrides names, per catalogued tunable, the file a
// stored value overrides and that file's value; uncatalogued keys, keys no file
// states and secret values are left out.
func TestConfigSchema_FileOverrides(t *testing.T) {
	src := configSchemaSource{overrides: func() []config.StoreOverride {
		return []config.StoreOverride{
			{Key: "execution.ewma_alpha", StoreValue: 0.5, File: "tuning.json", FileValue: 0.3, Differs: true},
			{Key: "execution.gatekeeper_max_candidates", StoreValue: 9.0}, // no file states it
			{Key: "llm_provider.generators", StoreValue: "x", File: "config.json", FileValue: "y"},
			{Key: "execution.memory_relevance_threshold", File: "config.json", FileValue: "s", Secret: true},
		}
	}}
	files, values := src.FileOverrides()
	if files["execution.ewma_alpha"] != "tuning.json" || values["execution.ewma_alpha"] != "0.3" {
		t.Fatalf("files=%v values=%v", files, values)
	}
	if _, ok := files["execution.gatekeeper_max_candidates"]; ok {
		t.Fatal("a key no file states must be absent")
	}
	if _, ok := files["llm_provider.generators"]; ok {
		t.Fatal("an uncatalogued key must be absent")
	}
	if _, ok := values["execution.memory_relevance_threshold"]; ok || files["execution.memory_relevance_threshold"] != "config.json" {
		t.Fatalf("secret: file named, value never shown: files=%v values=%v", files, values)
	}
	if f, v := (configSchemaSource{}).FileOverrides(); f != nil || v != nil {
		t.Fatal("no store ⇒ no overrides")
	}
}
