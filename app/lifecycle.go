// `stop` / `status` — minimal lifecycle for the detached kernel that `setup`
// starts (ADR-0122). With the Bun CLI retired, the kernel binary is the whole
// product surface: setup / run / status / stop / migrate in one artifact.
// Mechanism mirrors the old CLI lifecycle (CLI-006 MVP): a pid file at
// <prefix>/orchestrator.pid plus log redirection under <prefix>/logs/.
package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// probeHealth checks the grpc.health.v1 overall status at addr, trying
// plaintext first and then TLS with certificate verification OFF — a kernel
// with SEC-03 TLS configured (e.g. a self-signed loopback cert so cloudflared's
// http2Origin can reach it) answers the same DB-gated status. This is a local
// liveness probe, not an authenticity claim, which is why skipping verification
// is acceptable here and only here.
func probeHealth(ctx context.Context, addr string, timeout time.Duration) (healthpb.HealthCheckResponse_ServingStatus, error) {
	var lastErr error
	for _, creds := range []credentials.TransportCredentials{
		insecure.NewCredentials(),
		credentials.NewTLS(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}), //nolint:gosec // loopback liveness probe, not an authenticity check
	} {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
		if err != nil {
			lastErr = err
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		resp, cerr := healthpb.NewHealthClient(conn).Check(cctx, &healthpb.HealthCheckRequest{})
		cancel()
		_ = conn.Close()
		if cerr == nil {
			return resp.GetStatus(), nil
		}
		lastErr = cerr
	}
	return healthpb.HealthCheckResponse_SERVICE_UNKNOWN, lastErr
}

// setupPrefix resolves the install root the same way setup does.
func setupPrefix() string {
	if p := os.Getenv("CAMBRIAN_HOME"); p != "" {
		return p
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cambrian")
	}
	return "."
}

func readPidFile(prefix string) int {
	b, err := os.ReadFile(filepath.Join(prefix, "orchestrator.pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// readServerPort reads server.port from the installed config bundle; falls
// back to the default when the bundle is absent or unreadable.
func readServerPort(prefix string) string {
	b, err := os.ReadFile(filepath.Join(prefix, "configs", "config.json"))
	if err == nil {
		var cfg struct {
			Server struct {
				Port string `json:"port"`
			} `json:"server"`
		}
		if json.Unmarshal(b, &cfg) == nil && cfg.Server.Port != "" {
			return cfg.Server.Port
		}
	}
	return "50051"
}

// spawnDetached starts the kernel binary detached with cwd=dir (so
// ResolveBaseDir's CWD sentinel finds the install bundle) and logs redirected
// under logDir. The parent's file handles are closed after start — the child
// holds its own inherited copies.
func spawnDetached(bin, dir, logDir string) (int, error) {
	out, err := os.OpenFile(filepath.Join(logDir, "orchestrator.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	errf, err := os.OpenFile(filepath.Join(logDir, "orchestrator.err.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer errf.Close()
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = out, errf
	applyDetach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// RunStatus implements the `status` subcommand: pid-file process liveness plus
// the grpc.health.v1 DB-gated readiness. Exit 0 when SERVING, 1 otherwise.
func RunStatus(ctx context.Context) int {
	prefix := setupPrefix()
	if pid := readPidFile(prefix); pid != 0 && pidAlive(pid) {
		fmt.Printf("process  running (pid %d)\n", pid)
	} else {
		fmt.Println("process  not running (no live pid file)")
	}
	addr := net.JoinHostPort("localhost", readServerPort(prefix))
	status, err := probeHealth(ctx, addr, 3*time.Second)
	if err == nil {
		fmt.Printf("health   %s on %s\n", status, addr)
		if status == healthpb.HealthCheckResponse_SERVING {
			return 0
		}
		return 1
	}
	fmt.Printf("health   unreachable on %s\n", addr)
	return 1
}

// RunStop implements the `stop` subcommand: graceful terminate (SIGTERM on
// unix; hard stop on Windows, see setup_spawn_windows.go), a bounded wait,
// then a kill escalation. Stopping an already-stopped kernel is a no-op.
func RunStop(_ context.Context) int {
	prefix := setupPrefix()
	pidPath := filepath.Join(prefix, "orchestrator.pid")
	pid := readPidFile(prefix)
	if pid == 0 || !pidAlive(pid) {
		fmt.Println("orchestrator not running (no live pid file)")
		_ = os.Remove(pidPath)
		return 0
	}
	if err := terminateProcess(pid); err != nil {
		fmt.Printf("could not stop pid %d: %v\n", pid, err)
		return 1
	}
	for i := 0; i < 20 && pidAlive(pid); i++ {
		time.Sleep(500 * time.Millisecond)
	}
	if pidAlive(pid) {
		killProcess(pid)
	}
	_ = os.Remove(pidPath)
	fmt.Printf("✓ orchestrator stopped (pid %d)\n", pid)
	return 0
}
