//go:build windows

package sandbox

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/procenv"
)

// ENG-2215: on the Windows native runner an MCP server whose declared hosts
// are enforced runs in an AppContainer with no network capability, the test
// binary (standing in for the kernel executable) re-run inside as the
// forwarder, relaying the server's proxy port to the kernel's proxy over a
// unix socket in the workdir.

func egressPolicy(t *testing.T) *Policy {
	t.Helper()
	p := DefaultPolicy()
	p.WorkRoot = shortTempDir(t) // AF_UNIX paths are limited to 108 bytes
	p.Egress = startedBroker(t)
	return p
}

// readLine returns the first stdout line of a contained process.
func readLine(t *testing.T, proc *Process) string {
	t.Helper()
	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(proc)
		if sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	select {
	case l := <-lines:
		return l
	case <-time.After(2 * time.Minute):
		t.Fatal("the contained process printed nothing")
	}
	return ""
}

// Offline: the container has no route but the proxy, and the proxy refuses
// an undeclared host; the report says the hosts are enforced.
func TestWindowsEgressContainerHoldsServerToItsHosts(t *testing.T) {
	p := egressPolicy(t)
	spec := p.Resolve("egress-win", "third-party", "", false, ServerSpec{Network: NetworkEgress, Hosts: []string{"example.com"}})
	plan := Plan(context.Background(), spec, fixture(t, "connect", "www.wikipedia.org:443"))
	if !slices.Contains(plan.Mechanisms, MechanismEgressProxy) || !slices.Contains(plan.Mechanisms, MechanismAppContainer) {
		t.Fatalf("plan does not enforce the hosts: %s", plan.Summary())
	}
	proc, rep, err := Start(context.Background(), spec, fixture(t, "connect", "www.wikipedia.org:443"))
	if err != nil {
		t.Fatalf("start: %v (%s)", err, rep.Summary())
	}
	line := readLine(t, proc)
	if err := proc.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	t.Logf("probe: %s", line)
	t.Logf("containment: %s", rep.Summary())
	connect, direct, _ := strings.Cut(strings.TrimPrefix(line, "connect="), "|direct=")
	if !strings.Contains(connect, "403") {
		t.Errorf("undeclared host through the proxy: %q, want 403", connect)
	}
	if !strings.HasPrefix(direct, "error") {
		t.Errorf("a direct connection from the container: %q, want no route", direct)
	}
	if !slices.Contains(rep.Enforced, ControlNetwork) || rep.Degraded() || rep.Refused {
		t.Errorf("report: %s", rep.Summary())
	}
}

// Without a proxy the plan keeps the low-integrity runner and records the
// hosts, as before.
func TestWindowsEgressWithoutProxyIsRecorded(t *testing.T) {
	p := DefaultPolicy()
	p.WorkRoot = shortTempDir(t)
	spec := p.Resolve("egress-win", "third-party", "", false, ServerSpec{Network: NetworkEgress, Hosts: []string{"example.com"}})
	rep := Plan(context.Background(), spec, fixture(t, "echo"))
	if slices.Contains(rep.Mechanisms, MechanismEgressProxy) || !strings.Contains(strings.Join(rep.Notes, ";"), "not filtered") {
		t.Fatalf("no proxy: %s", rep.Summary())
	}
}

func TestResolveCommandUsesTheServersPath(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, base := exe[:strings.LastIndexAny(exe, `\/`)], exe[strings.LastIndexAny(exe, `\/`)+1:]
	got, err := resolveCommand(Command{Path: strings.TrimSuffix(base, ".exe"), Env: []string{"Path=" + dir}})
	if err != nil || !strings.EqualFold(got, exe) {
		t.Fatalf("resolve = %q, %v; want %q", got, err, exe)
	}
}

// TestLiveWindowsEgressAllowlistEnforced is the live check with a real
// interpreter in the container (the probe of TestLiveDockerEgressAllowlistEnforced):
//
//	SANDBOX_EGRESS_E2E=1 go test -run TestLiveWindowsEgress ./internal/sandbox/
//
// SANDBOX_EGRESS_PYTHON names python.exe (default: `uv python find`). Needs
// the internet.
func TestLiveWindowsEgressAllowlistEnforced(t *testing.T) {
	if os.Getenv("SANDBOX_EGRESS_E2E") != "1" {
		t.Skip("live check: set SANDBOX_EGRESS_E2E=1 (needs python and the internet)")
	}
	// A failed launch's stderr tail is logged at debug level.
	prev := slog.SetLogLoggerLevel(slog.LevelDebug)
	t.Cleanup(func() { slog.SetLogLoggerLevel(prev) })
	py := os.Getenv("SANDBOX_EGRESS_PYTHON")
	if py == "" {
		out, err := exec.Command("uv", "python", "find").Output()
		if err != nil {
			t.Fatalf("no python: set SANDBOX_EGRESS_PYTHON (%v)", err)
		}
		py = strings.TrimSpace(string(out))
	}
	p := egressPolicy(t)
	spec := p.Resolve("egress-probe", "third-party", "", false, ServerSpec{Network: NetworkEgress, Hosts: []string{"example.com"}})
	proc, rep, err := Start(context.Background(), spec, Command{Path: py, Args: []string{"-I", "-c", egressProbe}, Env: procenv.ServerEnv(nil, nil)})
	if err != nil {
		t.Fatalf("start: %v (%s)", err, rep.Summary())
	}
	r := readProbe(t, proc)
	if err := proc.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if !slices.Contains(rep.Mechanisms, MechanismAppContainer) {
		t.Errorf("not launched in an AppContainer: %s", rep.Summary())
	}
	// Some Windows Pythons cannot verify example.com's chain even outside any
	// sandbox (found on the owner's box, 2026-11-19: CERTIFICATE_VERIFY_FAILED
	// with no container involved), so the declared host counts as reached when
	// the tunnel opened — a refusal reads "Tunnel connection failed: 403" — and
	// the raw CONNECT below must answer 200.
	if strings.Contains(r.Allowed, "CERTIFICATE_VERIFY_FAILED") {
		t.Logf("declared host: TLS verification failed in this Python (not the sandbox); the tunnel opened")
		r.Allowed = "200"
	}
	assertEnforced(t, r, rep)

	proc, rep, err = Start(context.Background(), spec, fixture(t, "connect", "example.com:443"))
	if err != nil {
		t.Fatalf("start: %v (%s)", err, rep.Summary())
	}
	line := readLine(t, proc)
	_ = proc.Close()
	if !strings.HasPrefix(line, "connect=HTTP/1.1 200") || !strings.Contains(line, "|direct=error") {
		t.Errorf("CONNECT to the declared host from the container: %q, want 200 and no direct route", line)
	}
}

// finalPath follows a junction (filepath.EvalSymlinks does not) and returns a
// plain path; commandBase strips the launcher extension.
func TestFinalPathFollowsJunctions(t *testing.T) {
	root := shortTempDir(t)
	target := root + `\cpython-3.13.14`
	link := root + `\cpython-3.13`
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	got := finalPath(link)
	if strings.HasPrefix(got, `\?\`) || !strings.EqualFold(got, finalPath(target)) || !strings.HasSuffix(strings.ToLower(got), `cpython-3.13.14`) {
		t.Fatalf("finalPath(%s) = %s", link, got)
	}
	if b := commandBase(`C:\Users\x\.local\bin\UVX.exe`); b != "uvx" {
		t.Fatalf("commandBase = %q", b)
	}
}
