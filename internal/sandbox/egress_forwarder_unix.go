//go:build !windows

package sandbox

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/cambrian-sh/core/internal/sandbox/egressproxy"
)

// The bubblewrap runner's egress forwarder (ENG-688). A server whose declared
// hosts are enforced runs in its own network namespace (--unshare-net), where
// nothing but loopback exists. The kernel's own executable, bound read-only
// into the sandbox at egressForwarderPath, is re-executed there as the
// forwarder: it listens on 127.0.0.1:<egressproxy.ListenPort>, then starts
// the server as its child (so the server never starts before the proxy port
// is open) and relays each connection over the multiplex on an inherited
// socketpair (fd egressMuxFD) to the kernel's proxy session. It writes
// nothing to stdout, which is the server's MCP channel, and exits with the
// server's status.

const (
	egressForwarderArg  = "__cambrian_egress_forwarder"
	egressForwarderPath = "/tmp/.cambrian-egress-forwarder" // in the sandbox's private /tmp
	egressMuxFD         = 3                                 // cmd.ExtraFiles[0]
)

func init() {
	// The re-executed forwarder never reaches main (or TestMain).
	if len(os.Args) > 1 && os.Args[1] == egressForwarderArg {
		os.Exit(runEgressForwarder(os.Args[2:]))
	}
}

// runEgressForwarder: argv is <port> -- <server path> <server args…>.
func runEgressForwarder(argv []string) int {
	if len(argv) < 3 || argv[1] != "--" {
		fmt.Fprintln(os.Stderr, "cambrian egress forwarder: usage: <port> -- <command> [args…]")
		return 2
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+argv[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "cambrian egress forwarder: listen:", err)
		return 2
	}
	f := os.NewFile(egressMuxFD, "egress-mux")
	conn, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cambrian egress forwarder: multiplex fd:", err)
		return 2
	}
	go func() { _ = egressproxy.Forward(ln, conn) }()
	cmd := exec.Command(argv[2], argv[3:]...) //nolint:gosec // the kernel-built server command
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	default:
		fmt.Fprintln(os.Stderr, "cambrian egress forwarder: start server:", err)
		return 127
	}
}

// egressForwarderExe is the kernel executable the sandbox re-executes.
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

// egressPair opens the multiplex socketpair: the kernel's end as a net.Conn,
// the child's end as the file passed as fd egressMuxFD.
func egressPair() (net.Conn, *os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	kf := os.NewFile(uintptr(fds[0]), "egress-mux-kernel")
	kc, err := net.FileConn(kf)
	_ = kf.Close()
	if err != nil {
		_ = syscall.Close(fds[1])
		return nil, nil, err
	}
	return kc, os.NewFile(uintptr(fds[1]), "egress-mux-child"), nil
}

// forwarderArgv wraps a server command in the forwarder.
func forwarderArgv(path string, args []string) []string {
	return append([]string{egressForwarderPath, egressForwarderArg, strconv.Itoa(egressproxy.ListenPort), "--", path}, args...)
}
