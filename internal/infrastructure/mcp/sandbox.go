package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/internal/procenv"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// Trust-tier containment for stdio servers (ADR-0139 D8,
// T-20261105-stdio-mcp-servers-run-with-the-kernel-user-s). Every stdio launch
// — the shared session (connect), a console probe (Probe, through a throwaway
// connector) and a per-user session (dialUser) — goes through
// stdioTransportFor, so no launch path can skip the sandbox.

// containmentStore is each server's effective containment from its last
// launch. Its zero value is ready (the probe connector is built bare).
type containmentStore struct {
	mu sync.RWMutex
	m  map[string]sandbox.Report
}

func (s *containmentStore) set(id string, r sandbox.Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]sandbox.Report{}
	}
	s.m[id] = r
}

func (s *containmentStore) get(id string) (sandbox.Report, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.m[id]
	return r, ok
}

// Containment is a stdio server's effective containment from its most recent
// launch attempt — including a refusal, whose Report says what was missing.
// false when the server has not been launched (or is not stdio).
func (c *Connector) Containment(serverID string) (sandbox.Report, bool) {
	return c.containment.get(serverID)
}

// stdioTransport launches the shared session's process.
func (c *Connector) stdioTransport(ctx context.Context, s ServerConfig, explicit map[string]string) (mcpsdk.Transport, error) {
	return c.stdioTransportFor(ctx, s, explicit, "")
}

// stdioTransportFor launches one stdio server process under its tier's
// containment. principal != "" is a per-user instance: it gets a workdir of
// its own, and its report is not recorded as the server's.
func (c *Connector) stdioTransportFor(ctx context.Context, s ServerConfig, explicit map[string]string, principal string) (mcpsdk.Transport, error) {
	forward := append([]string(nil), s.Env...)
	for k := range explicit {
		forward = append(forward, k)
	}
	cmd := sandbox.Command{
		Path:    s.Endpoint,
		Args:    expandArgs(s.Args, s.EnvValues),
		Env:     procenv.ServerEnv(s.Env, explicit),
		Forward: forward,
	}
	spec := s.Sandbox
	if spec.ServerID == "" {
		spec.ServerID = s.ID
	}
	if principal != "" {
		sum := sha256.Sum256([]byte(principal))
		spec.ServerID = spec.ServerID + "@" + hex.EncodeToString(sum[:6])
	}
	proc, rep, err := sandbox.Start(ctx, spec, cmd)
	if principal == "" {
		rep.ServerID = s.ID
		c.containment.set(s.ID, rep)
	}
	if err != nil {
		if rep.Refused {
			slog.Warn("ADR-0139 D8: stdio MCP server refused by its trust tier", "server", s.ID,
				"tier", rep.Tier, "missing", rep.Missing)
		}
		return nil, err
	}
	slog.Info("ADR-0139 D8: stdio MCP server contained", "server", s.ID, "containment", rep.Summary())
	return &processTransport{proc: proc}, nil
}

// processTransport serves an already-started contained process over the
// SDK's newline-delimited JSON framing. Closing the connection runs the
// process's shutdown (close stdin, grace, kill the tree).
type processTransport struct{ proc *sandbox.Process }

func (t *processTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	return (&mcpsdk.IOTransport{Reader: t.proc, Writer: stdinOnly{t.proc}}).Connect(ctx)
}

// stdinOnly makes the writer's Close a no-op: the IOTransport closes the
// reader (the whole process) first, which already closed stdin.
type stdinOnly struct{ p *sandbox.Process }

func (w stdinOnly) Write(b []byte) (int, error) { return w.p.Write(b) }
func (stdinOnly) Close() error                  { return nil }

// ContainedStdioTransport is the exported, credential-agnostic entry point to
// the contained stdio launch, for a caller outside this package that builds a
// per-principal session (the premium accounts layer, after the per-user
// credential code moves behind a core port). The caller resolves the
// credential and passes it as explicit env (NAME=value, given to this process
// only); containment wraps whatever it is handed. instance != "" gives the
// process a workdir of its own (pass a principal id; it is hashed into the
// path) and does not overwrite the server's recorded containment.
func (c *Connector) ContainedStdioTransport(ctx context.Context, s ServerConfig, explicit map[string]string, instance string) (mcpsdk.Transport, error) {
	return c.stdioTransportFor(ctx, s, explicit, instance)
}
