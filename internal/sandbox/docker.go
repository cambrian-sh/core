package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/internal/procenv"
	"github.com/cambrian-sh/core/internal/sandbox/egressproxy"
)

// The Docker runner (any OS with a reachable Docker engine serving Linux
// containers; on the owner's Windows box, Docker Desktop). One container per
// server launch, stdio over `docker run -i`:
//
//	--rm --init --network none|bridge --memory/--memory-swap --pids-limit
//	--ulimit cpu=N --read-only --tmpfs /tmp --cap-drop ALL
//	--security-opt no-new-privileges --user 65534:65534
//	-v <workdir>:/work -w /work
//	--tmpfs /work/<dir>:rw,exec,… for each workdir env directory (ENG-2217)
//
// The host filesystem is not visible at all (only the workdir, at /work), the
// network is off unless the server declares egress, and the identity is
// nobody with no capabilities. Stopping it is `docker rm -f`. The image comes
// from the server's own sandbox.image, else mcp.sandbox.docker_images keyed by
// the launch command's basename (npx → a node image).
//
// Caveat recorded in every report: an npx/uvx server fetches its package at
// launch, so with network none it can only run from an image that already
// contains it.
//
// Declared egress hosts (ENG-688): when the tier denies the network and the
// server declares a host list, a forwarder sidecar (the python3 image running
// egressproxy.PythonForwarder, itself --network none) starts first; the
// server runs with --network container:<sidecar>, so its namespace has
// loopback only and its one way out is the sidecar's proxy port, relayed over
// the sidecar's stdio to the kernel's proxy session. Otherwise declared
// egress gets --network bridge, as before.

const containerWork = "/work"

var dockerMemo struct {
	sync.Mutex
	at  map[string]time.Time
	why map[string]string
}

// dockerReachable probes the engine (cached for a minute): the CLI must exist
// and the engine must answer and serve Linux containers.
func dockerReachable(ctx context.Context, dockerPath string) (string, string) {
	bin := dockerPath
	if bin == "" {
		bin = "docker"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", "docker CLI not found"
	}
	dockerMemo.Lock()
	if t, ok := dockerMemo.at[resolved]; ok && time.Since(t) < time.Minute {
		why := dockerMemo.why[resolved]
		dockerMemo.Unlock()
		if why != "" {
			return "", why
		}
		return resolved, ""
	}
	dockerMemo.Unlock()

	pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(pctx, resolved, "version", "--format", "{{.Server.Os}}").Output() //nolint:gosec // configured docker CLI
	why := ""
	switch {
	case err != nil:
		why = "docker engine not reachable: " + err.Error()
	case strings.TrimSpace(string(out)) != "linux":
		why = "docker engine serves " + strings.TrimSpace(string(out)) + " containers, not linux"
	}
	dockerMemo.Lock()
	if dockerMemo.at == nil {
		dockerMemo.at, dockerMemo.why = map[string]time.Time{}, map[string]string{}
	}
	dockerMemo.at[resolved], dockerMemo.why[resolved] = time.Now(), why
	dockerMemo.Unlock()
	if why != "" {
		return "", why
	}
	return resolved, ""
}

// imageFor picks the container image for a server.
func imageFor(p *Policy, spec Spec, c Command) string {
	if spec.Server.Image != "" {
		return spec.Server.Image
	}
	base := strings.ToLower(filepath.Base(c.Path))
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".exe"), ".cmd")
	return p.DockerImages[base]
}

func dockerCapability(ctx context.Context, p *Policy, spec Spec, tp TierPolicy, c Command) capability {
	capab := capability{runner: RunnerDocker}
	if spec.Agent != nil {
		return dockerAgentCapability(ctx, p, spec, tp, c)
	}
	if _, why := dockerReachable(ctx, p.DockerPath); why != "" {
		capab.unusable = why
		return capab
	}
	if imageFor(p, spec, c) == "" {
		capab.unusable = "no image for command " + filepath.Base(c.Path) + " (set the server's sandbox.image or mcp.sandbox.docker_images)"
		return capab
	}
	capab.enforced = map[Control]string{
		ControlEnv: "", ControlWorkdir: "", ControlOutputCap: "", ControlStallKill: "",
		ControlKillTree: "docker", ControlMemory: "docker", ControlProcesses: "docker", ControlCPUTime: "docker",
		ControlFSConfine: "docker", ControlNetwork: "docker", ControlLowPrivilege: "docker",
	}
	capab.notes = append(capab.notes, "fs: host filesystem invisible; only the workdir is mounted, at /work")
	capab.notes = append(capab.notes, "fs: home, temp, package caches and tool dirs ("+strings.Join(dockerScratchDirs(p), ", ")+" under /work) are per-launch exec tmpfs mounts owned by the server's uid, not kept across launches")
	if spec.Server.Root != "" {
		capab.notes = append(capab.notes, "fs: declared root "+spec.Server.Root+" is not mounted by the docker runner")
	}
	if spec.Server.Network != NetworkEgress {
		capab.notes = append(capab.notes, "network none: a package fetched at launch (npx/uvx) must already be in the image")
	}
	if ok, why := egressPlan(p, spec, tp); ok {
		if forwarderImage(p) == "" {
			capab.egressWhy = "no egress forwarder image (mcp.sandbox.docker_images.python3)"
		} else {
			capab.egress = RunnerDocker
			capab.notes = append(capab.notes, "network: a forwarder sidecar ("+forwarderImage(p)+", --network none) owns the server's namespace; the server joins it and reaches the kernel's proxy through the sidecar's stdio")
		}
	} else {
		capab.egressWhy = why
	}
	return capab
}

// forwarderImage is the egress forwarder sidecar's image: the configured
// python3 image (mcp.sandbox.docker_images.python3), which runs forwarder.py.
func forwarderImage(p *Policy) string { return p.DockerImages["python3"] }

// egressSidecar is a docker launch's egress forwarder (ENG-688): a container
// with no network that owns the server's network namespace (the server joins
// it with --network container:<name>) and relays the server's connections to
// the kernel's proxy session over its stdio.
type egressSidecar struct {
	bin, name string
	cmd       *exec.Cmd
	session   *egressproxy.Session
	once      sync.Once
}

// sidecarReadyTimeout bounds the sidecar's start, an image pull included.
const sidecarReadyTimeout = 3 * time.Minute

// startEgressSidecar starts the forwarder and waits until it listens, so the
// server never starts into a namespace with nothing on the proxy port.
func startEgressSidecar(ctx context.Context, bin string, p *Policy, spec Spec, base string) (*egressSidecar, error) {
	sess, err := p.Egress.Open(spec.ServerID, spec.Server.Hosts)
	if err != nil {
		return nil, fmt.Errorf("sandbox: egress proxy session: %w", err)
	}
	name := base + "-egress"
	args := []string{"run", "--rm", "-i", "--init", "--name", name,
		"--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--user", "65534:65534", "--memory", "64m", "--memory-swap", "64m", "--pids-limit", "32",
		"--label", "cambrian.sandbox=mcp-egress", "--label", "cambrian.server=" + spec.ServerID,
		forwarderImage(p), "python3", "-I", "-c", egressproxy.PythonForwarder, strconv.Itoa(egressproxy.ListenPort)}
	cmd := exec.Command(bin, args...) //nolint:gosec // configured docker CLI, kernel-built argv
	cmd.Env = procenv.Allowlisted([]string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"})
	tail := &tailBuffer{max: 2048}
	cmd.Stderr = tail
	in, err := cmd.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		sess.Close()
		return nil, fmt.Errorf("sandbox: egress forwarder: %w", err)
	}
	sc := &egressSidecar{bin: bin, name: name, cmd: cmd, session: sess}
	if err := sess.Serve(pipeRWC{out, in}); err != nil {
		sc.stop()
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, sidecarReadyTimeout)
	defer cancel()
	select {
	case <-sess.Ready():
		return sc, nil
	case <-wctx.Done():
	}
	sc.stop()
	return nil, fmt.Errorf("sandbox: egress forwarder %s did not start: %s", name, strings.TrimSpace(tail.String()))
}

// stop removes the sidecar, reaps its CLI and closes the proxy session.
// Idempotent.
func (s *egressSidecar) stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, s.bin, "rm", "-f", s.name).Run() //nolint:gosec // configured docker CLI
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.cmd.Wait()
		s.session.Close()
	})
}

// pipeRWC joins a command's stdout (read) and stdin (write) into the
// multiplex's byte stream.
type pipeRWC struct {
	io.ReadCloser
	io.WriteCloser
}

func (p pipeRWC) Close() error {
	_ = p.WriteCloser.Close()
	return p.ReadCloser.Close()
}

func startDocker(ctx context.Context, p *Policy, spec Spec, tp TierPolicy, c Command, workdir string, start starter) error {
	if spec.Agent != nil {
		return startDockerAgent(ctx, p, spec, tp, c, workdir, start)
	}
	bin, why := dockerReachable(ctx, p.DockerPath)
	if why != "" {
		return fmt.Errorf("sandbox: %s", why)
	}
	image := imageFor(p, spec, c)
	// Directories are created on the host; the process sees /work paths.
	inner, err := workdirEnv(p, nil, true, workdir, containerWork, "/")
	if err != nil {
		return err
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	name := "cambrian-mcp-" + strings.ToLower(safeName(spec.ServerID)) + "-" + hex.EncodeToString(rnd[:])

	network := "none"
	if spec.Server.Network == NetworkEgress || !tp.DenyNetwork {
		network = "bridge"
	}
	// ENG-688: an enforced egress allowlist. The forwarder sidecar starts
	// first and owns a network namespace with loopback only; the server joins
	// it, so its one way out is the proxy port the sidecar listens on.
	var sidecar *egressSidecar
	if ok, _ := egressPlan(p, spec, tp); ok && forwarderImage(p) != "" {
		sidecar, err = startEgressSidecar(ctx, bin, p, spec, name)
		if err != nil {
			return err
		}
		network = "container:" + sidecar.name
		inner = append(inner, egressproxy.ProxyEnv()...)
	}
	args := []string{"run", "--rm", "-i", "--init", "--name", name,
		"--network", network,
		"--read-only", "--tmpfs", "/tmp:rw,size=256m",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"-v", workdir + ":" + containerWork, "-w", containerWork,
		"--label", "cambrian.sandbox=mcp", "--label", "cambrian.server=" + spec.ServerID,
	}
	if tp.LowPrivilege {
		args = append(args, "--user", "65534:65534")
	}
	args = append(args, dockerScratchMounts(p, tp)...)
	if tp.MemoryMB > 0 {
		m := strconv.Itoa(tp.MemoryMB) + "m"
		args = append(args, "--memory", m, "--memory-swap", m)
	}
	if n := tp.taskCeiling(); n > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(n))
	}
	if tp.CPUTimeSec > 0 {
		s := strconv.Itoa(tp.CPUTimeSec)
		args = append(args, "--ulimit", "cpu="+s+":"+s)
	}
	// Values travel in the docker CLI's environment and are forwarded by NAME
	// (-e NAME), so no secret ever appears on a command line.
	cliEnv := procenv.Allowlisted([]string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"})
	own := map[string]bool{}
	for _, n := range c.Forward {
		own[procenv.Key(n)] = true
	}
	for _, kv := range c.Env {
		if i := strings.IndexByte(kv, '='); i > 0 && own[procenv.Key(kv[:i])] {
			args = append(args, "-e", kv[:i])
			cliEnv = append(cliEnv, kv)
		}
	}
	for _, kv := range inner {
		args = append(args, "-e", kv) // workdir paths: not secret
	}
	args = append(args, image, path.Base(filepath.ToSlash(stripExt(c.Path))))
	args = append(args, c.Args...)

	cmd := exec.Command(bin, args...) //nolint:gosec // configured docker CLI and operator-configured command
	cmd.Env = cliEnv
	post := func() (osHandle, error) {
		return &containerHandle{bin: bin, name: name, cmd: cmd, egress: sidecar}, nil
	}
	if err := start(cmd, post); err != nil {
		sidecar.stop()
		return err
	}
	return nil
}

// dockerScratchDirs are the top-level workdir directories the workdir env
// points into (tmp, home, cache, uv-tools with the default policy), sorted.
func dockerScratchDirs(p *Policy) []string {
	seen := map[string]bool{}
	for _, m := range []map[string]string{p.WorkdirEnv, p.ConfinedEnv} {
		for _, rel := range m {
			top := strings.SplitN(strings.Trim(filepath.ToSlash(rel), "/"), "/", 2)[0]
			if top != "" && top != "." && top != ".." {
				seen[top] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// dockerScratchMounts mounts an exec tmpfs over each workdir env directory
// (ENG-2217). The bind-mounted workdir is not writable by the container's
// uid in general — on Docker Desktop for Windows every write into it as uid
// 65534 fails with EIO, and on a Linux host the kernel's 0700 workdir belongs
// to the kernel user — so uvx and npx died before initialize: they could not
// write their package caches. The plain /tmp tmpfs is no fallback: Docker
// mounts it noexec and the tool venv uvx builds must execute. Each mount is
// owned by the server's uid, private (0700), nosuid/nodev, and bounded by the
// tier's memory limit (tmpfs pages count against it), else 1 GiB. Rejected: a
// named volume per server, which would keep caches across launches but is
// created root-owned and needs a chown container first, and outlives the
// server as state the runner would have to garbage-collect.
func dockerScratchMounts(p *Policy, tp TierPolicy) []string {
	size := tp.MemoryMB
	if size <= 0 {
		size = 1024
	}
	opts := "rw,exec,nosuid,nodev,mode=0700,size=" + strconv.Itoa(size) + "m"
	if tp.LowPrivilege {
		opts += ",uid=65534,gid=65534"
	}
	var args []string
	for _, d := range dockerScratchDirs(p) {
		args = append(args, "--tmpfs", containerWork+"/"+d+":"+opts)
	}
	return args
}

// stripExt drops a Windows launcher extension: "npx.cmd" runs as "npx" in the image.
func stripExt(p string) string {
	l := strings.ToLower(p)
	for _, ext := range []string{".cmd", ".exe", ".bat"} {
		if strings.HasSuffix(l, ext) {
			return p[:len(p)-len(ext)]
		}
	}
	return p
}

// containerHandle removes the container (which ends the docker CLI too).
type containerHandle struct {
	bin, name string
	cmd       *exec.Cmd
	once      sync.Once
	err       error
	// reclaim is set for an agent container: its scratch is handed back to
	// the kernel after the exit (reclaimScratch, ENG-2031). nil for MCP.
	reclaim *dockerReclaim
	// egress is the MCP server's egress forwarder sidecar (ENG-688); nil when
	// its egress allowlist is not enforced.
	egress *egressSidecar
}

func (h *containerHandle) kill() error {
	h.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, h.bin, "rm", "-f", h.name).CombinedOutput() //nolint:gosec // configured docker CLI
		if err != nil && !strings.Contains(string(out), "No such container") {
			h.err = fmt.Errorf("docker rm -f %s: %v: %s", h.name, err, strings.TrimSpace(string(out)))
		}
		if h.cmd.Process != nil {
			_ = h.cmd.Process.Kill()
		}
		h.egress.stop()
	})
	return h.err
}

func (*containerHandle) release() {}

func (*containerHandle) limitReason() string { return "" }
