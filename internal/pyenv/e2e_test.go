package pyenv

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2E_OfflineBuildBootCheckAndRepair is ADR-0157 D7's P1 end-to-end test,
// through the real path: the embedded pin table, the real pinned uv and
// python-build-standalone archives, a real hashed lock and real wheels, the boot
// check (Env.Start) with build_on_boot, and the real interpreter's import of the
// SDK. It runs only when PYENV_E2E_BUNDLE names an offline bundle made by
// scripts/build_offline_bundle.py for this host's platform:
//
//	PYENV_E2E_BUNDLE=<bundle dir> [PYENV_E2E_LOCK=<lock>] [PYENV_E2E_ROOT=<dir>] go test -run E2E ./internal/pyenv/
//
// The network is disabled for the whole test: offline mode, and every proxy
// variable points at a closed port so any HTTP attempt fails. Run it in a
// container started with --network none for a host-level guarantee (P1 ran it
// in debian:bookworm-slim, which has no Python, and on Windows as a non-admin
// user).
func TestE2E_OfflineBuildBootCheckAndRepair(t *testing.T) {
	bundle := os.Getenv("PYENV_E2E_BUNDLE")
	if bundle == "" {
		t.Skip("set PYENV_E2E_BUNDLE to an offline bundle (scripts/build_offline_bundle.py)")
	}
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"} {
		t.Setenv(k, "http://127.0.0.1:9")
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	lockPath := os.Getenv("PYENV_E2E_LOCK")
	if lockPath == "" {
		lockPath = filepath.Join("..", "..", "agents", "base.lock")
	}
	lock, err := os.ReadFile(lockPath)
	must(t, err)
	root := os.Getenv("PYENV_E2E_ROOT")
	if root == "" {
		root = filepath.Join(t.TempDir(), "runtime", "python")
	}
	pins, err := Pins()
	must(t, err)
	platform, err := HostPlatform()
	must(t, err)
	downloads := filepath.Join(bundle, "downloads")
	b := &Builder{
		Root: root, Pins: pins, Platform: platform, Lock: lock,
		Sources: Sources{
			PythonMirror: fileURL(downloads), InstallerMirror: fileURL(downloads),
			Wheelhouse: []string{filepath.Join(bundle, "wheelhouse")}, Offline: true,
		},
	}
	ctx := context.Background()

	// 1. First boot: nothing there → the boot check finds it missing and the
	//    build_on_boot background build makes it; the gate refuses meanwhile.
	st, err := b.Check(ctx)
	must(t, err)
	if st.State != StateMissing {
		t.Fatalf("fresh root: %+v", st)
	}
	env := NewEnv(b, true)
	env.Start(ctx)
	env.Wait()
	if err := env.Ready(); err != nil {
		t.Fatalf("after the boot build: %v", err)
	}
	st = env.Status()
	if st.State != StateHealthy {
		t.Fatalf("boot did not report healthy: %+v", st)
	}
	gen := filepath.Join(root, dirBase, st.Generation)
	t.Logf("generation %s: %d packages, %s on disk", gen, st.Packages, humanBytes(dirSize(t, gen)))
	t.Logf("env root %s: %s on disk (downloads + installer + cache + generation)", root, humanBytes(dirSize(t, root)))

	// 2. The interpreter agents will launch is the generation's real one: no
	//    venv (no pyvenv.cfg), no trampoline, prefix = the generation; the SDK
	//    imports in isolated mode.
	if _, err := os.Stat(filepath.Join(gen, "pyvenv.cfg")); err == nil {
		t.Fatal("the base generation must not be a venv")
	}
	out, err := exec.Command(st.Interpreter, "-I", "-c",
		"import sys, cambrian_agent_sdk; print(sys.prefix); print(sys.version.split()[0])").CombinedOutput()
	if err != nil {
		t.Fatalf("real interpreter: %v: %s", err, out)
	}
	lines := strings.Fields(string(out))
	if len(lines) < 2 || !sameDir(lines[0], gen) || lines[1] != pins.Python.Version {
		t.Fatalf("interpreter prefix/version %q, want %s / %s", out, gen, pins.Python.Version)
	}

	// 3. A generation deleted at rest: the boot check names it, and the boot
	//    build rebuilds it from the local bundle (same inputs → same directory).
	must(t, os.RemoveAll(gen))
	st, err = b.Check(ctx)
	must(t, err)
	if st.State != StateBroken || !strings.Contains(st.Reason, filepath.Base(gen)) {
		t.Fatalf("deleted generation not named: %+v", st)
	}
	env2 := NewEnv(b, true)
	env2.Start(ctx)
	env2.Wait()
	if err := env2.Ready(); err != nil {
		t.Fatalf("after the repair build: %v", err)
	}
	if st2 := env2.Status(); st2.State != StateHealthy || st2.Generation != filepath.Base(gen) {
		t.Fatalf("repair: %+v", st2)
	}

	// 4. Offline with a wheelhouse that lacks the lock's wheels: the build
	//    fails naming a missing distribution and nothing becomes current.
	bare := *b
	bare.Root = filepath.Join(t.TempDir(), "python")
	bare.Sources.Wheelhouse = []string{t.TempDir()}
	_, err = bare.Build(ctx)
	if err == nil {
		t.Fatal("an offline build without the wheels must fail")
	}
	t.Logf("offline without wheels: %v", err)
	if _, statErr := os.Stat(filepath.Join(bare.Root, fileCurrent)); statErr == nil {
		t.Fatal("a failed build must not write current.json")
	}
}

func sameDir(a, b string) bool {
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return strings.EqualFold(ea, eb)
}

func dirSize(t *testing.T, dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

func humanBytes(n int64) string {
	return fmt.Sprintf("%.2f GiB (%d bytes)", float64(n)/(1<<30), n)
}

// TestE2E_DevSDKWheel is setup's dev-tree path (D7.4.5): the local SDK is built
// into a wheel with the pinned uv and a scratch copy of the pinned interpreter
// (never the host's Python), then installed into the generation in place of the
// lock's published SDK. Needs the index for the build backend, so it runs only
// with PYENV_E2E_SDK_DIR (the sdk/ directory) and PYENV_E2E_BUNDLE (archives).
func TestE2E_DevSDKWheel(t *testing.T) {
	sdkDir, bundle := os.Getenv("PYENV_E2E_SDK_DIR"), os.Getenv("PYENV_E2E_BUNDLE")
	if sdkDir == "" || bundle == "" {
		t.Skip("set PYENV_E2E_SDK_DIR and PYENV_E2E_BUNDLE")
	}
	lockPath := os.Getenv("PYENV_E2E_LOCK")
	if lockPath == "" {
		lockPath = filepath.Join("..", "..", "agents", "base.lock")
	}
	lock, err := os.ReadFile(lockPath)
	must(t, err)
	pins, err := Pins()
	must(t, err)
	platform, err := HostPlatform()
	must(t, err)
	downloads := filepath.Join(bundle, "downloads")
	b := &Builder{
		Root: filepath.Join(t.TempDir(), "python"), Pins: pins, Platform: platform, Lock: lock,
		Sources: Sources{PythonMirror: fileURL(downloads), InstallerMirror: fileURL(downloads),
			Wheelhouse: []string{filepath.Join(bundle, "wheelhouse")}},
	}
	wheel, err := b.BuildSDKWheel(context.Background(), sdkDir)
	must(t, err)
	if !strings.HasPrefix(filepath.Base(wheel), "cambrian_agent_sdk-") || !strings.HasSuffix(wheel, ".whl") {
		t.Fatalf("wheel %s", wheel)
	}
	b.SDKWheel = wheel
	res, err := b.Build(context.Background())
	must(t, err)
	var m Marker
	must(t, readJSON(filepath.Join(res.Generation, fileMarker), &m))
	if m.Source != "release-lock+dev-sdk-wheel" || m.Inputs.SDKWheelSHA256 == "" {
		t.Fatalf("marker %+v", m)
	}
	out, err := exec.Command(res.Interpreter, "-I", "-c", "import cambrian_agent_sdk, os; print(os.path.dirname(cambrian_agent_sdk.__file__))").CombinedOutput()
	if err != nil || !strings.Contains(strings.ToLower(string(out)), strings.ToLower(filepath.Base(res.Generation))) {
		t.Fatalf("the SDK must import from the generation, not the repository: %v %s", err, out)
	}
}
