package archlint

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// Repository hygiene, AGENTS.md §3.6 item 7: no backup copies in the repo.
// Two hand-made backups (configs/config.json.bak-20261104,
// configs/mcp.json.bak-20261105) were tracked for weeks and COPY'd into the
// kernel image with the configs directory
// (T-20261118-stale-config-backups-configs-config-json-bak). A backup of an
// operator config is exactly the file that holds real endpoints and secrets,
// and nothing reviews it because nobody reads a ".bak".
//
// backupGlobs is the one list. .gitignore must carry each glob and
// Dockerfile.dockerignore each glob under "**/" (TestHygiene_IgnoreFilesCarryBackupGlobs),
// and no file git would commit may match one (TestHygiene_NoBackupFilesInRepo).

// backupGlobs match a file's base name (path.Match syntax, which is also valid
// gitignore and dockerignore syntax). ".<name>.tmp-*" is what the config
// store's atomic write leaves behind if it dies mid-write
// (internal/config/file_layers.go writeFileAtomic); "*.pre-compact" is the
// original bbolt-compact keeps (internal/storage.PreCompactSuffix).
var backupGlobs = []string{
	"*.bak",
	"*.bak-*",
	"*.bak.*",
	"*.bak_*",
	"*.orig",
	"*.rej",
	"*.old",
	"*.backup",
	"*.swp",
	"*~",
	".*.tmp-*",
	"*.pre-compact",
}

func isBackupName(base string) bool {
	for _, g := range backupGlobs {
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	return false
}

// ruleNoBackupFiles reports every path (slash, repo-relative) whose base name
// is a backup copy.
func ruleNoBackupFiles(paths []string) []finding {
	var out []finding
	for _, p := range paths {
		if isBackupName(path.Base(p)) {
			out = append(out, finding{rule: "backup", key: p, pos: p,
				msg: "backup copy in the repository: delete it (configs keep their template as *.example)"})
		}
	}
	return out
}

// committableFiles lists what `git add -A` would commit under root: tracked
// files still on disk plus untracked files no ignore rule covers. A tracked
// file already deleted from the working tree is a pending removal, not a
// violation. It reports ok=false when root is not a git work tree (a source
// tarball); CI always has one.
func committableFiles(t *testing.T, root string) (files []string, ok bool) {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) || errors.Is(err, exec.ErrNotFound) {
			t.Logf("git ls-files: %v %s", err, strings.TrimSpace(stderr.String()))
			return nil, false
		}
		t.Fatalf("git ls-files: %v", err)
	}
	seen := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
			continue
		}
		files = append(files, p)
	}
	return files, true
}

func TestHygiene_NoBackupFilesInRepo(t *testing.T) {
	root, _ := moduleRoot(t)
	files, ok := committableFiles(t, root)
	if !ok {
		if os.Getenv("CI") != "" {
			t.Fatal("not a git work tree under CI: the backup-file guard cannot run")
		}
		t.Skip("not a git work tree; the backup-file guard needs git ls-files")
	}
	if len(files) == 0 {
		t.Fatal("git ls-files listed nothing: the guard would pass vacuously")
	}
	check(t, "backup", ruleNoBackupFiles(files), nil)
}

func TestHygiene_IgnoreFilesCarryBackupGlobs(t *testing.T) {
	root, _ := moduleRoot(t)
	lines := func(name string) map[string]bool {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		set := map[string]bool{}
		for _, l := range strings.Split(string(b), "\n") {
			set[strings.TrimSpace(l)] = true
		}
		return set
	}
	gi, di := lines(".gitignore"), lines("Dockerfile.dockerignore")
	for _, g := range backupGlobs {
		if !gi[g] {
			t.Errorf(".gitignore lacks the backup glob %q", g)
		}
		if !di["**/"+g] {
			t.Errorf("Dockerfile.dockerignore lacks the backup glob %q", "**/"+g)
		}
	}
}

// The rule must be able to fail, and must leave real config names alone.
func TestPlanted_BackupFiles(t *testing.T) {
	got := ruleNoBackupFiles([]string{
		"configs/config.json.bak-20261104", // the two files this guard was written for
		"configs/mcp.json.bak-20261105",
		"configs/config.json.bak",
		"configs/.config.json.tmp-123456", // a dead atomic write
		"configs/tuning.json~",
		"internal/x/y.go.orig",
		"data/agents.db.pre-compact",
		"configs/config.example.json", // accepted from here on
		"configs/mcp.json.example",
		"configs/tuning.json",
		"configs/config.bakery.json",
		"configs/README.md",
		".gitignore",
	})
	wantKeys(t, "backup", got,
		"configs/config.json.bak-20261104",
		"configs/mcp.json.bak-20261105",
		"configs/config.json.bak",
		"configs/.config.json.tmp-123456",
		"configs/tuning.json~",
		"internal/x/y.go.orig",
		"data/agents.db.pre-compact",
	)
	r := &recorder{}
	checkInto(r, "backup", got, nil)
	if !r.failed {
		t.Error("check did not fail on planted backup files")
	}
}
