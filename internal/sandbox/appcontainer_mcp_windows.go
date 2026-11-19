//go:build windows

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/windows"
)

// The MCP server AppContainer (ENG-2215): the egress-enforced half of the
// Windows native runner. An MCP server whose declared hosts are enforced
// (egressPlan) runs under a lowbox token with NO network capability, built
// like an agent's (appcontainer_windows.go, CreateAppContainerToken), so the
// only way out is the forwarder in the same container, which relays the
// server's 127.0.0.1 proxy port to the kernel's CONNECT proxy
// (egress_forwarder_windows.go). The container reads system directories and
// what the kernel granted it:
//
//   - the kernel's executable (the forwarder), the FILE only — never its
//     directory, which may hold kernel state;
//   - the server command's own installation directory (node's, uv's, the
//     interpreter's), granted to the shared mcpRuntimeCapabilityName unless
//     the container can already read it (ALL APPLICATION PACKAGES on a
//     standard Program Files install);
//   - its workdir, read-write, through its own package SID.
//
// A grant the kernel cannot make (an installation owned by Administrators
// without an ALL APPLICATION PACKAGES ACE) leaves the server on the
// low-integrity runner with its hosts recorded, not filtered, and the report
// names the path and the one-time administrator command — the same fallback
// an agent gets.

// mcpRuntimeCapabilityName is the capability every MCP server container holds
// and every granted MCP runtime path is granted to.
const mcpRuntimeCapabilityName = "cambrianMcpRuntime"

// newMCPAppContainer builds an MCP server's lowbox token: its package SID and
// the shared MCP runtime capability; no network capability, ever.
func newMCPAppContainer(spec Spec) (*appContainer, error) {
	ac := &appContainer{identity: spec.ServerID}
	ac.pkgName = containerName("cambrian.mcp.", spec.ServerID)
	var err error
	if ac.pkg, err = derivePackageSID(ac.pkgName); err != nil {
		return nil, fmt.Errorf("appcontainer package sid: %w", err)
	}
	if ac.runtime, err = deriveCapabilitySID(mcpRuntimeCapabilityName); err != nil {
		return nil, fmt.Errorf("appcontainer mcp runtime capability: %w", err)
	}
	caps := []windows.SIDAndAttributes{{Sid: ac.runtime, Attributes: windows.SE_GROUP_ENABLED}}
	if ac.token, err = appContainerToken(ac.pkg, caps); err != nil {
		return nil, err
	}
	return ac, nil
}

// wantsMCPEgressContainer: an MCP server whose declared egress hosts are to be
// enforced, on a tier that confines it (the low-integrity runner would have
// run it otherwise).
func wantsMCPEgressContainer(p *Policy, spec Spec, tp TierPolicy) bool {
	if spec.Agent != nil || !(tp.ConfineFS || tp.LowPrivilege) {
		return false
	}
	ok, _ := egressPlan(p, spec, tp)
	return ok
}

// mcpEgressContainer builds the container for an egress-enforced MCP server
// and makes its read grants. It returns the container and the resolved
// command path, or nil and why the server cannot run in one (the caller
// closes the container).
func mcpEgressContainer(p *Policy, spec Spec, c Command) (*appContainer, string, string) {
	exe, err := egressForwarderExe()
	if err != nil {
		return nil, "", "the kernel executable (the forwarder) cannot be located: " + err.Error()
	}
	path, err := resolveCommand(c)
	if err != nil {
		return nil, "", "the server command cannot be resolved: " + err.Error()
	}
	// The container is granted the link's target, and opening a file through
	// a junction also checks the junction's own ACL (uv's
	// cpython-3.13-windows-… is a junction to the patch release), so the
	// server is started by its real path.
	path = finalPath(path)
	ac, err := newMCPAppContainer(spec)
	if err != nil {
		return nil, "", err.Error()
	}
	if why := ac.grantReadable(exe, false); why != "" {
		ac.close()
		return nil, "", why
	}
	if why := ac.grantReadable(filepath.Dir(path), true); why != "" {
		ac.close()
		return nil, "", why
	}
	// What the launcher reads beyond its own installation (uv's managed
	// Pythons): Policy.AppContainerReads, keyed by the command's basename.
	for _, raw := range p.AppContainerReads[commandBase(c.Path)] {
		extra := os.ExpandEnv(raw)
		if extra == "" || strings.Contains(extra, "${") {
			continue
		}
		if _, err := os.Stat(extra); err != nil {
			continue // absent here (no managed Pythons yet): nothing to read
		}
		if why := ac.grantReadable(extra, true); why != "" {
			ac.close()
			return nil, "", why
		}
	}
	return ac, path, ""
}

// commandBase is a launch command's basename, lower case, without a Windows
// launcher extension ("C:\…\uvx.exe" → "uvx").
func commandBase(path string) string {
	return strings.ToLower(stripExt(filepath.Base(path)))
}

// grantReadable makes path readable by the container: nothing to do when it
// already is, else an ACE for the MCP runtime capability (the whole tree when
// tree, the file alone otherwise). "" on success, else why not, naming the
// administrator's one-time command.
func (a *appContainer) grantReadable(path string, tree bool) string {
	target := grantTarget(path)
	if ok, err := canRead(a.token, target); err == nil && ok {
		return ""
	}
	var err error
	if tree {
		err = ensureGrant(target, a.runtime, accessRead)
	} else {
		err = grantOne(target, a.runtime, accessRead, false)
	}
	if err != nil {
		inherit := "(OI)(CI)"
		if !tree {
			inherit = ""
		}
		return fmt.Sprintf("the MCP server container cannot be granted read on %s (%v); an administrator can grant it once: icacls \"%s\" /grant *%s:%sRX%s",
			target, err, target, a.runtime.String(), inherit, map[bool]string{true: " /T", false: ""}[tree])
	}
	return ""
}

// resolveCommand returns the absolute path of the server command, searching
// the PATH of the command's own environment first (the server's, not the
// kernel's), then the kernel's.
func resolveCommand(c Command) (string, error) {
	if filepath.IsAbs(c.Path) {
		if _, err := os.Stat(c.Path); err == nil {
			return c.Path, nil
		}
	}
	var pathVar, extVar string
	for _, kv := range c.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(k) {
		case "PATH":
			pathVar = v
		case "PATHEXT":
			extVar = v
		}
	}
	if extVar == "" {
		extVar = ".COM;.EXE;.BAT;.CMD"
	}
	// A name whose extension is not executable (npx, sandbox.test) is tried
	// with each PATHEXT extension first, as cmd.exe does.
	exts := []string{""}
	known := strings.Split(strings.ToLower(extVar), ";")
	if !slices.Contains(known, strings.ToLower(filepath.Ext(c.Path))) {
		exts = append(known, "")
	}
	if pathVar != "" && !strings.ContainsAny(c.Path, `\/`) {
		for _, dir := range filepath.SplitList(pathVar) {
			for _, ext := range exts {
				cand := filepath.Join(dir, c.Path+ext)
				if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
					return cand, nil
				}
			}
		}
	}
	return exec.LookPath(c.Path)
}

// finalPath resolves every symlink AND junction in path
// (GetFinalPathNameByHandle; filepath.EvalSymlinks leaves junctions alone),
// falling back to path.
func finalPath(path string) string {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return path
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return path
	}
	defer func() { _ = windows.CloseHandle(h) }()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	if err != nil || n == 0 || int(n) > len(buf) {
		return path
	}
	out := windows.UTF16ToString(buf[:n])
	const verbatim, verbatimUNC = `\\?\`, `\\?\UNC\`
	if rest, ok := strings.CutPrefix(out, verbatimUNC); ok {
		return `\\` + rest
	}
	return strings.TrimPrefix(out, verbatim)
}
