package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeGitRepo creates a temp directory with a git repo and an initial commit.
func makeGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init")
	run("config", "user.email", "test@cambrian.local")
	run("config", "user.name", "Cambrian Test")

	initial := filepath.Join(dir, "initial.txt")
	if err := os.WriteFile(initial, []byte("initial content\n"), 0o644); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	run("add", "-A")
	run("commit", "-m", "init")

	return dir
}

// ============================================================
// Cycle 1 — Snapshot on a dirty repo returns a non-empty hash
// ============================================================

func TestSnapshot_DirtyRepo_ReturnsHash(t *testing.T) {
	dir := makeGitRepo(t)

	if err := os.WriteFile(filepath.Join(dir, "initial.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hash, err := Snapshot(dir)
	if err != nil {
		t.Fatalf("Snapshot returned unexpected error: %v", err)
	}
	if hash == "" {
		t.Fatal("Snapshot returned empty hash; want non-empty commit hash")
	}
}

// ============================================================
// Cycle 2 — Restore recovers mutated file to snapshotted content
// ============================================================

func TestRestore_MutatedFile_RestoredToOriginal(t *testing.T) {
	dir := makeGitRepo(t)
	filePath := filepath.Join(dir, "initial.txt")

	original, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}

	hash, err := Snapshot(dir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if err := os.WriteFile(filePath, []byte("mutated content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Restore(dir, hash); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	restored, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}

	normalise := func(b []byte) string { return strings.ReplaceAll(string(b), "\r\n", "\n") }
	if normalise(restored) != normalise(original) {
		t.Errorf("Restore: got %q, want %q", restored, original)
	}
}

// ============================================================
// Cycle 3 — File created after Snapshot is removed by Restore
// ============================================================

func TestRestore_NewFileAfterSnapshot_Removed(t *testing.T) {
	dir := makeGitRepo(t)

	hash, err := Snapshot(dir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	newFile := filepath.Join(dir, "new_file.txt")
	if err := os.WriteFile(newFile, []byte("should be gone after restore\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Restore(dir, hash); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Errorf("Restore: new_file.txt still exists; expected it to be removed")
	}
}

// ============================================================
// Cycle 4 — File deleted after Snapshot is reinstated by Restore
// ============================================================

func TestRestore_DeletedFileAfterSnapshot_Reinstated(t *testing.T) {
	dir := makeGitRepo(t)
	filePath := filepath.Join(dir, "initial.txt")

	hash, err := Snapshot(dir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if err := os.Remove(filePath); err != nil {
		t.Fatal(err)
	}

	if err := Restore(dir, hash); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Error("Restore: initial.txt was not reinstated after deletion")
	}
}

// ============================================================
// Cycle 5 — Snapshot on non-git directory returns ("", nil)
// ============================================================

func TestSnapshot_NonGitDir_ReturnsEmptyHashNoError(t *testing.T) {
	dir := t.TempDir()

	hash, err := Snapshot(dir)
	if err != nil {
		t.Fatalf("Snapshot on non-git dir: unexpected error: %v", err)
	}
	if hash != "" {
		t.Errorf("Snapshot on non-git dir: got hash %q, want empty string", hash)
	}
}

// ============================================================
// Cycle 6 — Restore with empty hash is a no-op, returns nil
// ============================================================

func TestRestore_EmptyHash_NoOp(t *testing.T) {
	dir := makeGitRepo(t)
	filePath := filepath.Join(dir, "initial.txt")

	before, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := Restore(dir, ""); err != nil {
		t.Fatalf("Restore(\"\") returned unexpected error: %v", err)
	}

	after, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("Restore with empty hash mutated the file: got %q, want %q", after, before)
	}
}
