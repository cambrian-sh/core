package proc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/tool/discovery"
)

func loadRealTools(t *testing.T) (*ProcessHandler, string) {
	t.Helper()
	py := pythonOrSkip(t)
	root := repoRoot()
	if root == "" {
		t.Skip("repo root not found")
	}
	reg := domain.NewInMemoryToolRegistry()
	files, err := discovery.LoadRegistry(filepath.Join(root, "tools"), reg)
	if err != nil {
		t.Fatalf("discover tools: %v", err)
	}
	return &ProcessHandler{PythonExec: py, ToolFiles: files, DefaultTimeout: 15 * time.Second}, root
}

// Capability-unlock demo (DoD): the real terminal tool runs an actual command and
// returns its real stdout. Mutation-proof — gut the tool, this fails.
func TestTerminalTool_RealCommand(t *testing.T) {
	h, _ := loadRealTools(t)
	args, _ := json.Marshal(map[string]string{"command": "echo cambrian-tools-work"})
	out, err := h.Execute(context.Background(), domain.ToolCall{ToolName: "execute_command", ArgsJSON: args})
	if err != nil {
		t.Fatalf("execute_command: %v", err)
	}
	var res map[string]any
	if jerr := json.Unmarshal(out, &res); jerr != nil {
		t.Fatalf("not JSON: %v (%s)", jerr, out)
	}
	if !strings.Contains(res["stdout"].(string), "cambrian-tools-work") {
		t.Errorf("terminal stdout missing expected output: %v", res)
	}
}

// Capability-unlock demo (DoD): the real code-exec tool runs real Python and
// returns its real stdout (only stdout, the Hermes-mirrored contract).
func TestCodeExecTool_RealPython(t *testing.T) {
	h, _ := loadRealTools(t)
	args, _ := json.Marshal(map[string]string{"code": "print(6 * 7)"})
	out, err := h.Execute(context.Background(), domain.ToolCall{ToolName: "execute_python", ArgsJSON: args})
	if err != nil {
		t.Fatalf("execute_python: %v", err)
	}
	var res map[string]any
	_ = json.Unmarshal(out, &res)
	if strings.TrimSpace(res["stdout"].(string)) != "42" {
		t.Errorf("code-exec stdout = %v, want 42", res["stdout"])
	}
}
