package config

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// ENG-1421 (owner decision 2026-11-19): execution.tools.tool_effects_strict
// governed only the native tools/*tool.py discovery, deleted because no such
// tool ever shipped. A deployment that still sets it — in a file, the ADR-0101
// store or a CAMBRIAN_* variable — keeps starting: the key is dropped with the
// retirement WARN and the same layer's other tools keys are honoured.
func TestRetiredToolEffectsStrict_EveryLayerDropsItWithAWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	retiredLogged.Range(func(k, _ any) bool { retiredLogged.Delete(k); return true })

	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"schema_version": 4, "execution": {"tools": {
	  "tool_effects_strict": true, "tools_auto_approve": true}}}`)
	t.Setenv("CAMBRIAN_EXECUTION__TOOLS__TOOL_EFFECTS_STRICT", "true")
	store := mapStore{"execution.tools.tool_effects_strict": true}

	cfg, prov, unused, err := loadMergedReport(filepath.Join(dir, "config.json"), store)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a config carrying the retired key must still validate: %v", err)
	}
	if !cfg.Execution.Tools.ToolsAutoApprove {
		t.Error("the same layer's other tools key was lost")
	}
	if src, ok := prov["execution.tools.tool_effects_strict"]; ok && src != SourceDefault {
		t.Errorf("retired key still claimed by %s", src)
	}
	for _, k := range unused {
		if strings.HasSuffix(k, "tool_effects_strict") {
			t.Errorf("retired key reached the decoder (unused %q)", k)
		}
	}
	if logs := buf.String(); !strings.Contains(logs, "execution.tools.tool_effects_strict") || !strings.Contains(logs, "ENG-1421") {
		t.Errorf("no retirement WARN naming the key and its reason:\n%s", logs)
	}
}
