// Package pyenv builds and checks the kernel-managed Python environment that
// sandboxed agents run on (ADR-0157 D7, Amendment A1; build stage P1).
//
// The kernel downloads a pinned python-build-standalone interpreter and a pinned
// installer (uv), verifies both against the SHA-256 digests embedded in this
// package (pins.json), and builds an immutable, content-addressed base
// generation under <root>/base/<id>: the extracted interpreter with the release's
// hashed base lock installed into its own site-packages (D7.5 option A). There is
// no virtual environment and no uv trampoline: the generation's real interpreter
// is what agents launch (P2), so the sandbox has exactly one tree to grant.
//
// The host's Python is never used, whatever PATH says (D7.2.3).
package pyenv

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
)

//go:embed pins.json
var pinsJSON []byte

// Asset is one downloadable archive with its expected SHA-256 (lower-case hex).
type Asset struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Release is one pinned artifact family (the interpreter or the installer).
type Release struct {
	Name     string           `json:"name,omitempty"`
	Version  string           `json:"version"`
	BuildTag string           `json:"build_tag,omitempty"`
	BaseURL  string           `json:"base_url"`
	Assets   map[string]Asset `json:"assets"`
}

// ModelAsset is one pinned, platform-independent wheel the base generation
// installs after the base lock (ADR-0157 D7.2.5, ENG-2072): a file published
// only outside any package index, such as spaCy's en_core_web_sm model (a
// GitHub release asset; PyPI's en-core-web-sm project has no files), so it
// cannot be a hashed base.lock line. It is fetched and verified like the
// interpreter archives and its digest feeds the generation id.
type ModelAsset struct {
	Asset
	// Project and Version name the distribution the wheel installs.
	Project string `json:"project"`
	Version string `json:"version"`
	// BaseURL is the upstream directory serving Name.
	BaseURL string `json:"base_url"`
}

// PinTable is the embedded pin table (D7.2.1, D7.2.4, D7.2.5).
type PinTable struct {
	Python    Release `json:"python"`
	Installer Release `json:"installer"`
	// ModelAssets are installed into every base generation, in order.
	ModelAssets []ModelAsset `json:"model_assets,omitempty"`
}

// ModelAssetsDigest is the SHA-256 over the model-asset table's identity
// (project, version, file name, file digest — not the URL, which a mirror may
// change without changing the bytes). "" when the table is empty, so a release
// without assets names its generations exactly as before.
func (p PinTable) ModelAssetsDigest() (string, error) {
	if len(p.ModelAssets) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, a := range p.ModelAssets {
		if a.Project == "" || a.Version == "" || a.Name == "" || len(a.SHA256) != 64 || !strings.HasSuffix(a.Name, ".whl") {
			return "", fmt.Errorf("pyenv: model asset %q %q: project, version, a .whl name and a 64-hex sha256 are required", a.Project, a.Name)
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\n", a.Project, a.Version, a.Name, a.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Pins returns the pin table embedded in this kernel release.
func Pins() (PinTable, error) {
	var p PinTable
	if err := json.Unmarshal(pinsJSON, &p); err != nil {
		return PinTable{}, fmt.Errorf("pyenv: embedded pin table: %w", err)
	}
	return p, nil
}

// PythonAsset returns the interpreter archive pinned for platform.
func (p PinTable) PythonAsset(platform string) (Asset, error) {
	return p.Python.asset("python", platform)
}

// InstallerAsset returns the installer archive pinned for platform.
func (p PinTable) InstallerAsset(platform string) (Asset, error) {
	return p.Installer.asset("installer", platform)
}

func (r Release) asset(kind, platform string) (Asset, error) {
	a, ok := r.Assets[platform]
	if !ok || a.Name == "" || len(a.SHA256) != 64 {
		return Asset{}, fmt.Errorf("pyenv: no pinned %s build for platform %q", kind, platform)
	}
	return a, nil
}

// Platform maps GOOS/GOARCH to a pin-table platform key. Linux builds are the
// glibc ones (D7.1.4: a musl host is a different download target, not pinned).
func Platform(goos, goarch string) (string, error) {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[goarch]
	switch {
	case arch == "":
	case goos == "windows" && arch == "x86_64":
		return "windows-x86_64", nil
	case goos == "linux":
		return "linux-" + arch + "-gnu", nil
	case goos == "darwin":
		return "macos-" + arch, nil
	}
	return "", fmt.Errorf("pyenv: no pinned Python build for %s/%s", goos, goarch)
}

// HostPlatform is Platform for the running kernel.
func HostPlatform() (string, error) { return Platform(runtime.GOOS, runtime.GOARCH) }

// interpreterRel is the real interpreter's path inside an extracted
// install_only build: python.exe at the root on Windows, bin/python3 elsewhere.
func interpreterRel(platform string) string {
	if isWindows(platform) {
		return "python.exe"
	}
	return "bin/python3"
}

// installerBinary is the installer's file name inside its archive.
func installerBinary(platform string) string {
	if isWindows(platform) {
		return "uv.exe"
	}
	return "uv"
}

func isWindows(platform string) bool { return strings.HasPrefix(platform, "windows") }
