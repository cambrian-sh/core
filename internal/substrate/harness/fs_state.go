package harness

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// Snapshot stages all changes in dir and creates a git commit tagged
// "cambrian-snapshot". It returns the resulting commit hash.
// If dir is not a git repository, it logs a WARN and returns ("", nil).
func Snapshot(dir string) (string, error) {
	if err := exec.Command("git", "-C", dir, "rev-parse", "--git-dir").Run(); err != nil {
		slog.Warn("Snapshot: directory is not a git repository", "dir", dir)
		return "", nil
	}

	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add -A: %w\n%s", err, out)
	}

	if out, err := exec.Command("git", "-C", dir, "commit", "--allow-empty", "-m", "cambrian-snapshot").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %w\n%s", err, out)
	}

	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Restore resets the working tree of dir to the state recorded at hash.
// If hash is empty, it logs a WARN and returns nil (no-op).
func Restore(dir string, hash string) error {
	if hash == "" {
		slog.Warn("Restore: empty hash supplied; skipping restore", "dir", dir)
		return nil
	}

	if out, err := exec.Command("git", "-C", dir, "checkout", hash, "--", ".").CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout %s -- .: %w\n%s", hash, err, out)
	}

	// Remove untracked files that did not exist at the snapshot point.
	if out, err := exec.Command("git", "-C", dir, "clean", "-fd").CombinedOutput(); err != nil {
		return fmt.Errorf("git clean -fd: %w\n%s", err, out)
	}
	return nil
}
