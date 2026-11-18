package pyenv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// layerRunner fakes the interpreter and the installer for layer work: the base
// generation's distributions, a resolver lock, a --target install, a health
// check; every call recorded.
type layerRunner struct {
	mu         sync.Mutex
	baseJSON   string
	compileOut string
	failHealth bool
	calls      []string
}

func (r *layerRunner) run(_ context.Context, env []string, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := filepath.Base(name) + " " + strings.Join(args, " ")
	r.calls = append(r.calls, line)
	argAfter := func(flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	switch {
	case len(args) >= 2 && args[0] == "-I" && strings.Contains(args[2], "importlib.metadata"):
		return "warning: noise\n" + r.baseJSON + "\n", nil
	case len(args) >= 2 && args[0] == "pip" && args[1] == "compile":
		return "", os.WriteFile(argAfter("--output-file"), []byte(r.compileOut), 0o644)
	case len(args) >= 2 && args[0] == "pip" && args[1] == "install" && argAfter("--target") != "":
		return "", os.MkdirAll(filepath.Join(argAfter("--target"), "polars"), 0o755)
	case len(args) >= 1 && args[0] == "-s":
		var pp string
		for _, kv := range env {
			if strings.HasPrefix(kv, "PYTHONPATH=") {
				pp = kv
			}
		}
		if pp == "" {
			return "no PYTHONPATH", errors.New("exit status 1")
		}
		if r.failHealth {
			return "ModuleNotFoundError: polars", errors.New("exit status 1")
		}
		return "ok", nil
	}
	return "", nil
}

func (r *layerRunner) find(sub string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			return c
		}
	}
	return ""
}

const compiledLock = `polars==1.9.0 \
    --hash=sha256:1111111111111111111111111111111111111111111111111111111111111111 \
    --hash=sha256:2222222222222222222222222222222222222222222222222222222222222222
pyarrow==17.0.0 \
    --hash=sha256:3333333333333333333333333333333333333333333333333333333333333333
numpy==2.1.0 \
    --hash=sha256:4444444444444444444444444444444444444444444444444444444444444444
`

func layerFixture(t *testing.T) (*Builder, *layerRunner, Interpreter) {
	t.Helper()
	f := newFixture(t)
	b := f.builder(t)
	r := &layerRunner{baseJSON: `{"numpy": "2.1.0", "Cambrian_Agent_SDK": "0.1.5"}`, compileOut: compiledLock}
	b.run = r.run
	gen := filepath.Join(f.root, "base", "0123456789abcdef")
	must(t, os.MkdirAll(gen, 0o755))
	return b, r, Interpreter{Path: filepath.Join(gen, "bin", "python3"), Home: gen}
}

// Resolution (D7.7.2): the pinned installer's pip compile with hashes, wheels
// only, the base's installed distributions as constraints; the lock holds only
// what the base lacks, the package first.
func TestResolvePackage_WheelsOnlyHashesAgainstTheBase(t *testing.T) {
	b, r, base := layerFixture(t)
	lock, err := b.ResolvePackage(context.Background(), base, domain.PythonPackage{Name: "Polars", Version: "1.9.0"})
	must(t, err)
	if len(lock.Entries) != 2 || lock.Entries[0].Name != "polars" || lock.Entries[1].Name != "pyarrow" {
		t.Fatalf("lock must be polars then pyarrow, never numpy (in the base): %+v", lock.Entries)
	}
	if len(lock.Entries[0].Hashes) != 2 {
		t.Errorf("every file hash is kept: %+v", lock.Entries[0])
	}
	compile := r.find("pip compile")
	for _, want := range []string{"--generate-hashes", "--only-binary :all:", "--constraint", "--python " + base.Path, "--no-config", "--offline", "--no-index", "--find-links"} {
		if !strings.Contains(compile, want) {
			t.Errorf("compile %q lacks %s", compile, want)
		}
	}
	if !strings.Contains(lock.Index, "offline") {
		t.Errorf("index label: %q", lock.Index)
	}
	// A different file set is a different lock identity.
	other := lock
	other.Entries = append([]domain.PythonLockEntry(nil), lock.Entries...)
	other.Entries[1] = domain.PythonLockEntry{Name: "pyarrow", Version: "17.0.0", Hashes: []string{"9"}}
	if other.SHA256() == lock.SHA256() {
		t.Error("lock identity must cover every file hash")
	}
}

// A declared package already in the base: same version needs nothing (no
// layer, no card); another version is refused naming both versions (a layer
// cannot change a base version, D7.5 rule 1).
func TestResolvePackage_BaseConflicts(t *testing.T) {
	b, r, base := layerFixture(t)
	_, err := b.ResolvePackage(context.Background(), base, domain.PythonPackage{Name: "numpy", Version: "2.1.0"})
	if !errors.Is(err, ErrInBase) || errors.Is(err, ErrUnresolvable) {
		t.Fatalf("same version in base: %v", err)
	}
	_, err = b.ResolvePackage(context.Background(), base, domain.PythonPackage{Name: "numpy", Version: "1.26.4"})
	if !errors.Is(err, ErrUnresolvable) || !strings.Contains(err.Error(), "2.1.0") || !strings.Contains(err.Error(), "1.26.4") {
		t.Fatalf("other version in base: %v", err)
	}
	if r.find("pip compile") != "" {
		t.Error("a base conflict is refused before the resolver runs")
	}
	// A resolver lock that would change a base package is refused.
	r.compileOut = strings.Replace(compiledLock, "numpy==2.1.0", "numpy==1.0.0", 1)
	_, err = b.ResolvePackage(context.Background(), base, domain.PythonPackage{Name: "polars", Version: "1.9.0"})
	if !errors.Is(err, ErrUnresolvable) || !strings.Contains(err.Error(), "numpy") {
		t.Fatalf("lock changing a base version: %v", err)
	}
}

// Install (D7.7.2): hash-checking mode, --no-deps, wheels only, copy link mode,
// --target the layer's site-packages; the boot file; the health check through
// the launch shape (base interpreter + PYTHONPATH); an atomic, content-named
// generation that is reused, and pre-granted when Prepare succeeds.
func TestBuildLayer_InstallsHashCheckedAndReuses(t *testing.T) {
	b, r, base := layerFixture(t)
	entries := []domain.PythonLockEntry{
		{Name: "pyarrow", Version: "17.0.0", Hashes: []string{strings.Repeat("3", 64)}},
		{Name: "polars", Version: "1.9.0", Hashes: []string{strings.Repeat("1", 64)}},
	}
	var prepared string
	l, err := b.BuildLayer(context.Background(), LayerRequest{Agent: "analyst", Base: base, Entries: entries, Declared: []string{"polars"},
		Prepare: func(dir string) error { prepared = dir; return nil }})
	must(t, err)
	if filepath.Dir(l.Dir) != filepath.Join(b.Root, "layers", "analyst") || len(filepath.Base(l.Dir)) != 16 {
		t.Fatalf("layer dir %s", l.Dir)
	}
	if !l.Granted || prepared == "" || filepath.Base(prepared) == filepath.Base(l.Dir) {
		t.Errorf("Prepare must run on the empty build dir and mark the layer granted: %q %+v", prepared, l)
	}
	install := r.find("--target")
	for _, want := range []string{"--require-hashes", "--no-deps", "--only-binary :all:", "--link-mode copy", "--python " + base.Path, "--target " + filepath.Join(filepath.Dir(prepared), filepath.Base(prepared), "site-packages")} {
		if !strings.Contains(install, want) {
			t.Errorf("install %q lacks %s", install, want)
		}
	}
	custom, err := os.ReadFile(filepath.Join(l.Boot, "sitecustomize.py"))
	must(t, err)
	if !strings.Contains(string(custom), "addsitedir") {
		t.Errorf("boot file: %s", custom)
	}
	var m LayerMarker
	must(t, readJSON(filepath.Join(l.Dir, fileMarker), &m))
	if m.Kind != "layer" || m.Agent != "analyst" || m.Base != filepath.Base(base.Home) || len(m.Entries) != 2 || m.Health != "ok" {
		t.Errorf("marker %+v", m)
	}
	// Order of entries does not change the generation; a reuse runs nothing.
	r.calls = nil
	l2, err := b.BuildLayer(context.Background(), LayerRequest{Agent: "analyst", Base: base, Entries: []domain.PythonLockEntry{entries[1], entries[0]}})
	must(t, err)
	if l2.Dir != l.Dir || !l2.Reused || len(r.calls) != 0 {
		t.Fatalf("same inputs must reuse %s without running anything: %+v calls=%v", l.Dir, l2, r.calls)
	}
	// Another agent, same packages: its own generation.
	l3, err := b.BuildLayer(context.Background(), LayerRequest{Agent: "reporter", Base: base, Entries: entries})
	must(t, err)
	if filepath.Dir(l3.Dir) == filepath.Dir(l.Dir) {
		t.Fatal("each agent's layer lives under its own directory")
	}
	// A different base generation is a different layer.
	gen2 := filepath.Join(b.Root, "base", "fedcba9876543210")
	l4, err := b.BuildLayer(context.Background(), LayerRequest{Agent: "analyst", Base: Interpreter{Path: base.Path, Home: gen2}, Entries: entries})
	must(t, err)
	if l4.Dir == l.Dir {
		t.Fatal("a layer is built for one base generation")
	}
}

// A failed health check leaves no generation behind; an entry without a hash
// is never installed.
func TestBuildLayer_FailuresLeaveNothing(t *testing.T) {
	b, r, base := layerFixture(t)
	r.failHealth = true
	entries := []domain.PythonLockEntry{{Name: "polars", Version: "1.9.0", Hashes: []string{strings.Repeat("1", 64)}}}
	_, err := b.BuildLayer(context.Background(), LayerRequest{Agent: "analyst", Base: base, Entries: entries, Declared: []string{"polars"}})
	if err == nil || !strings.Contains(err.Error(), "health") {
		t.Fatalf("health failure: %v", err)
	}
	left, _ := os.ReadDir(filepath.Join(b.Root, "layers", "analyst"))
	if len(left) != 0 {
		t.Fatalf("a failed build must leave nothing: %v", left)
	}
	if _, ok := b.FindLayer("analyst", base, entries); ok {
		t.Fatal("no layer after a failed build")
	}
	_, err = b.BuildLayer(context.Background(), LayerRequest{Agent: "analyst", Base: base, Entries: []domain.PythonLockEntry{{Name: "x", Version: "1"}}})
	if err == nil || !strings.Contains(err.Error(), "no hash") {
		t.Fatalf("an unhashed entry must be refused: %v", err)
	}
}

func TestAgentDirIsASafeSegment(t *testing.T) {
	for in, want := range map[string]string{"analyst": "analyst", "a.b_c-1": "a.b_c-1"} {
		if got := agentDir(in); got != want {
			t.Errorf("agentDir(%q) = %q", in, got)
		}
	}
	for _, in := range []string{"..", "../x", `a\b`, "a/b", strings.Repeat("x", 80), ""} {
		if got := agentDir(in); !strings.HasPrefix(got, "id-") {
			t.Errorf("agentDir(%q) = %q, want a digest", in, got)
		}
	}
}
