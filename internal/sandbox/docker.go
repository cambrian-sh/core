package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/internal/procenv"
)

// The Docker runner (any OS with a reachable Docker engine serving Linux
// containers; on the owner's Windows box, Docker Desktop). One container per
// server launch, stdio over `docker run -i`:
//
//	--rm --init --network none|bridge --memory/--memory-swap --pids-limit
//	--ulimit cpu=N --read-only --tmpfs /tmp --cap-drop ALL
//	--security-opt no-new-privileges --user 65534:65534
//	-v <workdir>:/work -w /work
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
	if spec.Server.Root != "" {
		capab.notes = append(capab.notes, "fs: declared root "+spec.Server.Root+" is not mounted by the docker runner")
	}
	if spec.Server.Network != NetworkEgress {
		capab.notes = append(capab.notes, "network none: a package fetched at launch (npx/uvx) must already be in the image")
	}
	return capab
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
	if tp.MemoryMB > 0 {
		m := strconv.Itoa(tp.MemoryMB) + "m"
		args = append(args, "--memory", m, "--memory-swap", m)
	}
	if tp.MaxProcesses > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(tp.MaxProcesses))
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
	post := func() (osHandle, error) { return &containerHandle{bin: bin, name: name, cmd: cmd}, nil }
	return start(cmd, post)
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
	})
	return h.err
}

func (*containerHandle) release() {}

func (*containerHandle) limitReason() string { return "" }
