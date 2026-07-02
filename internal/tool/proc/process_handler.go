// Package proc runs system tools as confined, kernel-spawned Python child
// processes (ADR-0039 A1.2). By the time a tool is invoked the kernel's
// ToolExecutor has already authorized it; this handler only executes the
// already-blessed operation, with defense-in-depth confinement: a per-call
// tempdir cwd jail, a deny-by-default scrubbed env, and a hard timeout that
// kills the child (bounding blast radius). OS resource caps (rlimit/cgroup) are
// applied on Unix and gracefully degraded elsewhere.
package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ProcessHandler invokes Python tool modules. It implements domain.ToolHandler.
type ProcessHandler struct {
	// PythonExec is the interpreter (e.g. the repo .venv python).
	PythonExec string
	// ToolFiles maps a tool name to its *tool.py path (from discovery).
	ToolFiles map[string]string
	// DefaultTimeout caps each invocation; the child is killed on expiry.
	DefaultTimeout time.Duration
	// EnvPassthrough is the deny-by-default allowlist of env var names that may
	// reach the tool process (Hermes #27303-style hardening). PATH is always kept.
	EnvPassthrough []string
	// ContentStore, when set, receives any files a tool writes into its per-call
	// tempdir jail (relative-path writes that would otherwise be lost to
	// os.RemoveAll). After a successful run the jail is swept and each created
	// file is offloaded to CAS; the resulting CIDs are surfaced to the agent in
	// the result JSON under "_artifacts". nil ⇒ no sweep (files in the jail are
	// discarded with it, the original behaviour).
	ContentStore domain.ContentStore
}

// artifactRef is one file swept from a tool's jail into CAS, surfaced to the
// agent so the persisted output is retrievable by CID.
type artifactRef struct {
	Path  string `json:"path"`  // path relative to the jail (the name the tool wrote)
	CID   string `json:"cid"`   // ContentStore handle to the durable copy
	Bytes int    `json:"bytes"` // size of the written file
}

// toolInput is the stdin contract: the tool receives its name and authorized args.
type toolInput struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// Execute runs the tool's Python module in a confined child and returns its
// stdout (the result JSON). A non-zero exit, timeout, or unknown tool is an error.
func (h *ProcessHandler) Execute(ctx context.Context, call domain.ToolCall) ([]byte, error) {
	file, ok := h.ToolFiles[call.ToolName]
	if !ok {
		return nil, fmt.Errorf("tool %q has no registered handler module", call.ToolName)
	}
	if h.PythonExec == "" {
		return nil, fmt.Errorf("no python interpreter configured for tool execution")
	}

	// Resolve the tool module to an absolute path BEFORE jailing the child's cwd
	// to the per-call tempdir below. Discovery may hand us a relative path (e.g.
	// "tools/web_tool.py" when the orchestrator is started with a relative tools
	// dir); once cmd.Dir is the jail, Python would resolve that relative path
	// against the jail and fail with ENOENT. filepath.Abs anchors it to the
	// orchestrator's cwd — where the relative tool path actually lives.
	if !filepath.IsAbs(file) {
		abs, aerr := filepath.Abs(file)
		if aerr != nil {
			return nil, fmt.Errorf("tool %q: resolve module path %q: %w", call.ToolName, file, aerr)
		}
		file = abs
	}

	timeout := h.DefaultTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Per-call tempdir cwd jail.
	jail, err := os.MkdirTemp("", "cambrian-tool-")
	if err != nil {
		return nil, fmt.Errorf("tool jail: %w", err)
	}
	defer os.RemoveAll(jail)

	args := call.ArgsJSON
	if len(args) == 0 {
		args = []byte("{}")
	}
	stdin, _ := json.Marshal(toolInput{Tool: call.ToolName, Args: args})

	cmd := exec.CommandContext(runCtx, h.PythonExec, file)
	cmd.Dir = jail
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = h.scrubbedEnv(jail)
	applyResourceCaps(cmd) // Unix rlimit; no-op elsewhere

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("tool %q timed out after %s", call.ToolName, timeout)
		}
		return nil, fmt.Errorf("tool %q failed: %w: %s", call.ToolName, err, truncate(stderr.String(), 500))
	}

	result := stdout.Bytes()
	// Sweep any files the tool created in its jail into CAS before the deferred
	// os.RemoveAll discards them — otherwise a relative-path write (e.g.
	// write_file "hello.txt") resolves against the jail cwd and is silently lost.
	// Runs before the deferred RemoveAll/cancel (defers fire after the body).
	if h.ContentStore != nil {
		if refs := h.sweepJail(ctx, jail, call.ToolName); len(refs) > 0 {
			result = mergeArtifacts(result, refs)
		}
	}
	return result, nil
}

// sweepJail offloads every file the tool wrote into its jail to CAS. The jail is
// created empty per call, so any file present afterwards is a tool output.
// Best-effort: a read or CAS error for one file is logged and skipped, never
// failing the (already successful) tool call.
func (h *ProcessHandler) sweepJail(ctx context.Context, jail, toolName string) []artifactRef {
	var refs []artifactRef
	_ = filepath.Walk(jail, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			slog.Warn("tool artifact sweep: read failed", "tool", toolName, "path", p, "err", rerr)
			return nil
		}
		rel, _ := filepath.Rel(jail, p)
		rel = filepath.ToSlash(rel)
		cid, perr := h.ContentStore.Put(ctx, data, "tool_artifact", []string{"tool_artifact", toolName, rel}, snippetOf(data))
		if perr != nil {
			slog.Warn("tool artifact sweep: CAS put failed", "tool", toolName, "path", rel, "err", perr)
			return nil
		}
		refs = append(refs, artifactRef{Path: rel, CID: string(cid), Bytes: len(data)})
		return nil
	})
	return refs
}

// mergeArtifacts injects an "_artifacts" array into a JSON-object result so the
// agent learns where its written files now live. If the result is not a JSON
// object it is returned unchanged (the artifacts are still persisted in CAS).
func mergeArtifacts(result []byte, refs []artifactRef) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil || obj == nil {
		return result
	}
	enc, err := json.Marshal(refs)
	if err != nil {
		return result
	}
	obj["_artifacts"] = enc
	merged, err := json.Marshal(obj)
	if err != nil {
		return result
	}
	return merged
}

// snippetOf returns a UTF-8-safe resilience snippet (≤500 bytes) for CAS, or ""
// for binary content. Truncation backs off to a rune boundary.
func snippetOf(data []byte) string {
	const max = 500
	if !utf8.Valid(data) {
		return ""
	}
	if len(data) <= max {
		return string(data)
	}
	s := data[:max]
	for len(s) > 0 && !utf8.Valid(s) {
		s = s[:len(s)-1]
	}
	return string(s)
}

// scrubbedEnv builds a deny-by-default environment: PATH (needed to run), a
// per-call TMPDIR pointing at the jail, plus the explicit passthrough allowlist.
func (h *ProcessHandler) scrubbedEnv(jail string) []string {
	keep := map[string]bool{"PATH": true, "SYSTEMROOT": true, "TEMP": true, "TMP": true}
	for _, k := range h.EnvPassthrough {
		keep[k] = true
	}
	env := []string{"TMPDIR=" + jail}
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				if keep[kv[:i]] {
					env = append(env, kv)
				}
				break
			}
		}
	}
	return env
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
