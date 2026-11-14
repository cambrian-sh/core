package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// REGRESSION (T-20261103-config-loader-silently-ignores-unknown-removed): a key a file
// sets that no Config field reads — a typo, a retired key — loaded silently. It must
// still load (no error, no behaviour change) but be named by unknownConfigKeys, while
// "_comment" keys, env-only CAMBRIAN_* variables and the defaults are not reported.
func TestLoadMerged_ReportsUnknownFileKeysOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
  "_comment": "shipped files carry comments",
  "telemetry": {"trace_sampling_rate": 0.5, "metrics_exporter_typo": "x"},
  "execution": {"retrieval": {"recall_top_k": 7, "max_per_document": 3}},
  "no_such_block": {"a": 1}
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not config at all, but in the CAMBRIAN_ namespace: must not be reported.
	t.Setenv("CAMBRIAN_PYTHON", "/usr/bin/python3")

	cfg, prov, unused, err := loadMergedReport(path, nil)
	if err != nil {
		t.Fatalf("an unknown key must never fail the load: %v", err)
	}
	if cfg.Execution.Retrieval.RecallTopK != 7 {
		t.Fatalf("known keys must still apply, recall_top_k = %d", cfg.Execution.Retrieval.RecallTopK)
	}
	got := strings.Join(unknownConfigKeys(unused, prov), "|")
	for _, want := range []string{
		"execution.retrieval.max_per_document (config.json)",
		"no_such_block (config.json)", // an unknown block is reported once, at its root
		"telemetry.metrics_exporter_typo (config.json)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unknown keys %q missing %q", got, want)
		}
	}
	for _, notWant := range []string{"_comment", "python", "trace_sampling_rate", "recall_top_k"} {
		if strings.Contains(got, notWant) {
			t.Errorf("unknown keys %q must not include %q", got, notWant)
		}
	}
}

// The Go defaults round-trip through JSON into the merge; none of them may be reported
// as unknown, or every boot would warn about the kernel's own defaults.
func TestLoadMerged_DefaultsAndShippedFilesHaveNoUnknownKeys(t *testing.T) {
	_, prov, unused, err := loadMergedReport(filepath.Join(t.TempDir(), "config.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := unknownConfigKeys(unused, prov); len(got) != 0 {
		t.Fatalf("defaults-only load reports unknown keys: %v", got)
	}
	// The committed starter files must be clean too (config.example.json and the
	// *.example files are what operators copy).
	for _, name := range []string{"config.example.json", "tuning.json"} {
		src := filepath.Join("..", "..", "configs", name)
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		_, prov, unused, err := loadMergedReport(filepath.Join(dir, "config.json"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := unknownConfigKeys(unused, prov); len(got) != 0 {
			t.Errorf("%s has keys this kernel does not read: %v", name, got)
		}
	}
}

func TestSourceOf_ListElementResolvesThroughItsList(t *testing.T) {
	t.Parallel()
	prov := Provenance{"llm_provider.generators": SourceProviders}
	if src, ok := sourceOf("llm_provider.generators[0].bogus", prov); !ok || src != SourceProviders {
		t.Fatalf("sourceOf = %q, %v", src, ok)
	}
}
