//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/cambrian-sh/core/internal/sandbox/egressproxy"
)

// The Windows native runner's egress forwarder (ENG-2215). A server whose
// declared hosts are enforced runs in an AppContainer with no network
// capability: it can reach nothing but loopback inside its own container
// (Windows loopback isolation is per package SID, so the container's own
// processes reach each other and nothing else). The kernel's executable,
// granted read to the container and re-executed inside it as the forwarder,
// listens on 127.0.0.1:<egressproxy.ListenPort>, dials the kernel's proxy
// session over an AF_UNIX socket the kernel listens on in the server's
// workdir (granted to the container), and then starts the server as its
// child — in the same container and Job Object — so the server never starts
// before the proxy port is open. It writes nothing to stdout, which is the
// server's MCP channel, and exits with the server's status. The Unix twin is
// egress_forwarder_unix.go (an inherited socketpair instead of a socket path).

const (
	egressForwarderArg = "__cambrian_egress_forwarder"
	// egressSocketName is the multiplex socket in the server's workdir. AF_UNIX
	// paths are limited to 108 bytes, so the name is short.
	egressSocketName = "egress.sock"
	maxUnixPath      = 107
)

func init() {
	// The re-executed forwarder never reaches main (or TestMain).
	if len(os.Args) > 1 && os.Args[1] == egressForwarderArg {
		os.Exit(runEgressForwarder(os.Args[2:]))
	}
}

// runEgressForwarder: argv is <port> <socket path> -- <server path> <server args…>.
func runEgressForwarder(argv []string) int {
	if len(argv) < 4 || argv[2] != "--" {
		fmt.Fprintln(os.Stderr, "cambrian egress forwarder: usage: <port> <socket> -- <command> [args…]")
		return 2
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+argv[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "cambrian egress forwarder: listen:", err)
		return 2
	}
	conn, err := net.Dial("unix", argv[1])
	if err != nil {
		_ = ln.Close()
		fmt.Fprintln(os.Stderr, "cambrian egress forwarder: kernel socket:", err)
		return 2
	}
	go func() { _ = egressproxy.Forward(ln, conn) }()
	cmd := exec.Command(argv[3], argv[4:]...) //nolint:gosec // the kernel-built server command
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	default:
		fmt.Fprintln(os.Stderr, "cambrian egress forwarder: start server:", err)
		return 127
	}
}

// egressForwarderExe is the kernel executable the container re-executes.
func egressForwarderExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe, nil
}

// egressSocketPath is the multiplex socket for a workdir, or an error when the
// path is too long for AF_UNIX.
func egressSocketPath(workdir string) (string, error) {
	p := filepath.Join(workdir, egressSocketName)
	if len(p) > maxUnixPath {
		return "", fmt.Errorf("egress socket path %s is longer than %d bytes (shorten mcp.sandbox.work_root)", p, maxUnixPath)
	}
	return p, nil
}

// forwarderArgv wraps a server command in the forwarder.
func forwarderArgv(exe, socket, path string, args []string) []string {
	return append([]string{exe, egressForwarderArg, strconv.Itoa(egressproxy.ListenPort), socket, "--", path}, args...)
}
