package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// ENG-2031: the docker runner hands an exited agent's scratch back to a
// non-root kernel before out/ is collected and the scratch removed.

func TestReclaimArgsAreConfinedAndMountOnlyTheScratch(t *testing.T) {
	a := reclaimArgs("cambrian-agent-x-1234", "img:1", "65534:65534", "/w/first-party/x-1")
	join := strings.Join(a, " ")
	for _, want := range []string{
		"run --rm --name cambrian-agent-x-1234-reclaim",
		"--network none", "--read-only", "--cap-drop ALL", "--security-opt no-new-privileges",
		"--user 65534:65534", "-v /w/first-party/x-1:" + containerWork,
		"--entrypoint /bin/sh img:1 -c chmod -R a+rwX " + containerWork,
	} {
		if !strings.Contains(join, want) {
			t.Errorf("argv lacks %q: %s", want, join)
		}
	}
	if n := strings.Count(join, " -v "); n != 1 {
		t.Errorf("only the scratch may be mounted, got %d mounts: %s", n, join)
	}
	if slices.Contains(a, "host") || slices.Contains(a, "--privileged") || slices.Contains(a, "--cap-add") {
		t.Errorf("the reclaim container must have no network, privilege or capability: %s", join)
	}
}

type fakeReclaimHandle struct {
	err   error
	calls int
	dir   string
}

func (f *fakeReclaimHandle) kill() error         { return nil }
func (f *fakeReclaimHandle) release()            {}
func (f *fakeReclaimHandle) limitReason() string { return "" }
func (f *fakeReclaimHandle) reclaimScratch(_ context.Context, dir string) error {
	f.calls++
	f.dir = dir
	return f.err
}

type plainTestHandle struct{}

func (plainTestHandle) kill() error         { return nil }
func (plainTestHandle) release()            {}
func (plainTestHandle) limitReason() string { return "" }

func TestLaunchedReclaimKeepsTheScratchWhenItFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scratch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &fakeReclaimHandle{err: errors.New("docker: no such image")}
	l := &Launched{h: h, workdir: dir, remove: true, report: Report{ServerID: "probe-1"}}
	err := l.Reclaim(context.Background())
	if err == nil || !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "kept") ||
		!strings.Contains(err.Error(), "no such image") {
		t.Fatalf("want an error naming the kept scratch and the cause, got %v", err)
	}
	if h.calls != 1 || h.dir != dir {
		t.Fatalf("reclaim ran %d times on %q", h.calls, h.dir)
	}
	l.Release()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a scratch that was not handed back must be kept, not removed: %v", err)
	}
}

func TestLaunchedReclaimThenReleaseRemovesTheScratch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scratch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &fakeReclaimHandle{}
	l := &Launched{h: h, workdir: dir, remove: true}
	if err := l.Reclaim(context.Background()); err != nil || h.calls != 1 {
		t.Fatalf("reclaim: %v (calls %d)", err, h.calls)
	}
	l.Release()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("scratch survived Release: %v", err)
	}
	// A runner without a reclaim step: a no-op.
	if err := (&Launched{h: plainTestHandle{}, workdir: dir}).Reclaim(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestContainerHandleReclaimIsANoOpForMCPAndRoot(t *testing.T) {
	// An MCP container has no reclaim step, and a root kernel skips it: neither
	// runs docker (bin is a path that does not exist).
	h := &containerHandle{bin: filepath.Join(t.TempDir(), "no-docker")}
	if err := h.reclaimScratch(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("MCP container: %v", err)
	}
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		h.reclaim = &dockerReclaim{image: "img", user: "65534:65534"}
		if err := h.reclaimScratch(context.Background(), t.TempDir()); err != nil {
			t.Fatalf("root kernel: %v", err)
		}
	}
}

func TestContainerHandleReclaimFailsVisiblyWhenDockerFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("the reclaim runs only for a non-root Unix kernel")
	}
	h := &containerHandle{bin: filepath.Join(t.TempDir(), "no-docker"), name: "cambrian-agent-x",
		reclaim: &dockerReclaim{image: "img", user: "65534:65534"}}
	err := h.reclaimScratch(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "reclaim container") {
		t.Fatalf("want the failed reclaim container named, got %v", err)
	}
}

func TestUnreachableInNamesTheClosedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix file modes that bind the caller (not root)")
	}
	root := t.TempDir()
	closed := filepath.Join(root, "out", "sub")
	if err := os.MkdirAll(closed, 0o755); err != nil {
		t.Fatal(err)
	}
	if p, err := unreachableIn(root); err != nil || p != "" {
		t.Fatalf("open tree: %q %v", p, err)
	}
	if err := os.Chmod(closed, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(closed, 0o755) })
	if p, err := unreachableIn(root); err == nil || p != closed {
		t.Fatalf("want %s unreachable, got %q %v", closed, p, err)
	}
}
