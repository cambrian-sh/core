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
	files, err := LoadRegistry(dir, reg, false)
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

// ADR-0086: a manifest may declare its own classification tags and effect classes,
// and a declared set beats inference.
func TestLoadRegistry_HonoursDeclaredEffects(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "pay_tool.py", `
TOOL_MANIFEST = '''
{
  "name": "issue_refund",
  "description": "Refund a payment",
  "dangerous": true,
  "classification_tags": ["payments"],
  "effects": ["read", "spend"]
}
'''
`)
	reg := domain.NewInMemoryToolRegistry()
	if _, err := LoadRegistry(dir, reg, false); err != nil {
		t.Fatal(err)
	}
	tool, ok := reg.Get("issue_refund")
	if !ok {
		t.Fatal("tool should be registered")
	}
	if tool.EffectsInferred {
		t.Errorf("a declared set must not be marked inferred")
	}
	if !tool.HasEffect(domain.EffectSpend) {
		t.Errorf("declared spend must survive registration, got %v", tool.Effects)
	}
	// `dangerous` would have inferred write; the declaration is authoritative.
	if tool.HasEffect(domain.EffectWrite) {
		t.Errorf("a declared set must not be augmented by inference, got %v", tool.Effects)
	}
	if len(tool.ClassificationTags) != 1 || tool.ClassificationTags[0] != "payments" {
		t.Errorf("classification tags must reach the registry, got %v", tool.ClassificationTags)
	}
}

// Strict mode refuses an un-migrated tool rather than guessing; the rest of the
// directory still loads, because one bad manifest must not take out every tool.
func TestLoadRegistry_StrictSkipsUnclassifiedButKeepsGoing(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "legacy_tool.py", `
TOOL_MANIFEST = '''
{ "name": "legacy_reader", "description": "no effects declared" }
'''
`)
	writeTool(t, dir, "modern_tool.py", `
TOOL_MANIFEST = '''
{ "name": "modern_reader", "description": "declares its effects", "effects": ["read"] }
'''
`)
	reg := domain.NewInMemoryToolRegistry()
	files, err := LoadRegistry(dir, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("legacy_reader"); ok {
		t.Errorf("strict mode must refuse an unclassified tool")
	}
	if _, ok := reg.Get("modern_reader"); !ok {
		t.Errorf("one bad manifest must not take out the rest of the directory")
	}
	if _, ok := files["legacy_reader"]; ok {
		t.Errorf("a refused tool must not appear in the invocation map either")
	}

	// Non-strict accepts it, inferred and flagged for migration.
	reg2 := domain.NewInMemoryToolRegistry()
	if _, err := LoadRegistry(dir, reg2, false); err != nil {
		t.Fatal(err)
	}
	got, ok := reg2.Get("legacy_reader")
	if !ok || !got.EffectsInferred {
		t.Errorf("non-strict must register it as inferred, got ok=%v %+v", ok, got.Effects)
	}
}

// An effect outside the closed set is refused even in non-strict mode: absence is
// a migration state, invalidity is a bug.
func TestLoadRegistry_RefusesUnknownEffectEvenWhenLenient(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "bad_tool.py", `
TOOL_MANIFEST = '''
{ "name": "sudo_tool", "description": "x", "effects": ["escalate"] }
'''
`)
	reg := domain.NewInMemoryToolRegistry()
	if _, err := LoadRegistry(dir, reg, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("sudo_tool"); ok {
		t.Errorf("an unrecognisable effect must never reach the executor")
	}
}
