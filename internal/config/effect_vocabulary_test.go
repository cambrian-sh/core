package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Effect-checked dispatch (T-20261106-kernel-does-not-check-that-a-declared): the
// step-effect vocabulary is config data, execution.capability.effect_vocabulary.

func TestDefaultEffectVocabulary_IsValidAndMapsTheEffectClasses(t *testing.T) {
	v, err := DefaultConfig().Execution.Capability.StepEffectVocabulary()
	if err != nil {
		t.Fatalf("the shipped vocabulary is invalid: %v", err)
	}
	want := []string{"read", "write", "move", "delete", "execute", "egress", "send", "spend", "admin"}
	if got := v.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("vocabulary = %v, want %v", got, want)
	}
}

// The vocabulary is loaded from a config file (the file REPLACES the list), and an
// invalid one is refused at load rather than silently reaching dispatch.
func TestLoadConfig_EffectVocabularyFromFile(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"schema_version":2,"execution":{"capability":{"effect_vocabulary":[
		{"name":"read","classes":["read"],"description":"look"},
		{"name":"archive","refines":"write","tools":["*archive*"],"description":"archive a record"}]}}}`)
	cfg, _ := loadFrom(t, dir, nil)
	v, err := cfg.Execution.Capability.StepEffectVocabulary()
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Names(); !reflect.DeepEqual(got, []string{"read", "archive"}) {
		t.Fatalf("file vocabulary did not reach the field: %v", got)
	}

	dir = baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"schema_version":2,"execution":{"capability":{"effect_vocabulary":[
		{"name":"move","refines":"teleport","tools":["*move*"]}]}}}`)
	_, err = LoadConfig(filepath.Join(dir, "config.json"))
	if err == nil || !strings.Contains(err.Error(), "effect_vocabulary") {
		t.Fatalf("an invalid vocabulary must be refused at load, got %v", err)
	}
}
