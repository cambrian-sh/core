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
	"time"

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
// (a read-only tmpfs for a directory, /dev/null for a file): empty AND
// read-only, as D1 and the bubblewrap runner's --remount-ro have it.
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
	user := spec.Agent.DockerUser
	if user == "" {
		user = agentDefaultDockerUser
	}
	post := func() (osHandle, error) {
		return &containerHandle{bin: bin, name: name, cmd: cmd,
			reclaim: &dockerReclaim{image: spec.Server.Image, user: user}}, nil
	}
	return start(cmd, post)
}

// reclaimTimeout bounds the one-shot container that hands an exited agent's
// scratch back to the kernel (ENG-2031).
const reclaimTimeout = 2 * time.Minute

// dockerReclaim is what the reclaim container needs: the agent's image and
// docker user (the owner of every file the agent created).
type dockerReclaim struct{ image, user string }

// reclaimScratch hands an exited agent container's scratch back to the
// kernel's uid (ADR-0157 D2.2 keeps the agent on a uid that is not the
// kernel's; owner decision on ENG-2031, option 1). A directory or file the
// agent made with an explicit mode (0700, 0600: caches, tempfile, pip) is
// closed to a non-root kernel, which then could neither collect out/ (D5.1)
// nor remove the scratch. One `docker run --rm` of the same image, as the
// agent's own user (it owns those files, so no capability is needed), with no
// network, a read-only root and only the scratch mounted, opens the tree; the
// kernel then checks it can walk every entry. A root kernel is not bound by
// file modes and skips it.
func (h *containerHandle) reclaimScratch(ctx context.Context, workdir string) error {
	if h.reclaim == nil || workdir == "" || os.Geteuid() == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, reclaimTimeout)
	defer cancel()
	args := reclaimArgs(h.name, h.reclaim.image, h.reclaim.user, workdir)
	cmd := exec.CommandContext(ctx, h.bin, args...) //nolint:gosec // configured docker CLI, kernel-built argv
	cmd.Env = procenv.Allowlisted([]string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"})
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reclaim container (%s): %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	if p, err := unreachableIn(workdir); err != nil {
		return fmt.Errorf("after the reclaim container the kernel still cannot reach %s: %w", p, err)
	}
	return nil
}

// reclaimArgs is the reclaim container's `docker run` argv. chmod -R goes on
// past entries it may not change (the kernel's own directories, already open)
// and the kernel's walk afterwards is the check, so its status is not.
func reclaimArgs(agentName, image, user, workdir string) []string {
	return []string{"run", "--rm", "--name", agentName + "-reclaim",
		"--network", "none", "--read-only",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", "32", "--memory", "128m", "--memory-swap", "128m",
		"--user", user,
		"-v", workdir + ":" + containerWork,
		"--label", "cambrian.sandbox=agent-reclaim",
		"--entrypoint", "/bin/sh", image,
		"-c", "chmod -R a+rwX " + containerWork + " 2>/dev/null; exit 0"}
}

// unreachableIn walks dir as the kernel and returns the first entry it cannot
// read (a directory it cannot list), with the error; "" and nil when every
// entry is reachable.
func unreachableIn(dir string) (string, error) {
	var bad string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			bad = path
			return err
		}
		return nil
	})
	return bad, err
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
					// :ro — a write there must fail, not land in a throwaway
					// tmpfs (ENG-2017 found the agent could write it).
					args = append(args, "--tmpfs", h+":ro")
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
