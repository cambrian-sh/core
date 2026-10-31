package agentmgr

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// TestCallAgent_DoesNotCommitAgentDir pins the removal of ADR-0005 D2b/D3
// (owner decision 2026-10-31). CallAgent used to run `git add -A && git commit
// -m cambrian-snapshot` in the agent's dir before every call; when that dir sat
// in an operator's repo it committed their uncommitted work (69 such commits
// reached core/premium history). CallAgent must leave the dir's git history and
// working tree exactly as it found them, whether or not the call succeeds.
func TestCallAgent_DoesNotCommitAgentDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "work.txt")
	git("commit", "-q", "-m", "initial")
	// Uncommitted operator work: one modified tracked file, one untracked file.
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("v2-uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("untracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	headBefore := git("rev-parse", "HEAD")
	statusBefore := git("status", "--porcelain")

	reg := newTestRegistry()
	reg.agents["agent-d"] = domain.AgentDefinition{ID: "agent-d", Dir: dir}
	// A python path that does not exist: the boot fails, which is fine — the
	// old snapshot ran before boot, so a failing call still exercised it.
	m := NewAgentManager(reg, filepath.Join(t.TempDir(), "no-such-python"), "localhost:0", nil)
	_, _ = m.CallAgent(t.Context(), "agent-d", &domain.Handoff{
		Context: map[string]string{"task_id": "task-0"},
	})

	if got := git("rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("CallAgent moved HEAD of the agent dir: %s -> %s (log: %s)",
			headBefore, got, git("log", "--oneline", "-3"))
	}
	if got := git("status", "--porcelain"); got != statusBefore {
		t.Fatalf("CallAgent changed the working tree status:\nbefore:\n%s\nafter:\n%s", statusBefore, got)
	}
}
