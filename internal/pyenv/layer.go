package pyenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Per-agent package layers (ADR-0157 D7.5, P3).
//
// Mechanism, chosen over the ADR's option A as written (a `venv
// --system-site-packages` per agent): a layer is an immutable generation
// holding only the agent's extra distributions in `site-packages/`, installed
// by the kernel with `uv pip install --target`, plus `boot/sitecustomize.py`
// that `site.addsitedir`s it. The agent runs the base generation's REAL
// interpreter with PYTHONPATH=<layer>/boot. Why not a venv: on Windows a venv's
// python.exe is a redirector that spawns the base python.exe, and the
// AppContainer blocks exactly that kind of child launch (ENG-2005, P1/P2
// observations); a layer the real interpreter loads has no launcher at all.
// It keeps A's properties: no duplication of the base, `.pth` files in the
// layer are processed (addsitedir), and the base's own directories stay first
// on sys.path, so a layer can add packages but never shadow a base version —
// which resolution also enforces (the base is the constraint set).

const (
	dirLayers     = "layers"
	layerSite     = "site-packages"
	layerBoot     = "boot"
	layerCustom   = "sitecustomize.py"
	layerMarkerKd = "layer"
)

// ErrUnresolvable is wrapped by a resolution that cannot produce a lock: the
// version is not on the index or wheelhouse, it has no wheel for this platform
// (wheels only, D7.7.3), or it conflicts with the base env (D7.5 rule 1).
var ErrUnresolvable = errors.New("python package unresolvable")

// ErrInBase is wrapped when a declared package is already in the base env: at
// the same version it needs no layer (and no card); at another version the
// declaration is refused, because a layer can never change a base version.
var ErrInBase = errors.New("python package is part of the base env")

// Layer is one agent's built package layer.
type Layer struct {
	// Dir is the generation directory (absolute): the one tree the agent is
	// shown read-only besides the base generation.
	Dir string
	// Boot is the directory to put on PYTHONPATH (holds sitecustomize.py).
	Boot string
	// Granted records that the build put the agent's own read grant on Dir
	// while it was empty (Windows AppContainer, D7.7.5).
	Granted bool
	ID      string
	Base    string // the base generation directory (absolute) it was built on
	Reused  bool
}

// LayerMarker is a layer generation's generation.json.
type LayerMarker struct {
	Kind     string                   `json:"kind"`
	ID       string                   `json:"id"`
	Agent    string                   `json:"agent"`
	Base     string                   `json:"base"` // base generation directory name
	Platform string                   `json:"platform"`
	Declared []string                 `json:"declared"`
	Entries  []domain.PythonLockEntry `json:"entries"`
	Actor    string                   `json:"actor"`
	Health   string                   `json:"health"`
	Granted  bool                     `json:"granted"`
	BuiltAt  time.Time                `json:"built_at"`
}

// LayerRequest is one layer build.
type LayerRequest struct {
	Agent string
	Base  Interpreter // the base generation (Path, Home) the layer runs on
	// Entries is the union of the agent's approved locks.
	Entries []domain.PythonLockEntry
	// Declared are the declared project names whose top-level modules the
	// health check imports.
	Declared []string
	// Prepare, when set, runs on the empty build directory (the agent's own
	// AppContainer read grant on Windows, sandbox.GrantAgentPrivateRead).
	Prepare func(dir string) error
	Actor   string
}

var safeAgentDir = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// agentDir is the layers/<agent> directory name: the id when it is a safe path
// segment, else a digest of it (an SDK agent id may hold any character).
func agentDir(agent string) string {
	if safeAgentDir.MatchString(agent) && agent != "." && agent != ".." {
		return agent
	}
	sum := sha256.Sum256([]byte(agent))
	return "id-" + hex.EncodeToString(sum[:])[:24]
}

// layerID names a layer generation by its content (AGENTS.md §2.6): the base
// generation, the platform and every distribution with its hashes.
func layerID(platform, baseGen string, entries []domain.PythonLockEntry) string {
	parts := []string{"layer", platform, baseGen}
	for _, e := range sortedEntries(entries) {
		h := append([]string(nil), e.Hashes...)
		sort.Strings(h)
		parts = append(parts, NormalizeName(e.Name)+"=="+e.Version+"|"+strings.Join(h, ","))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func sortedEntries(in []domain.PythonLockEntry) []domain.PythonLockEntry {
	out := append([]domain.PythonLockEntry(nil), in...)
	sort.Slice(out, func(i, j int) bool { return NormalizeName(out[i].Name) < NormalizeName(out[j].Name) })
	return out
}

// installer returns the verified, extracted pinned installer.
func (b *Builder) installer(ctx context.Context) (string, error) {
	in, _, err := b.Inputs()
	if err != nil {
		return "", err
	}
	_, uv, err := b.prepare(ctx, in)
	return uv, err
}

// sourcesLabel is where packages come from, for the card (D7.8.6).
func (b *Builder) sourcesLabel() string {
	var parts []string
	if b.Sources.Offline {
		parts = append(parts, "offline")
	} else {
		parts = append(parts, "index "+orDefault(b.Sources.IndexURL, DefaultIndexURL))
	}
	for _, w := range b.Sources.Wheelhouse {
		parts = append(parts, "wheelhouse "+w)
	}
	return strings.Join(parts, ", ")
}

// BaseDistributions lists the distributions installed in a base generation,
// read by that generation's own interpreter (isolated mode): normalized name →
// version. This, not the release lock, is the layer's constraint set, so a dev
// tree's SDK wheel or a stale-but-usable base generation is honoured exactly.
func (b *Builder) BaseDistributions(ctx context.Context, base Interpreter) (map[string]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	script := "import importlib.metadata as m, json, sys\n" +
		"sys.stdout.write(json.dumps({d.metadata['Name']: d.version for d in m.distributions() if d.metadata['Name']}))\n"
	out, err := b.runner()(cctx, installerEnv(), base.Path, "-I", "-c", script)
	if err != nil {
		return nil, fmt.Errorf("list the base env's distributions: %w: %s", err, lastLines(out, 4))
	}
	// The interpreter may print warnings before the JSON; take the last line.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var raw map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(lines[len(lines)-1])), &raw); err != nil {
		return nil, fmt.Errorf("list the base env's distributions: %w", err)
	}
	dists := make(map[string]string, len(raw))
	for n, v := range raw {
		dists[NormalizeName(n)] = v
	}
	return dists, nil
}

// ResolvePackage resolves one declared package for this platform against the
// base generation (D7.7.2, resolve step): the pinned installer's `pip compile`
// with hashes, wheels only, the base's installed distributions as constraints,
// and the configured index / wheelhouse / offline sources. The lock holds the
// distributions the layer must add — the package first, then its
// dependencies — never one the base already has.
func (b *Builder) ResolvePackage(ctx context.Context, base Interpreter, pkg domain.PythonPackage) (domain.PythonPackageLock, error) {
	if err := domain.ValidatePythonPackages([]domain.PythonPackage{pkg}); err != nil {
		return domain.PythonPackageLock{}, err
	}
	dists, err := b.BaseDistributions(ctx, base)
	if err != nil {
		return domain.PythonPackageLock{}, err
	}
	name := NormalizeName(pkg.Name)
	if v, ok := dists[name]; ok {
		if v == pkg.Version && len(pkg.Extras) == 0 {
			return domain.PythonPackageLock{}, fmt.Errorf("%s==%s: %w at that version; it needs no layer", pkg.Name, pkg.Version, ErrInBase)
		}
		if v != pkg.Version {
			return domain.PythonPackageLock{}, fmt.Errorf("%s==%s: %w at %s, and a layer cannot change a base version (the operator may change the base instead): %w",
				pkg.Name, pkg.Version, ErrInBase, v, ErrUnresolvable)
		}
	}
	uv, err := b.installer(ctx)
	if err != nil {
		return domain.PythonPackageLock{}, err
	}
	work := filepath.Join(b.Root, dirCache)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return domain.PythonPackageLock{}, err
	}
	tmp, err := os.MkdirTemp(work, "resolve-")
	if err != nil {
		return domain.PythonPackageLock{}, err
	}
	defer os.RemoveAll(tmp)
	names := make([]string, 0, len(dists))
	for n := range dists {
		names = append(names, n)
	}
	sort.Strings(names)
	var cons strings.Builder
	for _, n := range names {
		cons.WriteString(n + "==" + dists[n] + "\n")
	}
	reqFile, consFile, outFile := filepath.Join(tmp, "requirements.in"), filepath.Join(tmp, "constraints.txt"), filepath.Join(tmp, "lock.txt")
	if err := os.WriteFile(reqFile, []byte(pkg.Spec()+"\n"), 0o644); err != nil {
		return domain.PythonPackageLock{}, err
	}
	if err := os.WriteFile(consFile, []byte(cons.String()), 0o644); err != nil {
		return domain.PythonPackageLock{}, err
	}
	args := []string{"pip", "compile", reqFile, "--constraint", consFile, "--output-file", outFile,
		"--python", base.Path, "--generate-hashes", "--only-binary", ":all:", "--no-header", "--no-annotate",
		"--no-config", "--cache-dir", filepath.Join(b.Root, dirCache)}
	args = append(args, b.sourceArgs()...)
	if out, err := b.runner()(ctx, installerEnv(), uv, args...); err != nil {
		return domain.PythonPackageLock{}, fmt.Errorf("%s: %w: %s", pkg.Spec(), ErrUnresolvable, lastLines(out, 8))
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		return domain.PythonPackageLock{}, fmt.Errorf("%s: resolver wrote no lock: %w", pkg.Spec(), err)
	}
	lock, err := ParseLock(raw)
	if err != nil {
		return domain.PythonPackageLock{}, fmt.Errorf("%s: resolver lock: %w", pkg.Spec(), err)
	}
	var head *domain.PythonLockEntry
	var rest []domain.PythonLockEntry
	for _, r := range lock.Requirements {
		n := NormalizeName(r.Name)
		if v, inBase := dists[n]; inBase {
			if v != r.Version {
				return domain.PythonPackageLock{}, fmt.Errorf("%s: %w: it needs %s==%s but the base env has %s", pkg.Spec(), ErrUnresolvable, r.Name, r.Version, v)
			}
			continue
		}
		e := domain.PythonLockEntry{Name: n, Version: r.Version, Hashes: append([]string(nil), r.Hashes...)}
		if n == name {
			head = &e
			continue
		}
		rest = append(rest, e)
	}
	if head == nil && dists[name] == "" {
		return domain.PythonPackageLock{}, fmt.Errorf("%s: %w: the resolver's lock does not contain it", pkg.Spec(), ErrUnresolvable)
	}
	entries := make([]domain.PythonLockEntry, 0, len(rest)+1)
	if head != nil {
		entries = append(entries, *head)
	}
	entries = append(entries, sortedEntries(rest)...)
	return domain.PythonPackageLock{Entries: entries, Index: b.sourcesLabel()}, nil
}

// sourceArgs are the installer's source flags: the index (or offline) and the
// wheelhouse directories.
func (b *Builder) sourceArgs() []string {
	var args []string
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

// LayerPath is where a layer generation with these inputs lives, built or not.
func (b *Builder) LayerPath(agent string, base Interpreter, entries []domain.PythonLockEntry) string {
	id := layerID(b.Platform, filepath.Base(base.Home), entries)
	return filepath.Join(b.Root, dirLayers, agentDir(agent), id[:16])
}

// FindLayer returns the agent's built, intact layer generation for these
// inputs, if there is one (no build, no probe: the launch path's check).
func (b *Builder) FindLayer(agent string, base Interpreter, entries []domain.PythonLockEntry) (Layer, bool) {
	id := layerID(b.Platform, filepath.Base(base.Home), entries)
	gen := filepath.Join(b.Root, dirLayers, agentDir(agent), id[:16])
	m, ok := readLayerMarker(gen, id)
	if !ok {
		return Layer{}, false
	}
	return Layer{Dir: gen, Boot: filepath.Join(gen, layerBoot), Granted: m.Granted, ID: id, Base: filepath.Clean(base.Home), Reused: true}, true
}

func readLayerMarker(gen, id string) (LayerMarker, bool) {
	var m LayerMarker
	if err := readJSON(filepath.Join(gen, fileMarker), &m); err != nil {
		return LayerMarker{}, false
	}
	if m.Kind != layerMarkerKd || m.ID != id || m.Health != "ok" {
		return LayerMarker{}, false
	}
	for _, p := range []string{filepath.Join(gen, layerBoot, layerCustom), filepath.Join(gen, layerSite)} {
		if _, err := os.Stat(p); err != nil {
			return LayerMarker{}, false
		}
	}
	return m, true
}

// sitecustomize is the layer's boot file. It resolves the layer's
// site-packages relative to itself (the generation is built in a temporary
// directory and renamed into place) and appends it with addsitedir, which also
// processes the layer's .pth files; the base's own directories stay first.
const sitecustomize = `# Written by the Cambrian kernel (ADR-0157 D7.5): this agent's package layer.
# The kernel-managed base env's interpreter loads it through PYTHONPATH; the
# layer is read-only to the agent and only the kernel installs into it.
import os as _os
import site as _site

_site.addsitedir(_os.path.join(_os.path.dirname(_os.path.dirname(_os.path.abspath(__file__))), "site-packages"))
del _os, _site
`

// layerHealth checks a layer through the real launch shape (the base
// interpreter with PYTHONPATH=<boot>): the layer's site-packages is on
// sys.path, every locked distribution has its locked version, and every
// declared package's top-level modules import (D7.7.2, health check).
const layerHealth = `import importlib, importlib.metadata as md, json, os, sys
want = json.loads(sys.argv[1]); declared = json.loads(sys.argv[2]); site_dir = os.path.normcase(os.path.abspath(sys.argv[3]))
if site_dir not in [os.path.normcase(os.path.abspath(p)) for p in sys.path]:
    sys.exit("layer site-packages is not on sys.path")
for n, v in want.items():
    got = md.version(n)
    if got != v:
        sys.exit("%s is %s, locked %s" % (n, got, v))
for n in declared:
    d = md.distribution(n)
    tops = [t for t in (d.read_text("top_level.txt") or "").split() if t and not t.startswith("_") and t not in ("test", "tests")]
    if not tops:
        tops = sorted({f.parts[0][:-3] if f.parts[0].endswith(".py") else f.parts[0] for f in (d.files or []) if f.parts and not f.parts[0].endswith((".dist-info", ".data", ".pth")) and (len(f.parts) > 1 or f.parts[0].endswith(".py"))} - {"__pycache__", "bin"})
    for t in tops:
        importlib.import_module(t)
print("ok")
`

// BuildLayer installs a layer generation from approved locks (D7.7.2, install
// step): a fresh build directory (pre-granted to the agent on Windows), the
// pinned installer in hash-checking mode with --no-deps over the full set,
// wheels only, copy link mode, --target the layer's site-packages, the boot
// file, the health check through the launch shape, then an atomic rename. An
// intact generation with the same inputs is reused (a rebuild of an approved
// lock needs nothing new).
func (b *Builder) BuildLayer(ctx context.Context, req LayerRequest) (Layer, error) {
	if len(req.Entries) == 0 {
		return Layer{}, fmt.Errorf("layer for %s: nothing to install", req.Agent)
	}
	if l, ok := b.FindLayer(req.Agent, req.Base, req.Entries); ok {
		return l, nil
	}
	id := layerID(b.Platform, filepath.Base(req.Base.Home), req.Entries)
	parent := filepath.Join(b.Root, dirLayers, agentDir(req.Agent))
	gen := filepath.Join(parent, id[:16])
	if _, err := os.Stat(gen); err == nil {
		if err := os.RemoveAll(gen); err != nil {
			return Layer{}, fmt.Errorf("remove broken layer %s: %w", gen, err)
		}
	}
	uv, err := b.installer(ctx)
	if err != nil {
		return Layer{}, err
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Layer{}, err
	}
	tmp, err := os.MkdirTemp(parent, ".build-"+id[:16]+"-")
	if err != nil {
		return Layer{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(tmp)
		}
	}()
	// World-readable like the base (the docker runner's uid, a root kernel's
	// dropped uid); only the kernel writes, and only this agent's sandbox is
	// shown the directory.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return Layer{}, err
	}
	granted := false
	if req.Prepare != nil {
		if err := req.Prepare(tmp); err == nil {
			granted = true
		}
	}
	var lock strings.Builder
	want := map[string]string{}
	for _, e := range sortedEntries(req.Entries) {
		if len(e.Hashes) == 0 {
			return Layer{}, fmt.Errorf("layer for %s: %s==%s has no hash; refusing to install it", req.Agent, e.Name, e.Version)
		}
		lock.WriteString(e.Name + "==" + e.Version)
		for _, h := range e.Hashes {
			lock.WriteString(" \\\n    --hash=sha256:" + h)
		}
		lock.WriteString("\n")
		want[e.Name] = e.Version
	}
	lockFile := tmp + ".lock.txt"
	if err := os.WriteFile(lockFile, []byte(lock.String()), 0o644); err != nil {
		return Layer{}, err
	}
	defer os.Remove(lockFile)
	site := filepath.Join(tmp, layerSite)
	args := append(b.installArgs(req.Base.Path), "--target", site, "--require-hashes", "--only-binary", ":all:", "-r", lockFile)
	if out, err := b.runner()(ctx, installerEnv(), uv, args...); err != nil {
		return Layer{}, fmt.Errorf("install the layer of %s: %w: %s", req.Agent, err, lastLines(out, 6))
	}
	if err := os.MkdirAll(filepath.Join(tmp, layerBoot), 0o755); err != nil {
		return Layer{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, layerBoot, layerCustom), []byte(sitecustomize), 0o644); err != nil {
		return Layer{}, err
	}
	wantJSON, _ := json.Marshal(want)         // map of strings: cannot fail
	declJSON, _ := json.Marshal(req.Declared) // slice of strings: cannot fail
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	out, err := b.runner()(cctx, append(installerEnv(), "PYTHONPATH="+filepath.Join(tmp, layerBoot)), req.Base.Path,
		"-s", "-c", layerHealth, string(wantJSON), string(declJSON), site)
	cancel()
	if err != nil {
		return Layer{}, fmt.Errorf("layer health check for %s failed: %w: %s", req.Agent, err, lastLines(out, 6))
	}
	actor := req.Actor
	if actor == "" {
		actor = "system:kernel"
	}
	m := LayerMarker{
		Kind: layerMarkerKd, ID: id, Agent: req.Agent, Base: filepath.Base(req.Base.Home), Platform: b.Platform,
		Declared: req.Declared, Entries: sortedEntries(req.Entries), Actor: actor, Health: "ok", Granted: granted,
		BuiltAt: time.Now().UTC(),
	}
	if err := writeJSON(filepath.Join(tmp, fileMarker), m); err != nil {
		return Layer{}, err
	}
	if err := os.Rename(tmp, gen); err != nil {
		if l, ok := b.FindLayer(req.Agent, req.Base, req.Entries); ok {
			return l, nil // a concurrent build of the same inputs won the rename
		}
		return Layer{}, fmt.Errorf("activate layer %s: %w", gen, err)
	}
	keep = true
	return Layer{Dir: gen, Boot: filepath.Join(gen, layerBoot), Granted: granted, ID: id, Base: filepath.Clean(req.Base.Home)}, nil
}
