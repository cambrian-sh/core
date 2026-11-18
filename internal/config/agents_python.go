package config

import (
	"bytes"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AgentPythonConfig is execution.agents.python: the kernel-managed Python
// environment sandboxed agents run on (ADR-0157 D7, Amendment A1; owner
// decisions 2026-11-18 on ENG-1993, A1-Q14: a block of its own, not nested
// under execution.agents.sandbox, because it applies with the sandbox on or off).
//
// The Python version and every digest are in the kernel's embedded pin table
// (internal/pyenv/pins.json), never here. P1 (ENG-2007) adds the keys the env
// builder reads; P3 (ENG-2009) adds approval_ttl_hours; request_wait_seconds
// and gc arrive with the stages that read them (P4-P5), so no key is parsed
// without a consumer (AGENTS.md §3.4). allow_sdists is not added: wheels only
// is fixed, because a source build would run arbitrary code in the kernel
// (A1-Q7).
type AgentPythonConfig struct {
	// Root is the env root; relative paths resolve against the kernel base
	// directory. "" ⇒ <kernel base dir>/runtime/python (D7.1.1).
	Root string `json:"root,omitempty"`
	// BuildOnBoot: when the boot check finds the env missing, broken or built
	// from an older release, build it in the background; Python agents wait for
	// it, the rest of the kernel does not (owner A1-Q12). Default true.
	BuildOnBoot bool `json:"build_on_boot"`
	// PythonMirrorURL serves the pinned interpreter archives under their
	// upstream names (https:// or file://). "" ⇒ the upstream release URL.
	PythonMirrorURL string `json:"python_mirror_url,omitempty"`
	// InstallerMirrorURL does the same for the pinned installer (uv).
	InstallerMirrorURL string `json:"installer_mirror_url,omitempty"`
	// IndexURL is the package index (https:// or file://). "" ⇒ https://pypi.org/simple.
	IndexURL string `json:"index_url,omitempty"`
	// Wheelhouse lists local directories (or file:// URLs) of pre-downloaded
	// wheels; relative paths resolve against the config directory and may not
	// leave it.
	Wheelhouse []string `json:"wheelhouse,omitempty"`
	// Offline: no network at all for env work — archives must be in
	// <root>/downloads or behind a file:// mirror, wheels come only from the
	// wheelhouse.
	Offline bool `json:"offline,omitempty"`
	// ApprovalTTLHours bounds an unanswered python_package approval card
	// (ADR-0157 D7.8.7, P3): it then expires fail-closed and the kernel asks
	// again at the agent's next spawn. 0 ⇒ 72.
	ApprovalTTLHours int `json:"approval_ttl_hours,omitempty"`
}

// ApprovalTTL is approval_ttl_hours as a duration (0 ⇒ 72 h).
func (c AgentPythonConfig) ApprovalTTL() time.Duration {
	if c.ApprovalTTLHours <= 0 {
		return 72 * time.Hour
	}
	return time.Duration(c.ApprovalTTLHours) * time.Hour
}

// validate checks execution.agents.python (D7.15).
func (c AgentPythonConfig) validate() []string {
	var errs []string
	for _, f := range []struct{ key, v string }{
		{"python_mirror_url", c.PythonMirrorURL},
		{"installer_mirror_url", c.InstallerMirrorURL},
		{"index_url", c.IndexURL},
	} {
		if f.v == "" {
			continue
		}
		u, err := url.Parse(f.v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "file") || (u.Scheme == "https" && u.Host == "") {
			errs = append(errs, fmt.Sprintf("execution.agents.python.%s must be an https:// or file:// URL (got %q)", f.key, f.v))
		}
	}
	for _, w := range c.Wheelhouse {
		switch {
		case strings.TrimSpace(w) == "":
			errs = append(errs, "execution.agents.python.wheelhouse must not contain an empty path")
		case strings.HasPrefix(w, "file://"), filepath.IsAbs(w):
		case strings.Contains(w, "://"):
			errs = append(errs, fmt.Sprintf("execution.agents.python.wheelhouse entries are local directories or file:// URLs (got %q)", w))
		default:
			if rel := filepath.Clean(w); rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.HasPrefix(filepath.ToSlash(rel), "../") {
				errs = append(errs, fmt.Sprintf("execution.agents.python.wheelhouse relative path %q leaves the config directory; use an absolute path", w))
			}
		}
	}
	if c.ApprovalTTLHours < 0 {
		errs = append(errs, fmt.Sprintf("execution.agents.python.approval_ttl_hours must be 0 (default 72) or positive (got %d)", c.ApprovalTTLHours))
	}
	if c.Offline && len(c.Wheelhouse) == 0 {
		errs = append(errs, "execution.agents.python.offline requires execution.agents.python.wheelhouse: offline wheels come only from a local wheelhouse")
	}
	return errs
}

// Validate is validate for callers outside the full config load (setup step 4
// and the python-env CLI read only this block from configs/config.json).
func (c AgentPythonConfig) Validate() error {
	if errs := c.validate(); len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// ReadAgentPythonBlock reads execution.agents.python from one config file over
// its defaults, without the rest of the config load: setup runs step 4 before
// the config bundle is complete, and an air-gapped operator writes this block
// first. A missing file yields the defaults.
func ReadAgentPythonBlock(configPath string) (AgentPythonConfig, error) {
	def := DefaultConfig().Execution.Agents.Python
	b, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return def, nil
	}
	if err != nil {
		return def, err
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	var doc struct {
		Execution struct {
			Agents struct {
				Python *stdjson.RawMessage `json:"python"`
			} `json:"agents"`
		} `json:"execution"`
	}
	if err := stdjson.Unmarshal(b, &doc); err != nil {
		return def, fmt.Errorf("%s: %w", configPath, err)
	}
	out := def
	if doc.Execution.Agents.Python != nil {
		if err := stdjson.Unmarshal(*doc.Execution.Agents.Python, &out); err != nil {
			return def, fmt.Errorf("%s: execution.agents.python: %w", configPath, err)
		}
	}
	return out, out.Validate()
}

// ResolvedRoot is Root anchored at the kernel base directory.
func (c AgentPythonConfig) ResolvedRoot(baseDir string) string {
	switch {
	case c.Root == "":
		return filepath.Join(baseDir, "runtime", "python")
	case filepath.IsAbs(c.Root):
		return c.Root
	}
	return filepath.Join(baseDir, c.Root)
}

// ResolvedWheelhouse anchors relative wheelhouse entries at the config directory.
func (c AgentPythonConfig) ResolvedWheelhouse(configDir string) []string {
	out := make([]string, 0, len(c.Wheelhouse))
	for _, w := range c.Wheelhouse {
		if !strings.HasPrefix(w, "file://") && !filepath.IsAbs(w) {
			w = filepath.Join(configDir, w)
		}
		out = append(out, w)
	}
	return out
}
