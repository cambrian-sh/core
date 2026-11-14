package agentmgr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/cambrian-sh/core/internal/agentwire"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// ExecuteA2A runs one handoff against a remote A2A agent (no local process). The
// agent-plane Transport calls it for a RuntimeA2A agent: the Transport otherwise
// dials a local instance, and booting an A2A agent fails ("desteklenmeyen
// runtime"), so an A2A agent was reachable only through this package's 3-arg
// CallAgent (T-20261113-agentmanager-callagent-kills-the-shared-jit).
func (m *AgentManager) ExecuteA2A(ctx context.Context, def domain.AgentDefinition, handoff *domain.Handoff) (*domain.Handoff, error) {
	return m.AgentConnector.a2aClient.Execute(ctx, def, handoff)
}

// CallAgent routes a handoff from the Substrate (Server) to the named agent.
// Accepts and returns domain.Handoff; converts to/from proto at the gRPC boundary.
func (m *AgentManager) CallAgent(ctx context.Context, agentName string, handoff *domain.Handoff) (*domain.Handoff, error) {
	def, err := m.Registry.GetAgentByName(ctx, agentName)
	if err != nil {
		return nil, fmt.Errorf("agent not found: %v", err)
	}

	// A2A BRANCH — no local workspace
	if def.Runtime == domain.RuntimeA2A {
		return m.ExecuteA2A(ctx, *def, handoff)
	}

	// 0. TRUTH LAYER: Pre-execution snapshot
	preWD, _ := os.Getwd()
	preEnv := os.Environ()

	// Boot or reuse: find an existing instance for this agent, or boot a new one.
	//
	// Only an instance THIS call booted is released when it returns. A running
	// instance it merely reused may be the one the agent-plane Transport caches
	// (internal/agentplane, GetOrBootInstance); killing it here tore down the
	// Transport's live agent, so its next call failed Unavailable and re-booted
	// (T-20261113-agentmanager-callagent-kills-the-shared-jit). Releasing a
	// self-booted JIT instance keeps this path's one-shot lifecycle unchanged.
	inst, found := m.InstanceManager.GetInstance(agentName)
	bootedHere := false
	if !found {
		bootedHere = true
		if err := m.InstanceManager.bootAgent(ctx, def); err != nil {
			return nil, err
		}
		inst, found = m.InstanceManager.GetInstance(agentName)
		if !found {
			return nil, fmt.Errorf("agent %s booted but no instance found", agentName)
		}
	}

	addr := "unix:" + inst.SocketPath

	// REQ-AGENT-CTX-3: debug logging for context injection
	slog.Debug("agent_context_inject",
		"agent", agentName,
		"working_memory_count", len(handoff.WorkingMemory),
		"context_keys", len(handoff.Context),
	)
	for i, ref := range handoff.WorkingMemory {
		slog.Debug("agent_context_ref",
			"agent", agentName,
			"index", i,
			"cid", ref.CID,
			"type", ref.Type,
			"precision", ref.Precision,
			"activation", ref.Activation,
			"snippet_len", len(ref.Snippet),
		)
	}

	protoResp, errExec := m.executeHandoff(ctx, addr, agentwire.ToProto(handoff))

	if bootedHere {
		defer m.ReleaseInstance(inst.ID)
	}

	if errExec != nil {
		return nil, errExec
	}

	resp := agentwire.FromProto(protoResp)

	// REQ-AGENT-CTX-3: log assembled context length from agent response if available
	if ctxStr := resp.Context["_assembled_context_len"]; ctxStr != "" {
		slog.Info("agent_context_assembled",
			"agent", agentName,
			"context_len", ctxStr,
		)
	}

	// 0. TRUTH LAYER: Post-execution snapshot
	postWD, _ := os.Getwd()
	postEnv := os.Environ()

	forceSync := false
	if postWD != preWD {
		forceSync = true
		slog.Info("🔍 Substrate detected PWD mutation, forcing sync barrier", "old", preWD, "new", postWD)
	}
	if !envsEqual(preEnv, postEnv) {
		forceSync = true
		slog.Info("🔍 Substrate detected ENV mutation, forcing sync barrier")
	}

	// 2. MEMORY BARRIER: Protocol-based trigger. Mutations are recorded as
	// synchronous `mnemonic_action` records (ADR-0049 D1); step output is not
	// captured (RecordExecution was deleted 2026-11-05). The barrier no longer
	// ingests anything. Retained as a barrier signal only.
	if val, ok := resp.Context["_kernel_sync"]; (ok && val == "true") || forceSync {
		slog.Info("⚡ Sync Barrier (signal only; no ingest)", "agent", agentName)
	}

	return resp, nil
}

// CallDaemon routes a handoff to the SPECIFIC per-stream daemon instance the kernel spawned
// for streamID (ADR-0080), rather than any instance of the agent (which CallAgent picks). The
// chat worker pool (internal/agentpool) uses it to hand a conversation turn to a free pool
// worker's stream — there are no per-conversation daemons any more. Returns an error if no daemon is registered/running for streamID.
func (m *AgentManager) CallDaemon(ctx context.Context, streamID string, handoff *domain.Handoff) (*domain.Handoff, error) {
	instanceID, ok := m.DaemonInstanceID(streamID)
	if !ok {
		return nil, fmt.Errorf("CallDaemon: %w: no daemon registered for stream %q", domain.ErrDaemonUnavailable, streamID)
	}
	inst, ok := m.InstanceManager.GetInstanceByID(instanceID)
	if !ok || inst.SocketPath == "" {
		return nil, fmt.Errorf("CallDaemon: %w: daemon instance %q for stream %q not live", domain.ErrDaemonUnavailable, instanceID, streamID)
	}
	protoResp, err := m.executeHandoff(ctx, "unix:"+inst.SocketPath, agentwire.ToProto(handoff))
	if err != nil {
		return nil, err
	}
	return agentwire.FromProto(protoResp), nil
}

func envsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sortedA := make([]string, len(a))
	sortedB := make([]string, len(b))
	copy(sortedA, a)
	copy(sortedB, b)
	sort.Strings(sortedA)
	sort.Strings(sortedB)
	for i := range sortedA {
		if sortedA[i] != sortedB[i] {
			return false
		}
	}
	return true
}

// pythonLogPrefix matches the default `logging` format, `LEVEL:logger:message`.
//
// It is worth special-casing because it is what every Python agent in this repo
// emits: the SDK does not configure a formatter, so `logging.basicConfig` writes
// this shape to STDERR for every level including INFO and DEBUG.
var pythonLogPrefix = regexp.MustCompile(`^(DEBUG|INFO|WARNING|WARN|ERROR|CRITICAL|FATAL):([^:]*):(.*)$`)

// parseLevel maps a level word onto slog. The second return is false for
// anything unrecognised, so a caller can tell "not stated" from "stated as info".
func parseLevel(word string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "debug":
		return slog.LevelDebug, true
	case "info", "notice":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error", "err", "critical", "fatal", "exception":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// forwardPipe relays one agent process's output into the kernel's log.
//
// The level an agent DECLARES wins over the stream it arrived on, and that is
// the whole point of this function. Python's `logging` writes to stderr at every
// level, so keying the level off the stream recorded every agent INFO line as a
// kernel ERROR — which is what the log actually looked like: hundreds of
//
//	{"level":"ERROR","msg":"INFO:__main__:telegram ingress: polling from update_id 0"}
//
// That is not a cosmetic problem. Level is the primary signal an operator
// filters and alerts on, so a stream where almost everything is ERROR is a
// stream where nothing is: the real errors were indistinguishable from a bot
// reporting that it was fine.
//
// Precedence, strongest first:
//
//  1. an explicit `level` field on a JSON line,
//  2. a `LEVEL:logger:` prefix on a plain line,
//  3. the stream it came from — stderr is an error only when nothing said
//     otherwise, which keeps a bare traceback or a panic correctly loud.
func forwardPipe(ctx context.Context, r io.Reader, agentID string, isErr bool) {
	streamLevel := slog.LevelInfo
	if isErr {
		streamLevel = slog.LevelError
	}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()

		var fields map[string]any
		if json.Unmarshal([]byte(line), &fields) == nil {
			msg, _ := fields["msg"].(string)
			if msg == "" {
				msg = line
			}
			delete(fields, "msg")

			// The declared level, from EITHER stream. The old `if !isErr` guard
			// here is the bug: a structured line that said "info" was still
			// recorded as an error purely because it arrived on stderr.
			logLevel := streamLevel
			if lvlStr, ok := fields["level"].(string); ok {
				if parsed, known := parseLevel(lvlStr); known {
					logLevel = parsed
				}
			}
			delete(fields, "level")

			// The SDK's own records arrive as JSON whose `msg` still carries the
			// `LEVEL:logger:` prefix. Lift it out so the level is honoured and the
			// logger becomes an attribute rather than punctuation inside a string
			// that no field query can reach.
			if m := pythonLogPrefix.FindStringSubmatch(msg); m != nil {
				if parsed, known := parseLevel(m[1]); known {
					logLevel = parsed
				}
				if _, taken := fields["logger"]; !taken && m[2] != "" {
					fields["logger"] = m[2]
				}
				msg = strings.TrimSpace(m[3])
			}

			args := []any{"agent_id", agentID}
			for k, v := range fields {
				args = append(args, k, v)
			}
			slog.Log(ctx, logLevel, msg, args...)
			continue
		}

		// Plain text. This is the common path for Python agents, and the one that
		// produced the ERROR flood.
		logLevel := streamLevel
		args := []any{"agent_id", agentID}
		msg := line
		if m := pythonLogPrefix.FindStringSubmatch(line); m != nil {
			if parsed, known := parseLevel(m[1]); known {
				logLevel = parsed
			}
			if m[2] != "" {
				args = append(args, "logger", m[2])
			}
			msg = strings.TrimSpace(m[3])
		}
		slog.Log(ctx, logLevel, msg, args...)
	}
}

func (m *AgentManager) executeHandoff(ctx context.Context, addr string, handoff *pb.Handoff) (*pb.Handoff, error) {
	conn, err := m.DialAgent(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	client := pb.NewAgentServiceClient(conn)
	return client.Execute(ctx, handoff)
}
