package proc

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

func pythonOrSkip(t *testing.T) string {
	t.Helper()
	// Prefer the repo .venv python (the Windows box only has the Store alias stub
	// on PATH, which exits 9009). Walk up to the module root and look for .venv.
	if root := repoRoot(); root != "" {
		for _, rel := range []string{
			filepath.Join(".venv", "Scripts", "python.exe"),
			filepath.Join(".venv", "bin", "python3"),
			filepath.Join(".venv", "bin", "python"),
		} {
			p := filepath.Join(root, rel)
			if isRealPython(p) {
				return p
			}
		}
	}
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil && isRealPython(p) {
			return p
		}
	}
	t.Skip("no real python interpreter available (Store stub / none)")
	return ""
}

func repoRoot() string {
	dir, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func isRealPython(p string) bool {
	if _, err := os.Stat(p); err != nil && filepath.IsAbs(p) {
		return false
	}
	out, err := exec.Command(p, "--version").CombinedOutput()
	return err == nil && bytesHasPrefix(out, "Python ")
}

func bytesHasPrefix(b []byte, s string) bool {
	return len(b) >= len(s) && string(b[:len(s)]) == s
}

// A tool file reads {tool, args} JSON on stdin and writes its result JSON on
// stdout. echo_tool reports the args and whether a secret env var leaked in.
const echoTool = `import sys, json, os
d = json.load(sys.stdin)
print(json.dumps({"echo": d["args"], "has_secret": "SECRET_TOKEN" in os.environ}))
`

func newHandler(t *testing.T, py string) (*ProcessHandler, string) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "echo_tool.py"), []byte(echoTool), 0o644); err != nil {
		t.Fatal(err)
	}
	return &ProcessHandler{
		PythonExec:     py,
		ToolFiles:      map[string]string{"echo": filepath.Join(dir, "echo_tool.py")},
		DefaultTimeout: 5 * time.Second,
		EnvPassthrough: nil, // deny-by-default
	}, dir
}

func TestProcessHandler_RunsToolAndReturnsResult(t *testing.T) {
	py := pythonOrSkip(t)
	h, _ := newHandler(t, py)

	out, err := h.Execute(context.Background(), domain.ToolCall{ToolName: "echo", ArgsJSON: []byte(`{"x":42}`)})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var res map[string]any
	if jerr := json.Unmarshal(out, &res); jerr != nil {
		t.Fatalf("result not JSON: %v (%s)", jerr, out)
	}
	echo, _ := res["echo"].(map[string]any)
	if echo["x"].(float64) != 42 {
		t.Errorf("args did not round-trip: %v", res)
	}
}

// Deny-by-default env scrub (Hermes #27303): a non-passthrough secret in the
// parent env must NOT reach the tool process.
func TestProcessHandler_EnvScrubbed(t *testing.T) {
	py := pythonOrSkip(t)
	t.Setenv("SECRET_TOKEN", "leak-me")
	h, _ := newHandler(t, py)

	out, err := h.Execute(context.Background(), domain.ToolCall{ToolName: "echo", ArgsJSON: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var res map[string]any
	_ = json.Unmarshal(out, &res)
	if res["has_secret"] == true {
		t.Error("SECRET_TOKEN leaked into the tool process; env scrub failed")
	}
}

// A runaway tool is killed by the timeout; the call returns an error and does
// not hang.
func TestProcessHandler_TimeoutKills(t *testing.T) {
	py := pythonOrSkip(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "sleep_tool.py"), []byte("import time\ntime.sleep(30)\n"), 0o644)
	h := &ProcessHandler{
		PythonExec:     py,
		ToolFiles:      map[string]string{"sleep": filepath.Join(dir, "sleep_tool.py")},
		DefaultTimeout: 150 * time.Millisecond,
	}
	start := time.Now()
	_, err := h.Execute(context.Background(), domain.ToolCall{ToolName: "sleep", ArgsJSON: []byte(`{}`)})
	if err == nil {
		t.Error("a timed-out tool should return an error")
	}
	if time.Since(start) > 3*time.Second {
		t.Error("timeout did not kill the process promptly")
	}
}

// Regression: a RELATIVE tool path — what discovery stores when the orchestrator
// is started with a relative tools dir (production `LoadRegistry("tools", …)`) —
// must still run, even though Execute jails the child's cwd to a tempdir. Before
// the fix the child resolved the relative path against the jail and failed with
// ENOENT ("can't open file '…\\Temp\\cambrian-tool-XXX\\tools\\web_tool.py'").
func TestProcessHandler_RelativeToolPathResolves(t *testing.T) {
	py := pythonOrSkip(t)
	dir := t.TempDir()
	abs := filepath.Join(dir, "echo_tool.py")
	if err := os.WriteFile(abs, []byte(echoTool), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || filepath.IsAbs(rel) {
		t.Skip("tool path could not be made relative on this volume")
	}
	h := &ProcessHandler{
		PythonExec:     py,
		ToolFiles:      map[string]string{"echo": rel}, // relative, like production discovery
		DefaultTimeout: 5 * time.Second,
	}
	out, err := h.Execute(context.Background(), domain.ToolCall{ToolName: "echo", ArgsJSON: []byte(`{"x":7}`)})
	if err != nil {
		t.Fatalf("relative tool path must resolve despite the jailed cwd, got: %v", err)
	}
	var res map[string]any
	_ = json.Unmarshal(out, &res)
	echo, _ := res["echo"].(map[string]any)
	if echo == nil || echo["x"].(float64) != 7 {
		t.Errorf("args did not round-trip via relative path: %v", res)
	}
}

var _ domain.ToolHandler = (*ProcessHandler)(nil)
