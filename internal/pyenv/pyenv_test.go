package pyenv

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// ---- fixtures -----------------------------------------------------------------------------

type tarEntry struct {
	name, body, link string
	typ              byte
}

func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o755, Size: int64(len(e.body)), Linkname: e.link}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeZip(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(body))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

const testPlatform = "linux-x86_64-gnu"

const testLock = `# hashed
six==1.17.0 \
    --hash=sha256:4721f391ed90541fddacab5acf947aa0d3dc7d27b2e1e8eda2be8970586c3274
cambrian-agent-sdk==0.1.5 --hash=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
nvidia-nccl-cu13==2.29.7 ; sys_platform == 'linux' \
    --hash=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
`

// fixture is a fake upstream: an interpreter archive, an installer archive,
// a pin table naming their digests, and a mirror directory holding both.
type fixture struct {
	pins     PinTable
	pyBytes  []byte
	uvBytes  []byte
	mirror   string // local directory with both archives
	root     string
	calls    []string
	mu       sync.Mutex
	failNext string // a substring: the next matching run fails
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{root: filepath.Join(t.TempDir(), "runtime", "python"), mirror: t.TempDir()}
	f.pyBytes = makeTarGz(t, []tarEntry{
		{name: "python/", typ: tar.TypeDir},
		{name: "python/bin/python3.12", body: "#!fake interpreter\n"},
		{name: "python/bin/python3", typ: tar.TypeSymlink, link: "python3.12"},
		{name: "python/lib/python3.12/os.py", body: "# stdlib\n"},
	})
	if runtime.GOOS == "windows" {
		// os.Symlink needs a privilege on Windows; the Windows build has no links.
		f.pyBytes = makeTarGz(t, []tarEntry{
			{name: "python/bin/python3", body: "#!fake interpreter\n"},
			{name: "python/lib/python3.12/os.py", body: "# stdlib\n"},
		})
	}
	f.uvBytes = makeTarGz(t, []tarEntry{{name: "uv-x86_64-unknown-linux-gnu/uv", body: "fake uv"}})
	pyName := "cpython-3.12.16+20261110-x86_64-unknown-linux-gnu-install_only.tar.gz"
	uvName := "uv-x86_64-unknown-linux-gnu.tar.gz"
	f.pins = PinTable{
		Python:    Release{Version: "3.12.16", BuildTag: "20261110", BaseURL: "https://example.invalid/py/", Assets: map[string]Asset{testPlatform: {Name: pyName, SHA256: sum(f.pyBytes)}}},
		Installer: Release{Name: "uv", Version: "0.11.21", BaseURL: "https://example.invalid/uv/", Assets: map[string]Asset{testPlatform: {Name: uvName, SHA256: sum(f.uvBytes)}}},
	}
	must(t, os.WriteFile(filepath.Join(f.mirror, pyName), f.pyBytes, 0o644))
	must(t, os.WriteFile(filepath.Join(f.mirror, uvName), f.uvBytes, 0o644))
	return f
}

func (f *fixture) run(ctx context.Context, env []string, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := filepath.Base(name) + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	if f.failNext != "" && strings.Contains(line, f.failNext) {
		f.failNext = ""
		return "boom: last installer line", errors.New("exit status 1")
	}
	return "", nil
}

func (f *fixture) builder(t *testing.T) *Builder {
	return &Builder{
		Root: f.root, Pins: f.pins, Platform: testPlatform, Lock: []byte(testLock),
		Sources: Sources{PythonMirror: fileURL(f.mirror), InstallerMirror: fileURL(f.mirror), Wheelhouse: []string{t.TempDir()}, Offline: true},
		run:     f.run,
	}
}

func fileURL(dir string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs(dir))}).String()
}

func abs(p string) string {
	a, _ := filepath.Abs(p)
	if runtime.GOOS == "windows" {
		return "/" + filepath.ToSlash(a)
	}
	return a
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- pin table ----------------------------------------------------------------------------

func TestEmbeddedPinTableCoversEveryPlatform(t *testing.T) {
	p, err := Pins()
	must(t, err)
	if !strings.HasPrefix(p.Python.Version, "3.12.") {
		t.Fatalf("python minor must be 3.12 (owner A1-Q1), got %s", p.Python.Version)
	}
	if p.Installer.Name != "uv" || p.Installer.Version == "" {
		t.Fatalf("installer pin: %+v", p.Installer)
	}
	for _, plat := range []string{"windows-x86_64", "linux-x86_64-gnu", "linux-aarch64-gnu", "macos-x86_64", "macos-aarch64"} {
		py, err := p.PythonAsset(plat)
		if err != nil || !strings.Contains(py.Name, "install_only") || !strings.Contains(py.Name, p.Python.Version) {
			t.Errorf("%s python asset %+v err %v", plat, py, err)
		}
		uv, err := p.InstallerAsset(plat)
		if err != nil || uv.Name == "" {
			t.Errorf("%s installer asset %+v err %v", plat, uv, err)
		}
		for _, h := range []string{py.SHA256, uv.SHA256} {
			if _, err := hex.DecodeString(h); err != nil || len(h) != 64 {
				t.Errorf("%s: bad digest %q", plat, h)
			}
		}
	}
	if _, err := p.PythonAsset("linux-riscv64-gnu"); err == nil {
		t.Fatal("an unpinned platform must be refused")
	}
}

func TestPlatformMapping(t *testing.T) {
	for _, c := range []struct{ goos, goarch, want string }{
		{"windows", "amd64", "windows-x86_64"}, {"linux", "amd64", "linux-x86_64-gnu"},
		{"linux", "arm64", "linux-aarch64-gnu"}, {"darwin", "arm64", "macos-aarch64"}, {"darwin", "amd64", "macos-x86_64"},
	} {
		if got, err := Platform(c.goos, c.goarch); err != nil || got != c.want {
			t.Errorf("Platform(%s,%s) = %q, %v; want %q", c.goos, c.goarch, got, err, c.want)
		}
	}
	for _, c := range [][2]string{{"windows", "arm64"}, {"linux", "386"}, {"freebsd", "amd64"}} {
		if _, err := Platform(c[0], c[1]); err == nil {
			t.Errorf("Platform(%s,%s) should be refused", c[0], c[1])
		}
	}
}

// ---- base lock ----------------------------------------------------------------------------

func TestReleaseBaseLockIsFullyHashed(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "agents", "base.lock"))
	must(t, err)
	l, err := ParseLock(b)
	must(t, err)
	for _, want := range []string{SDKProject, "grpcio", "protobuf", "torch", "spacy", "docling-slim"} {
		if !l.Has(want) {
			t.Errorf("base lock lacks %s", want)
		}
	}
	var linuxOnly int
	for _, r := range l.Requirements {
		if strings.Contains(r.Marker, "sys_platform == 'linux'") {
			linuxOnly++
		}
	}
	if linuxOnly == 0 {
		t.Error("expected platform markers in the universal base lock (Linux CUDA wheels)")
	}
}

func TestParseLockRefusesWhatHashModeCannotInstall(t *testing.T) {
	for name, text := range map[string]string{
		"no hash":   "six==1.17.0\n",
		"range":     "six>=1.0 --hash=sha256:" + strings.Repeat("a", 64) + "\n",
		"option":    "--index-url https://evil.example/simple\nsix==1.17.0 --hash=sha256:" + strings.Repeat("a", 64) + "\n",
		"editable":  "-e ./sdk\n",
		"url":       "six @ https://x/six.whl --hash=sha256:" + strings.Repeat("a", 64) + "\n",
		"md5":       "six==1.17.0 --hash=md5:abc\n",
		"duplicate": "six==1.17.0 --hash=sha256:" + strings.Repeat("a", 64) + "\nsix==1.16.0 --hash=sha256:" + strings.Repeat("b", 64) + "\n",
		"empty":     "# nothing\n",
	} {
		if _, err := ParseLock([]byte(text)); err == nil {
			t.Errorf("%s: expected refusal", name)
		}
	}
	l, err := ParseLock([]byte(testLock))
	must(t, err)
	if len(l.Requirements) != 3 || l.Requirements[2].Marker != "sys_platform == 'linux'" || len(l.Requirements[0].Hashes) != 1 {
		t.Fatalf("parsed %+v", l.Requirements)
	}
	out := string(l.Render(SDKProject))
	if strings.Contains(out, "cambrian-agent-sdk") || !strings.Contains(out, "nvidia-nccl-cu13==2.29.7 ; sys_platform == 'linux'") {
		t.Fatalf("render: %s", out)
	}
	if _, err := ParseLock([]byte(out)); err != nil {
		t.Fatalf("rendered lock must re-parse: %v", err)
	}
}

// ---- download + verify --------------------------------------------------------------------

func TestEnsureArchiveFromMirrorUsesPinnedDigest(t *testing.T) {
	good := []byte("the pinned archive")
	served := good
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.EscapedPath(), "/a%2Bb.tar.gz") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(served)
	}))
	defer srv.Close()
	dl := t.TempDir()
	a := Asset{Name: "a+b.tar.gz", SHA256: sum(good)}
	p, err := ensureArchive(context.Background(), srv.Client(), dl, a, srv.URL+"/mirror/", false)
	must(t, err)
	if b, _ := os.ReadFile(p); !bytes.Equal(b, good) {
		t.Fatal("stored archive differs")
	}

	// A mirror serving other bytes is refused with both digests, nothing kept.
	served = []byte("a substituted build")
	dl2 := t.TempDir()
	_, err = ensureArchive(context.Background(), srv.Client(), dl2, a, srv.URL+"/mirror/", false)
	var mm *HashMismatchError
	if !errors.As(err, &mm) || mm.Want != sum(good) || mm.Got != sum(served) {
		t.Fatalf("want a mismatch naming both digests, got %v", err)
	}
	if !strings.Contains(err.Error(), sum(good)) || !strings.Contains(err.Error(), sum(served)) {
		t.Fatalf("error must name both digests: %v", err)
	}
	if entries, _ := os.ReadDir(dl2); len(entries) != 0 {
		t.Fatalf("mismatched download left files behind: %v", entries)
	}
}

func TestEnsureArchiveTamperedAtRestIsDeleted(t *testing.T) {
	dl := t.TempDir()
	a := Asset{Name: "py.tar.gz", SHA256: sum([]byte("good"))}
	must(t, os.WriteFile(filepath.Join(dl, a.Name), []byte("tampered"), 0o644))
	_, err := ensureArchive(context.Background(), http.DefaultClient, dl, a, "https://example.invalid/", true)
	var mm *HashMismatchError
	if !errors.As(err, &mm) || mm.Got != sum([]byte("tampered")) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dl, a.Name)); !os.IsNotExist(err) {
		t.Fatal("tampered archive must be deleted")
	}
}

func TestEnsureArchiveOffline(t *testing.T) {
	dl := t.TempDir()
	a := Asset{Name: "py.tar.gz", SHA256: sum([]byte("good"))}
	_, err := ensureArchive(context.Background(), http.DefaultClient, dl, a, "https://example.invalid/", true)
	if !errors.Is(err, ErrOfflineMissing) || !strings.Contains(err.Error(), "py.tar.gz") {
		t.Fatalf("offline without the archive must fail naming it, got %v", err)
	}
	mirror := t.TempDir()
	must(t, os.WriteFile(filepath.Join(mirror, a.Name), []byte("good"), 0o644))
	if _, err := ensureArchive(context.Background(), http.DefaultClient, dl, a, fileURL(mirror), true); err != nil {
		t.Fatalf("a file:// mirror works offline: %v", err)
	}
}

// ---- extraction ---------------------------------------------------------------------------

func TestExtractRefusesEscapes(t *testing.T) {
	for name, entries := range map[string][]tarEntry{
		"dotdot":         {{name: "python/../evil", body: "x"}},
		"outside strip":  {{name: "other/evil", body: "x"}},
		"absolute link":  {{name: "python/l", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"escaping link":  {{name: "python/bin/l", typ: tar.TypeSymlink, link: "../../x"}},
		"backslash name": {{name: "python/..\\evil", body: "x"}},
	} {
		dir := t.TempDir()
		arch := filepath.Join(dir, "a.tar.gz")
		must(t, os.WriteFile(arch, makeTarGz(t, entries), 0o644))
		dest := filepath.Join(dir, "out")
		if err := extractTarGz(arch, dest, "python/"); !errors.Is(err, ErrArchiveEscape) {
			t.Errorf("%s: want ErrArchiveEscape, got %v", name, err)
		}
	}
}

// ---- build, generations, boot check -------------------------------------------------------

func TestBuildOfflineMakesContentAddressedGeneration(t *testing.T) {
	f := newFixture(t)
	b := f.builder(t)
	res, err := b.Build(context.Background())
	must(t, err)
	in, _, _ := b.Inputs()
	if filepath.Base(res.Generation) != in.Dir() || len(in.Dir()) != 16 || res.ID != in.ID() {
		t.Fatalf("generation %s not named by the inputs digest %s", res.Generation, in.ID())
	}
	if res.Interpreter != filepath.Join(res.Generation, "bin", "python3") {
		t.Fatalf("interpreter %s must be the real interpreter, not a venv launcher", res.Interpreter)
	}
	var m Marker
	must(t, readJSON(filepath.Join(res.Generation, fileMarker), &m))
	if m.Source != "release-lock" || m.Actor != "system:kernel" || m.Packages != 3 || m.ID != in.ID() {
		t.Fatalf("marker %+v", m)
	}
	var cur Current
	must(t, readJSON(filepath.Join(f.root, fileCurrent), &cur))
	if cur.Base != in.Dir() || cur.Interpreter != res.Interpreter {
		t.Fatalf("current.json %+v", cur)
	}
	install := ""
	for _, c := range f.calls {
		if strings.Contains(c, "pip install") {
			install = c
		}
	}
	for _, want := range []string{"--require-hashes", "--no-deps", "--only-binary :all:", "--link-mode copy", "--offline", "--no-index", "--find-links", "--no-config"} {
		if !strings.Contains(install, want) {
			t.Errorf("install %q lacks %s", install, want)
		}
	}
	if strings.Contains(install, "--index-url") {
		t.Errorf("offline install must not name an index: %q", install)
	}

	st, err := b.Check(context.Background())
	must(t, err)
	if st.State != StateHealthy {
		t.Fatalf("check after build: %+v", st)
	}

	// Same inputs → same generation, reused, no reinstall.
	f.calls = nil
	res2, err := b.Build(context.Background())
	must(t, err)
	if !res2.Reused || res2.Generation != res.Generation {
		t.Fatalf("rebuild of the same inputs must reuse %s: %+v", res.Generation, res2)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "pip install") {
			t.Fatalf("reuse must not reinstall: %v", f.calls)
		}
	}

	// A different lock is a different generation; the old one is untouched.
	b.Lock = []byte(strings.Replace(testLock, "six==1.17.0", "six==1.16.0", 1))
	res3, err := b.Build(context.Background())
	must(t, err)
	if res3.Generation == res.Generation {
		t.Fatal("a changed lock must produce a new generation")
	}
	if _, err := os.Stat(res.Generation); err != nil {
		t.Fatal("generations are immutable: the previous one must stay")
	}
}

func TestBuildOnlineUsesIndexAndUpstreamDigest(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.FileServer(http.Dir(f.mirror)))
	defer srv.Close()
	b := f.builder(t)
	b.Sources = Sources{PythonMirror: srv.URL + "/", InstallerMirror: srv.URL + "/", IndexURL: "https://pypi.internal/simple"}
	b.HTTPClient = srv.Client()
	_, err := b.Build(context.Background())
	must(t, err)
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "--index-url https://pypi.internal/simple") || strings.Contains(joined, "--offline") {
		t.Fatalf("online install args: %s", joined)
	}
}

func TestBuildHashMismatchKeepsPreviousGeneration(t *testing.T) {
	f := newFixture(t)
	b := f.builder(t)
	first, err := b.Build(context.Background())
	must(t, err)
	// A new release pins a different interpreter; the mirror serves bytes that
	// do not match it.
	b2 := f.builder(t)
	b2.Pins.Python.Assets = map[string]Asset{testPlatform: {Name: "cpython-new.tar.gz", SHA256: sum([]byte("expected"))}}
	must(t, os.WriteFile(filepath.Join(f.mirror, "cpython-new.tar.gz"), []byte("served"), 0o644))
	_, err = b2.Build(context.Background())
	var mm *HashMismatchError
	if !errors.As(err, &mm) || mm.Want != sum([]byte("expected")) || mm.Got != sum([]byte("served")) {
		t.Fatalf("want mismatch with both digests, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, dirDownloads, "cpython-new.tar.gz")); !os.IsNotExist(err) {
		t.Fatal("the mismatched archive must not be kept")
	}
	var cur Current
	must(t, readJSON(filepath.Join(f.root, fileCurrent), &cur))
	if cur.Interpreter != first.Interpreter {
		t.Fatalf("a failed build must leave the previous generation current: %+v", cur)
	}
	// And the old generation now reads as stale (other inputs), still usable.
	st, err := b2.Check(context.Background())
	must(t, err)
	if st.State != StateStale || !st.Usable() {
		t.Fatalf("want stale+usable, got %+v", st)
	}
}

func TestCheckDetectsMissingAndBrokenGenerations(t *testing.T) {
	f := newFixture(t)
	b := f.builder(t)
	st, err := b.Check(context.Background())
	must(t, err)
	if st.State != StateMissing {
		t.Fatalf("fresh root: %+v", st)
	}
	res, err := b.Build(context.Background())
	must(t, err)

	// Tampered interpreter → broken, naming the generation.
	must(t, os.WriteFile(res.Interpreter, []byte("swapped"), 0o755))
	st, _ = b.Check(context.Background())
	if st.State != StateBroken || !strings.Contains(st.Reason, filepath.Base(res.Generation)) || !strings.Contains(st.Reason, "digest") {
		t.Fatalf("tampered: %+v", st)
	}

	// Generation deleted at rest → broken naming it; Build repairs it.
	must(t, os.RemoveAll(res.Generation))
	st, _ = b.Check(context.Background())
	if st.State != StateBroken || !strings.Contains(st.Reason, filepath.Base(res.Generation)) {
		t.Fatalf("deleted: %+v", st)
	}
	res2, err := b.Build(context.Background())
	must(t, err)
	if res2.Generation != res.Generation || res2.Reused {
		t.Fatalf("rebuild: %+v", res2)
	}
	if st, _ = b.Check(context.Background()); st.State != StateHealthy {
		t.Fatalf("after rebuild: %+v", st)
	}

	// Probe failure → broken.
	f.failNext = "-I -c import cambrian_agent_sdk"
	if st, _ = b.Check(context.Background()); st.State != StateBroken || !strings.Contains(st.Reason, "health probe") {
		t.Fatalf("probe failure: %+v", st)
	}
}

func TestBuildInstallFailureNamesInstallerOutput(t *testing.T) {
	f := newFixture(t)
	f.failNext = "pip install"
	_, err := f.builder(t).Build(context.Background())
	if err == nil || !strings.Contains(err.Error(), "last installer line") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, fileCurrent)); !os.IsNotExist(err) {
		t.Fatal("a failed build must not write current.json")
	}
	entries, _ := os.ReadDir(filepath.Join(f.root, dirBase))
	if len(entries) != 0 {
		t.Fatalf("a failed build must leave no generation: %v", entries)
	}
}

func TestBuildWithDevSDKWheel(t *testing.T) {
	f := newFixture(t)
	b := f.builder(t)
	wheel := filepath.Join(t.TempDir(), "cambrian_agent_sdk-0.1.5-py3-none-any.whl")
	must(t, os.WriteFile(wheel, []byte("wheel"), 0o644))
	b.SDKWheel = wheel
	res, err := b.Build(context.Background())
	must(t, err)
	var m Marker
	must(t, readJSON(filepath.Join(res.Generation, fileMarker), &m))
	if m.Source != "release-lock+dev-sdk-wheel" || m.Inputs.SDKWheelSHA256 != sum([]byte("wheel")) {
		t.Fatalf("marker %+v", m)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, wheel) {
		t.Fatalf("wheel not installed: %s", joined)
	}
	// A boot builder (no wheel) sees the dev generation as current, not stale.
	boot := f.builder(t)
	st, err := boot.Check(context.Background())
	must(t, err)
	if st.State != StateHealthy {
		t.Fatalf("dev generation must stay current at boot: %+v", st)
	}
}

func TestEnvReadyGate(t *testing.T) {
	f := newFixture(t)

	// build_on_boot off and nothing built → refused, fix named.
	off := NewEnv(f.builder(t), false)
	off.Start(context.Background())
	err := off.Ready()
	if err == nil || !strings.Contains(err.Error(), FixCommand) || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("refusal must name the state and the fix: %v", err)
	}
	off.Stop()

	// build_on_boot on → background build, then ready.
	on := NewEnv(f.builder(t), true)
	on.Start(context.Background())
	on.Wait()
	if err := on.Ready(); err != nil {
		t.Fatalf("after the background build: %v", err)
	}
	if on.Status().Interpreter == "" {
		t.Fatal("interpreter unset after build")
	}

	// A failed background build refuses with the cause and the fix.
	f2 := newFixture(t)
	f2.failNext = "pip install"
	bad := NewEnv(f2.builder(t), true)
	bad.Start(context.Background())
	bad.Wait()
	if err := bad.Ready(); err == nil || !strings.Contains(err.Error(), "build failed") || !strings.Contains(err.Error(), FixCommand) {
		t.Fatalf("got %v", err)
	}
}

func TestEnvReadyWhileBuilding(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	b := f.builder(t)
	b.run = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "pip install") {
			<-release
		}
		return f.run(ctx, env, name, args...)
	}
	e := NewEnv(b, true)
	e.Start(context.Background())
	err := e.Ready()
	if !errors.Is(err, ErrBuilding) || !strings.Contains(err.Error(), "step") {
		t.Fatalf("while building: %v", err)
	}
	close(release)
	e.Wait()
	if err := e.Ready(); err != nil {
		t.Fatal(err)
	}
}

// ENG-2105: ReadyC is the explicit readiness signal a boot-time Python agent
// start waits on — open while the background build runs, closed once it
// finishes healthy; closed at once when the boot check finds a usable env;
// never closed after a failed build or with build_on_boot off and nothing built.
func TestEnvReadyCSignalsTheBuiltEnv(t *testing.T) {
	isClosed := func(c <-chan struct{}) bool {
		select {
		case <-c:
			return true
		default:
			return false
		}
	}
	f := newFixture(t)
	release := make(chan struct{})
	b := f.builder(t)
	b.run = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "pip install") {
			<-release
		}
		return f.run(ctx, env, name, args...)
	}
	e := NewEnv(b, true)
	e.Start(context.Background())
	if isClosed(e.ReadyC()) {
		t.Fatal("ReadyC closed while the env is still building")
	}
	close(release)
	select {
	case <-e.ReadyC():
	case <-time.After(10 * time.Second):
		t.Fatal("ReadyC not closed after the background build finished")
	}
	if err := e.Ready(); err != nil {
		t.Fatalf("ReadyC closed but Ready refuses: %v", err)
	}
	e.Wait()

	// The next boot finds the generation healthy: ready at once, no build.
	again := NewEnv(f.builder(t), true)
	again.Start(context.Background())
	if !isClosed(again.ReadyC()) {
		t.Fatal("a healthy env at the boot check must signal ready at once")
	}
	again.Stop()

	// Nothing built and build_on_boot off: never ready.
	off := NewEnv(newFixture(t).builder(t), false)
	off.Start(context.Background())
	if isClosed(off.ReadyC()) {
		t.Fatal("an unusable env signalled ready")
	}

	// A failed build: never ready.
	f2 := newFixture(t)
	f2.failNext = "pip install"
	bad := NewEnv(f2.builder(t), true)
	bad.Start(context.Background())
	bad.Wait()
	if isClosed(bad.ReadyC()) {
		t.Fatal("a failed build signalled ready")
	}
}

// ADR-0157 D7.7.5 (P2): PrepareGeneration runs on the generation's build
// directory while it is still EMPTY — before the interpreter is extracted or
// anything installed — so an inheritable grant on it reaches every file; the
// marker then records granted. A failing hook builds anyway, ungranted.
func TestBuildPreGrantsTheEmptyGeneration(t *testing.T) {
	f := newFixture(t)
	b := f.builder(t)
	var seen []string
	b.PrepareGeneration = func(dir string) error {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range ents {
			seen = append(seen, e.Name())
		}
		f.mu.Lock()
		calls := len(f.calls)
		f.mu.Unlock()
		if calls != 0 {
			t.Errorf("installer ran before the pre-grant: %v", f.calls)
		}
		if filepath.Dir(dir) != filepath.Join(f.root, dirBase) || !strings.HasPrefix(filepath.Base(dir), ".build-") {
			t.Errorf("hook ran on %s, want the build directory under base/", dir)
		}
		return nil
	}
	res, err := b.Build(context.Background())
	must(t, err)
	if len(seen) != 0 {
		t.Fatalf("the build directory was not empty at the pre-grant: %v", seen)
	}
	var m Marker
	must(t, readJSON(filepath.Join(res.Generation, fileMarker), &m))
	if !m.Granted {
		t.Fatal("marker must record the pre-grant")
	}
	st, err := b.Check(context.Background())
	must(t, err)
	if !st.Granted || st.Home != res.Generation {
		t.Fatalf("status %+v: want Granted and Home %s", st, res.Generation)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(res.Generation)
		must(t, err)
		if fi.Mode().Perm()&0o005 != 0o005 {
			t.Fatalf("generation mode %v: agents under another uid must read it", fi.Mode().Perm())
		}
	}

	f2 := newFixture(t)
	b2 := f2.builder(t)
	b2.PrepareGeneration = func(string) error { return errors.New("no WRITE_DAC") }
	res2, err := b2.Build(context.Background())
	must(t, err)
	var m2 Marker
	must(t, readJSON(filepath.Join(res2.Generation, fileMarker), &m2))
	if m2.Granted {
		t.Fatal("a failed pre-grant must be recorded as not granted")
	}
}

// Env.Interpreter is what the agent launch path reads per spawn (P2): the
// real interpreter, its generation directory and the pre-grant once usable;
// the gate's refusal before.
func TestEnvInterpreter(t *testing.T) {
	f := newFixture(t)
	off := NewEnv(f.builder(t), false)
	off.Start(context.Background())
	if _, err := off.Interpreter(); err == nil || !strings.Contains(err.Error(), FixCommand) {
		t.Fatalf("missing env: %v", err)
	}
	b := f.builder(t)
	b.PrepareGeneration = func(string) error { return nil }
	on := NewEnv(b, true)
	on.Start(context.Background())
	on.Wait()
	pi, err := on.Interpreter()
	must(t, err)
	st := on.Status()
	if pi.Path != st.Interpreter || pi.Home != filepath.Join(f.root, dirBase, st.Generation) || !pi.Granted {
		t.Fatalf("interpreter %+v vs status %+v", pi, st)
	}
	if filepath.Dir(filepath.Dir(pi.Path)) != pi.Home {
		t.Fatalf("interpreter %s is not inside its generation %s", pi.Path, pi.Home)
	}
}
