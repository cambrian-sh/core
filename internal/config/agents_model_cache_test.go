package config

import (
	"path/filepath"
	"slices"
	"testing"
)

// ADR-0157 S7: model_cache_dir resolves against the kernel base directory
// ("" ⇒ runtime/models) and points the model libraries at one cache.
func TestModelCacheDirAndEnv(t *testing.T) {
	base := filepath.Join(t.TempDir(), "k")
	abs := filepath.Join(t.TempDir(), "models")
	for in, want := range map[string]string{
		"":        filepath.Join(base, "runtime", "models"),
		"  ":      filepath.Join(base, "runtime", "models"),
		"m/cache": filepath.Join(base, "m", "cache"),
		abs:       abs,
	} {
		if got := (AgentsConfig{ModelCacheDir: in}).ResolvedModelCacheDir(base); got != want {
			t.Errorf("model_cache_dir %q => %q, want %q", in, got, want)
		}
	}
	env := ModelCacheEnv(abs)
	for _, want := range []string{"HF_HOME=" + filepath.Join(abs, "huggingface"), "TORCH_HOME=" + filepath.Join(abs, "torch"),
		"DOCLING_CACHE_DIR=" + filepath.Join(abs, "docling")} {
		if !slices.Contains(env, want) {
			t.Errorf("env %v lacks %s", env, want)
		}
	}
	if ModelCacheEnv("") != nil {
		t.Error("no cache must set no variable")
	}
	if DefaultConfig().Execution.Agents.ModelCacheDir != "" {
		t.Error("the default model_cache_dir is empty (the base's runtime/models)")
	}
}
