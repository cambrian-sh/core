package discovery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func writeTool(t *testing.T, dir, filename, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Tools are auto-discovered from tools/*tool.py TOOL_MANIFEST literals into the
// registry — no hand-written Go Register() calls (ADR-0039 A1.1).
func TestScanTools(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "file_tool.py", `
TOOL_MANIFEST = '''
{
  "name": "read_file",
  "description": "Read a file",
  "dangerous": false,
  "path_args": ["path"],
  "schema": {"type": "object", "properties": {"path": {"type": "string"}}}
}
'''
def handle(args): ...
`)
	writeTool(t, dir, "terminal_tool.py", `
TOOL_MANIFEST = '''
{ "name": "execute_command", "description": "Run a command", "dangerous": true, "command_args": ["command"] }
'''
`)
	// a non-tool python file is ignored
	writeTool(t, dir, "helper.py", `x = 1`)
	// a *tool.py without a manifest is skipped, not fatal
	writeTool(t, dir, "broken_tool.py", `# no manifest here`)

	tools, err := ScanTools(dir)
	if err != nil {
		t.Fatalf("ScanTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("found %d tools, want 2 (read_file, execute_command)", len(tools))
	}
	byName := map[string]domain.SystemTool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	rf, ok := byName["read_file"]
	if !ok {
		t.Fatal("read_file not discovered")
	}
	if rf.Dangerous || len(rf.PathArgs) != 1 || rf.PathArgs[0] != "path" || len(rf.Schema) == 0 {
		t.Errorf("read_file parsed wrong: %+v", rf)
	}
	ec := byName["execute_command"]
	if !ec.Dangerous || len(ec.CommandArgs) != 1 {
		t.Errorf("execute_command parsed wrong: %+v", ec)
	}
}

func TestLoadRegistry(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "echo_tool.py", `
TOOL_MANIFEST = '''
{ "name": "echo", "description": "Echo input" }
'''
`)
	reg := domain.NewInMemoryToolRegistry()
	files, err := LoadRegistry(dir, reg)
	if err != nil || len(files) != 1 {
		t.Fatalf("LoadRegistry files=%v err=%v, want 1,nil", files, err)
	}
	if _, ok := reg.Get("echo"); !ok {
		t.Error("echo not registered")
	}
	if files["echo"] == "" {
		t.Error("LoadRegistry should return the echo tool's file path")
	}
}

// A missing directory is not fatal (no tools dir ⇒ zero tools, no system tools).
func TestScanTools_MissingDir(t *testing.T) {
	tools, err := ScanTools(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Errorf("missing dir should not error, got %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("missing dir should yield 0 tools, got %d", len(tools))
	}
}
