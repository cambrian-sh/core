package config

import (
	"bytes"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The shared model cache of kernel-spawned agents (ADR-0157 S7; owner decision
// 2026-11-17 on ENG-500: "S7 ships a writable shared model-cache path for the
// organs by default"). The organs (docling, the reranker, anything built on
// Hugging Face or torch) download model weights on first use. A sandboxed agent's
// HOME is its private scratch, which is deleted when the instance exits, so
// without a shared cache every spawn would download again — and under
// network: kernel_only_all could not download at all.
//
// The kernel therefore owns one cache directory and SETS the libraries' cache
// variables to it in every agent's environment (HF_HOME, TORCH_HOME,
// DOCLING_CACHE_DIR), with the sandbox on or off — one pathway, the same one
// setup's model pre-fetch writes into. With the sandbox on, the directory is
// shown to every agent at its own path: writable for first-party and system
// agents, read-only for third-party agents (a third-party agent could otherwise
// plant weights a first-party organ later loads).
//
// Rejected: carrying HF_HOME through agent_env_passthrough, as first sketched.
// procenv strips every HF_* name as secret-looking (it covers HF_TOKEN), and the
// owner's 2026-08-11 rule keeps passthrough empty because a knob must never
// depend on the kernel's own environment; the kernel sets config-derived agent
// variables itself (envExtras), as it does for CAMBRIAN_TOOL_MENU_K. Rejected
// too: a manifest-declared sandbox_paths entry per organ — a static manifest
// cannot name a per-install path, and the cache is kernel-owned, so it needs no
// allowlist_cap entry.

// DefaultModelCacheSubdir is the cache's place under the kernel base directory
// when execution.agents.model_cache_dir is empty, beside the managed Python env
// (runtime/python, ADR-0157 D7.1.1).
const DefaultModelCacheSubdir = "runtime/models"

// ResolvedModelCacheDir is model_cache_dir anchored at the kernel base
// directory ("" ⇒ <base>/runtime/models).
func (c AgentsConfig) ResolvedModelCacheDir(baseDir string) string {
	d := strings.TrimSpace(c.ModelCacheDir)
	switch {
	case d == "":
		return filepath.Join(baseDir, filepath.FromSlash(DefaultModelCacheSubdir))
	case filepath.IsAbs(d):
		return filepath.Clean(d)
	}
	return filepath.Join(baseDir, d)
}

// ModelCacheEnv is the environment that points the model libraries at dir:
// Hugging Face (hub, transformers, sentence-transformers), torch hub and
// docling's own cache. Empty when dir is empty.
func ModelCacheEnv(dir string) []string {
	if dir == "" {
		return nil
	}
	return []string{
		"HF_HOME=" + filepath.Join(dir, "huggingface"),
		"TORCH_HOME=" + filepath.Join(dir, "torch"),
		"DOCLING_CACHE_DIR=" + filepath.Join(dir, "docling"),
	}
}

// ReadModelCacheDir reads execution.agents.model_cache_dir from one config file
// without the full layered load, for setup's model pre-fetch (it runs before a
// kernel exists, like ReadAgentPythonBlock). A missing file or key is "".
func ReadModelCacheDir(configPath string) (string, error) {
	b, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var doc struct {
		Execution struct {
			Agents struct {
				ModelCacheDir string `json:"model_cache_dir"`
			} `json:"agents"`
		} `json:"execution"`
	}
	if err := stdjson.Unmarshal(bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}), &doc); err != nil {
		return "", fmt.Errorf("%s: %w", configPath, err)
	}
	return doc.Execution.Agents.ModelCacheDir, nil
}
