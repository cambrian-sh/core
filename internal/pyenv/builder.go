package pyenv

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SDKProject is the agent SDK's project name; its import is the health probe.
const (
	SDKProject = "cambrian-agent-sdk"
	sdkModule  = "cambrian_agent_sdk"
)

// Layout under the env root (D7.1.2). Only the kernel writes here; agents are
// granted read access to the current base generation only (P2).
const (
	dirDownloads = "downloads" // verified archives, kept for rebuilds and offline re-use
	dirInstaller = "installer" // the extracted pinned installer (kernel-only)
	dirBase      = "base"      // immutable base generations
	dirCache     = "cache"     // the installer's wheel cache (kernel-only)
	fileCurrent  = "current.json"
	fileMarker   = "generation.json"
)

// Inputs are everything a base generation is built from. Their digest names the
// generation (AGENTS.md §2.6: content-derived, never clock-derived), so the same
// inputs always land in the same directory and a changed input is a new one.
type Inputs struct {
	Platform         string `json:"platform"`
	PythonVersion    string `json:"python_version"`
	Python           Asset  `json:"python"`
	InstallerVersion string `json:"installer_version"`
	Installer        Asset  `json:"installer"`
	LockSHA256       string `json:"lock_sha256"`
	// SDKWheelSHA256 is set when a dev tree installs its locally built SDK
	// wheel instead of the lock's published SDK (D7.4.5, owner A1-Q4).
	SDKWheelSHA256 string `json:"sdk_wheel_sha256,omitempty"`
}

// ID is the full hex digest of the inputs.
func (in Inputs) ID() string {
	b, _ := json.Marshal(in) // fixed struct of strings: cannot fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Dir is the generation's directory name: the first 16 hex characters of ID.
// A 64-character path segment pushes torch's deepest files past Windows'
// MAX_PATH under <prefix>\runtime\python\base\; 64 bits still make an
// accidental collision implausible, and the marker carries the full digest.
func (in Inputs) Dir() string { return in.ID()[:16] }

// Marker is generation.json, written into every generation (D7.1.2).
type Marker struct {
	Kind              string `json:"kind"`
	ID                string `json:"id"`
	Inputs            Inputs `json:"inputs"`
	Interpreter       string `json:"interpreter"` // relative to the generation
	InterpreterSHA256 string `json:"interpreter_sha256"`
	// Source is the audit label of what was installed: "release-lock" (the
	// release's base lock, recorded without an approval card, owner A1-Q3), with
	// "+dev-sdk-wheel" when a dev tree's SDK wheel replaced the published SDK.
	Source string `json:"source"`
	// Actor is who built it: the setup user or system:kernel (boot builds).
	Actor    string `json:"actor"`
	Packages int    `json:"packages"`
	Health   string `json:"health"`
	// Granted records that PrepareGeneration put the agent sandbox's read grant
	// on the generation directory before anything was extracted or installed
	// into it, so every file inherited it (ADR-0157 D7.7.5; Windows
	// AppContainer). The launch path then skips the per-file grant walk.
	Granted bool      `json:"granted"`
	BuiltAt time.Time `json:"built_at"`
}

// Current is current.json: which base generation new spawns use (D7.1.2).
type Current struct {
	Base        string `json:"base"`
	Interpreter string `json:"interpreter"` // absolute path of the real interpreter
}

// runFunc runs a command and returns its combined output.
type runFunc func(ctx context.Context, env []string, name string, args ...string) (string, error)

func execRun(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Builder builds the base generation. One code path for setup and boot
// (D7.11.1).
type Builder struct {
	Root     string // the env root (execution.agents.python.root, resolved)
	Pins     PinTable
	Platform string
	Sources  Sources
	// Lock is the hashed base lock (agents/base.lock, embedded in the release).
	Lock []byte
	// SDKWheel is a locally built SDK wheel (dev trees); "" uses the lock's SDK.
	SDKWheel string
	// Actor is recorded in the marker ("system:kernel" when empty).
	Actor      string
	HTTPClient *http.Client
	// Progress, when set, receives each build step's name.
	Progress func(step string)
	// PrepareGeneration, when set, runs on a new generation's (still empty)
	// build directory before the interpreter is extracted into it. The
	// composition root sets it to the agent sandbox's grant on Windows
	// (sandbox.GrantAgentRuntimeRead): an inheritable read ACE on the empty
	// directory reaches every file the build then creates, so the launch path
	// never walks the multi-GB tree (D7.7.5). nil, or an error, records the
	// generation as not granted (the launch path then grants it as before).
	PrepareGeneration func(dir string) error

	run runFunc // nil ⇒ execRun; tests substitute it
}

// Result is a finished build.
type Result struct {
	Generation  string // absolute path
	Interpreter string // absolute path of the real interpreter
	ID          string
	Reused      bool // the generation already existed and was healthy
	Packages    int
}

func (b *Builder) runner() runFunc {
	if b.run != nil {
		return b.run
	}
	return execRun
}

func (b *Builder) step(s string) {
	if b.Progress != nil {
		b.Progress(s)
	}
}

// Inputs resolves the build inputs and parses the lock.
func (b *Builder) Inputs() (Inputs, Lock, error) {
	lock, err := ParseLock(b.Lock)
	if err != nil {
		return Inputs{}, Lock{}, err
	}
	py, err := b.Pins.PythonAsset(b.Platform)
	if err != nil {
		return Inputs{}, Lock{}, err
	}
	inst, err := b.Pins.InstallerAsset(b.Platform)
	if err != nil {
		return Inputs{}, Lock{}, err
	}
	in := Inputs{
		Platform: b.Platform, PythonVersion: b.Pins.Python.Version, Python: py,
		InstallerVersion: b.Pins.Installer.Version, Installer: inst, LockSHA256: lock.SHA256,
	}
	if b.SDKWheel != "" {
		sum, err := fileSHA256(b.SDKWheel)
		if err != nil {
			return Inputs{}, Lock{}, fmt.Errorf("SDK wheel: %w", err)
		}
		in.SDKWheelSHA256 = sum
	}
	return in, lock, nil
}

func (b *Builder) client() *http.Client {
	if b.HTTPClient != nil {
		return b.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// Build makes the base generation for the current inputs current. An existing
// healthy generation with the same inputs is reused (a rebuild of approved
// inputs needs nothing new); a broken one is deleted and rebuilt. A failure
// leaves the previous current generation in place (failure table).
func (b *Builder) Build(ctx context.Context) (Result, error) {
	in, lock, err := b.Inputs()
	if err != nil {
		return Result{}, err
	}
	id := in.ID()
	baseDir := filepath.Join(b.Root, dirBase)
	gen := filepath.Join(baseDir, in.Dir())
	interp := filepath.Join(gen, filepath.FromSlash(interpreterRel(b.Platform)))

	if _, err := os.Stat(gen); err == nil {
		st := inspect(ctx, b.runner(), b.Root, in.Dir(), in)
		if st.State == StateHealthy {
			if err := writeCurrent(b.Root, Current{Base: in.Dir(), Interpreter: interp}); err != nil {
				return Result{}, err
			}
			return Result{Generation: gen, Interpreter: interp, ID: id, Reused: true, Packages: st.Packages}, nil
		}
		b.step("removing broken generation " + in.Dir() + " (" + st.Reason + ")")
		if err := os.RemoveAll(gen); err != nil {
			return Result{}, fmt.Errorf("remove broken generation %s: %w", gen, err)
		}
	}

	pyArchive, uv, err := b.prepare(ctx, in)
	if err != nil {
		return Result{}, err
	}

	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return Result{}, err
	}
	tmp, err := os.MkdirTemp(baseDir, ".build-"+in.Dir()+"-")
	if err != nil {
		return Result{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(tmp)
		}
	}()
	// MkdirTemp makes the directory 0700. The generation is read by agents
	// that may run under another uid (the docker runner's non-root user, the
	// separate uid of a root kernel, D2.2), so it is world-readable like the
	// rest of the tree; only the kernel writes (D7.1.3).
	if err := os.Chmod(tmp, 0o755); err != nil {
		return Result{}, err
	}
	granted := false
	if b.PrepareGeneration != nil {
		if err := b.PrepareGeneration(tmp); err != nil {
			b.step("generation not pre-granted (" + err.Error() + "); agents launch with the per-file grant")
		} else {
			granted = true
		}
	}
	b.step("extracting Python " + in.PythonVersion)
	if err := extractTarGz(pyArchive, tmp, "python/"); err != nil {
		return Result{}, fmt.Errorf("extract interpreter: %w", err)
	}
	tmpInterp := filepath.Join(tmp, filepath.FromSlash(interpreterRel(b.Platform)))
	if _, err := os.Stat(tmpInterp); err != nil {
		return Result{}, fmt.Errorf("extracted build has no interpreter at %s", interpreterRel(b.Platform))
	}

	// The release lock, minus the published SDK when a dev wheel replaces it.
	var exclude []string
	if b.SDKWheel != "" {
		exclude = append(exclude, SDKProject)
	}
	lockFile := tmp + ".lock.txt"
	if err := os.WriteFile(lockFile, lock.Render(exclude...), 0o644); err != nil {
		return Result{}, err
	}
	defer os.Remove(lockFile)

	b.step(fmt.Sprintf("installing the base lock (%d packages, hash-checked, wheels only)", len(lock.Requirements)))
	args := append(b.installArgs(tmpInterp), "--require-hashes", "--only-binary", ":all:", "-r", lockFile)
	if out, err := b.runner()(ctx, installerEnv(), uv, args...); err != nil {
		return Result{}, fmt.Errorf("install base lock: %w: %s", err, lastLines(out, 6))
	}
	source := "release-lock"
	if b.SDKWheel != "" {
		b.step("installing the local SDK wheel " + filepath.Base(b.SDKWheel))
		args := append(b.installArgs(tmpInterp), b.SDKWheel)
		if out, err := b.runner()(ctx, installerEnv(), uv, args...); err != nil {
			return Result{}, fmt.Errorf("install SDK wheel: %w: %s", err, lastLines(out, 6))
		}
		source += "+dev-sdk-wheel"
	}

	b.step("health probe")
	if err := probe(ctx, b.runner(), tmpInterp); err != nil {
		return Result{}, err
	}
	interpSum, err := fileSHA256(tmpInterp)
	if err != nil {
		return Result{}, err
	}
	actor := b.Actor
	if actor == "" {
		actor = "system:kernel"
	}
	m := Marker{
		Kind: "base", ID: id, Inputs: in, Interpreter: interpreterRel(b.Platform), InterpreterSHA256: interpSum,
		Source: source, Actor: actor, Packages: len(lock.Requirements), Health: "ok", Granted: granted,
		BuiltAt: time.Now().UTC(),
	}
	if err := writeJSON(filepath.Join(tmp, fileMarker), m); err != nil {
		return Result{}, err
	}
	if err := os.Rename(tmp, gen); err != nil {
		// A concurrent build of the same inputs may have won the rename.
		if st := inspect(ctx, b.runner(), b.Root, in.Dir(), in); st.State != StateHealthy {
			return Result{}, fmt.Errorf("activate generation %s: %w", gen, err)
		}
	} else {
		keep = true
	}
	if err := writeCurrent(b.Root, Current{Base: in.Dir(), Interpreter: interp}); err != nil {
		return Result{}, err
	}
	return Result{Generation: gen, Interpreter: interp, ID: id, Packages: len(lock.Requirements)}, nil
}

// prepare verifies (downloading when allowed) the pinned interpreter and
// installer archives and extracts the installer; it returns the interpreter
// archive and the installer binary.
func (b *Builder) prepare(ctx context.Context, in Inputs) (pyArchive, uv string, err error) {
	downloads := filepath.Join(b.Root, dirDownloads)
	b.step("verifying interpreter archive " + in.Python.Name)
	pyArchive, err = ensureArchive(ctx, b.client(), downloads, in.Python, orDefault(b.Sources.PythonMirror, b.Pins.Python.BaseURL), b.Sources.Offline)
	if err != nil {
		return "", "", err
	}
	b.step("verifying installer archive " + in.Installer.Name)
	instArchive, err := ensureArchive(ctx, b.client(), downloads, in.Installer, orDefault(b.Sources.InstallerMirror, b.Pins.Installer.BaseURL), b.Sources.Offline)
	if err != nil {
		return "", "", err
	}
	uv = filepath.Join(b.Root, dirInstaller, b.Pins.Installer.Version+"-"+b.Platform, installerBinary(b.Platform))
	if err := ensureInstaller(instArchive, installerBinary(b.Platform), uv); err != nil {
		return "", "", fmt.Errorf("extract installer: %w", err)
	}
	return pyArchive, uv, nil
}

// BuildSDKWheel builds a dev tree's local SDK (sdkDir, the directory holding
// sdk/pyproject.toml) into a wheel with the pinned installer and the pinned
// interpreter, and returns its path for Builder.SDKWheel (D7.4.5, owner A1-Q4:
// an editable install would point agents at the repository, which is not
// granted). The interpreter is a scratch extraction under cache/, never the
// host's Python. The build backend comes from the configured index or
// wheelhouse; it runs only in dev trees, never on a released install.
func (b *Builder) BuildSDKWheel(ctx context.Context, sdkDir string) (string, error) {
	saved := b.SDKWheel
	b.SDKWheel = ""
	in, _, err := b.Inputs()
	b.SDKWheel = saved
	if err != nil {
		return "", err
	}
	pyArchive, uv, err := b.prepare(ctx, in)
	if err != nil {
		return "", err
	}
	scratch := filepath.Join(b.Root, dirCache, "wheel-build-python-"+in.Python.SHA256[:16])
	interp := filepath.Join(scratch, filepath.FromSlash(interpreterRel(b.Platform)))
	if _, err := os.Stat(interp); err != nil {
		_ = os.RemoveAll(scratch)
		b.step("extracting a scratch Python for the SDK wheel build")
		if err := extractTarGz(pyArchive, scratch, "python/"); err != nil {
			_ = os.RemoveAll(scratch)
			return "", fmt.Errorf("extract scratch interpreter: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(b.Root, dirCache), 0o755); err != nil {
		return "", err
	}
	out, err := os.MkdirTemp(filepath.Join(b.Root, dirCache), "sdk-wheel-")
	if err != nil {
		return "", err
	}
	b.step("building the local SDK wheel from " + sdkDir)
	args := []string{"build", "--wheel", "--no-config", "--python", interp, "--out-dir", out,
		"--cache-dir", filepath.Join(b.Root, dirCache)}
	if b.Sources.Offline {
		args = append(args, "--offline", "--no-index")
	} else {
		args = append(args, "--index-url", orDefault(b.Sources.IndexURL, DefaultIndexURL))
	}
	for _, w := range b.Sources.Wheelhouse {
		if p, ok := localPath(w); ok {
			w = p
		}
		args = append(args, "--find-links", w)
	}
	args = append(args, sdkDir)
	if o, err := b.runner()(ctx, installerEnv(), uv, args...); err != nil {
		_ = os.RemoveAll(out)
		return "", fmt.Errorf("build SDK wheel: %w: %s", err, lastLines(o, 6))
	}
	wheels, _ := filepath.Glob(filepath.Join(out, "*.whl"))
	if len(wheels) != 1 {
		_ = os.RemoveAll(out)
		return "", fmt.Errorf("build SDK wheel: expected one wheel in %s, found %d", out, len(wheels))
	}
	return wheels[0], nil
}

// ensureInstaller extracts the installer binary from its verified archive
// unless an extraction with a matching recorded digest is already there (a
// running installer cannot be overwritten on Windows, and re-extracting ~50 MB
// per build buys nothing when the bytes are unchanged).
func ensureInstaller(archive, base, dest string) error {
	sidecar := dest + ".sha256"
	if want, err := os.ReadFile(sidecar); err == nil {
		if got, err := fileSHA256(dest); err == nil && got == strings.TrimSpace(string(want)) {
			return nil
		}
	}
	tmp := dest + ".new"
	if err := extractOne(archive, base, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	sum, err := fileSHA256(tmp)
	if err != nil {
		return err
	}
	_ = os.Remove(dest)
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.WriteFile(sidecar, []byte(sum+"\n"), 0o644)
}

// installArgs are the installer flags every kernel install uses (D7.7):
// target the generation's own interpreter, copy files (no hardlinks from the
// cache, so a generation's ACLs are its own on Windows, D7.7.5), never a
// dependency the lock does not name, and only the configured sources.
func (b *Builder) installArgs(interp string) []string {
	args := []string{"pip", "install", "--python", interp, "--no-deps", "--link-mode", "copy",
		"--cache-dir", filepath.Join(b.Root, dirCache), "--no-config",
		// The kernel owns this interpreter; the marker (when a build carries
		// one) protects distribution-managed Pythons, which this is not.
		"--break-system-packages"}
	if b.Sources.Offline {
		args = append(args, "--offline", "--no-index")
	} else {
		args = append(args, "--index-url", orDefault(b.Sources.IndexURL, DefaultIndexURL))
	}
	for _, w := range b.Sources.Wheelhouse {
		if p, ok := localPath(w); ok {
			w = p
		}
		args = append(args, "--find-links", w)
	}
	return args
}

// installerEnv is the kernel's environment without installer/pip/Python
// overrides, so an operator's shell settings cannot redirect a kernel install,
// and with uv's own interpreter downloads off (the pin table is the only
// source of Python).
func installerEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		ku := strings.ToUpper(k)
		if strings.HasPrefix(ku, "UV_") || strings.HasPrefix(ku, "PIP_") || ku == "PYTHONPATH" || ku == "PYTHONHOME" || ku == "VIRTUAL_ENV" || ku == "CONDA_PREFIX" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "UV_PYTHON_DOWNLOADS=never", "UV_NO_PROGRESS=1")
}

// probe imports the SDK in isolated mode with the generation's interpreter.
func probe(ctx context.Context, run runFunc, interp string) error {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := run(cctx, installerEnv(), interp, "-I", "-c", "import "+sdkModule)
	if err != nil {
		return fmt.Errorf("health probe (python -I -c \"import %s\") failed: %w: %s", sdkModule, err, lastLines(out, 4))
	}
	return nil
}

func writeCurrent(root string, c Current) error {
	return writeJSON(filepath.Join(root, fileCurrent), c)
}

// writeJSON writes v atomically (temp file + rename in the same directory).
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:]) // a name collision only fails this write
	tmp := path + ".tmp-" + hex.EncodeToString(rnd[:])
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, " | "))
}

// errIsNotExist reports a missing file through any wrapping.
func errIsNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
