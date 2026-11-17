package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/cambrian-sh/core/internal/procenv"
)

// The docker runner for kernel-spawned agents (ADR-0157 D2.1, stage S1). One
// container per agent INSTANCE:
//
//	--rm --init --read-only --tmpfs /tmp --cap-drop ALL
//	--security-opt no-new-privileges --user <non-root uid, not the kernel's>
//	--network host | none                (normal network, or kernel-only: S2)
//	-v <scratch>:/work                   (its TEMP/TMPDIR/HOME)
//	-v <socket dir>:<socket dir>         (D2.6: the --socket path is unchanged)
//	-v <code, runtime, declared paths>:<same path>[:ro]
//
// A kernel-only tier (DenyNetwork, ADR-0157 D3) runs with --network none: the
// container has a loopback interface only, and reaches the kernel through the
// kernel socket the agent manager puts in the instance's socket directory,
// which is mounted at its own path. Everything else runs with the host network.
//
// Nothing else of the host is visible, so the D1 hide-list is enforced by
// absence; a hidden path that lies inside a mounted one is masked again
// (tmpfs for a directory, /dev/null for a file).
//
// Host paths and the unix socket are shared with the container at the same
// paths, so this runner needs a LINUX host: on Docker Desktop (Windows, macOS)
// the engine runs in a VM where neither the paths nor the socket exist. The
// image must provide the agent's runtime at the path the kernel launches
// (execution.agents.sandbox.docker_image: e.g. the kernel's own image).

const agentDefaultDockerUser = "65534:65534"

func dockerAgentCapability(ctx context.Context, p *Policy, spec Spec, tp TierPolicy, c Command) capability {
	capab := capability{runner: RunnerDocker}
	if runtime.GOOS != "linux" {
		capab.unusable = "the docker runner for agents needs a Linux host (host paths and the agent's unix socket are shared with the container); " + runtime.GOOS + " runs the engine in a VM"
		return capab
	}
	if _, why := dockerReachable(ctx, p.DockerPath); why != "" {
		capab.unusable = why
		return capab
	}
	if spec.Server.Image == "" {
		capab.unusable = "no image (set execution.agents.sandbox.docker_image)"
		return capab
	}
	capab.enforced = map[Control]string{
		ControlEnv: "", ControlWorkdir: "", ControlOutputCap: "", ControlStallKill: "",
		ControlKillTree: "docker", ControlMemory: "docker", ControlProcesses: "docker", ControlCPUTime: "docker",
		ControlFSConfine: "docker", ControlLowPrivilege: "docker", ControlHideState: "docker",
		ControlNetwork: "docker",
	}
	capab.notes = append(capab.notes,
		"fs: host filesystem invisible except the agent's code, runtime, declared paths, scratch (/work) and socket directory",
		"docker: the image "+spec.Server.Image+" must provide the agent runtime at "+c.Path)
	if tp.DenyNetwork {
		capab.notes = append(capab.notes, "network: kernel-only (--network none; the kernel is reached through the unix socket in the socket directory)")
	} else {
		capab.notes = append(capab.notes, "network: host network (normal network)")
	}
	return capab
}

func startDockerAgent(ctx context.Context, p *Policy, spec Spec, tp TierPolicy, c Command, workdir string, start starter) error {
	bin, why := dockerReachable(ctx, p.DockerPath)
	if why != "" {
		return fmt.Errorf("sandbox: %s", why)
	}
	args, cliEnv, err := dockerAgentArgs(p, spec, tp, c, workdir)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, args...) //nolint:gosec // configured docker CLI and kernel-built agent command
	cmd.Env = cliEnv
	name := args[indexOf(args, "--name")+1]
	post := func() (osHandle, error) { return &containerHandle{bin: bin, name: name, cmd: cmd}, nil }
	return start(cmd, post)
}

// dockerAgentArgs builds the `docker run` argv and the docker CLI's own
// environment (secrets travel there and are forwarded by NAME, never on argv).
func dockerAgentArgs(p *Policy, spec Spec, tp TierPolicy, c Command, workdir string) ([]string, []string, error) {
	inner, err := workdirEnv(p, nil, tp.ConfineFS, workdir, containerWork, "/")
	if err != nil {
		return nil, nil, err
	}
	// The container's uid is not the kernel's: it must be able to write its
	// scratch and socket directories, and the kernel must be able to connect
	// to the socket it creates (umask 000 below). Both directories are private
	// to this instance under traverse-only (0711) parents.
	chmodTree(workdir, 0o777)
	if spec.Agent.SocketDir != "" {
		_ = os.Chmod(spec.Agent.SocketDir, 0o777)
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	name := "cambrian-agent-" + strings.ToLower(safeName(spec.ServerID)) + "-" + hex.EncodeToString(rnd[:])
	user := spec.Agent.DockerUser
	if user == "" {
		user = agentDefaultDockerUser
	}
	network := "host"
	if tp.DenyNetwork {
		network = "none" // ADR-0157 D3 kernel-only: the kernel socket is in the socket dir
	}
	args := []string{"run", "--rm", "--init", "--name", name,
		"--network", network,
		"--read-only", "--tmpfs", "/tmp:rw,size=256m",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--user", user,
		"-v", workdir + ":" + containerWork,
		"--label", "cambrian.sandbox=agent", "--label", "cambrian.agent=" + spec.ServerID,
	}
	if spec.Agent.SocketDir != "" {
		args = append(args, "-v", spec.Agent.SocketDir+":"+spec.Agent.SocketDir)
	}
	var mounted []string
	for _, b := range spec.Agent.Binds {
		if b.Path == "" {
			continue
		}
		if _, err := os.Stat(b.Path); err != nil {
			continue
		}
		v := b.Path + ":" + b.Path
		if !b.Write {
			v += ":ro"
		}
		args = append(args, "-v", v)
		mounted = append(mounted, b.Path)
	}
	// A hidden path inside a mounted one is masked again.
	for _, h := range p.HidePaths {
		for _, m := range mounted {
			if h == "" || !within(h, m) || filepath.Clean(h) == filepath.Clean(m) {
				continue
			}
			if fi, err := os.Stat(h); err == nil {
				if fi.IsDir() {
					args = append(args, "--tmpfs", h)
				} else {
					args = append(args, "-v", "/dev/null:"+h+":ro")
				}
			}
			break
		}
	}
	dir := c.Dir
	if dir == "" {
		dir = containerWork
	}
	args = append(args, "-w", dir)
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
		args = append(args, "-e", kv) // scratch paths: not secret
	}
	// umask 000: the socket the agent creates must be connectable by the
	// kernel's uid, which is not the container's.
	args = append(args, spec.Server.Image, "/bin/sh", "-c", `umask 000; exec "$@"`, "sh", c.Path)
	args = append(args, c.Args...)
	return args, cliEnv, nil
}

func indexOf(a []string, s string) int {
	for i, v := range a {
		if v == s {
			return i
		}
	}
	return -1
}

// chmodTree sets mode on dir and every directory below it.
func chmodTree(dir string, mode os.FileMode) {
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, mode)
		}
		return nil
	})
}
