package agentmgr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/substrate/harness"
)

// CallAgent routes a handoff from the Substrate (Server) to the named agent.
// Accepts and returns domain.Handoff; converts to/from proto at the gRPC boundary.
func (m *AgentManager) CallAgent(ctx context.Context, agentName string, handoff *domain.Handoff) (*domain.Handoff, error) {
	def, err := m.Registry.GetAgentByName(ctx, agentName)
	if err != nil {
		return nil, fmt.Errorf("agent not found: %v", err)
	}

	// A2A BRANCH — no local workspace, no CoW snapshot needed
	if def.Runtime == domain.RuntimeA2A {
		return m.AgentConnector.a2aClient.Execute(ctx, *def, handoff)
	}

	// 0. TRUTH LAYER: Pre-execution snapshot
	preWD, _ := os.Getwd()
	preEnv := os.Environ()

	taskID := agentName
	if handoff.Context != nil {
		if tid, ok := handoff.Context["task_id"]; ok && tid != "" {
			taskID = tid
		}
	}
	snapshotKey := agentName + taskID

	hash, snapErr := harness.Snapshot(def.Dir)
	if snapErr != nil {
		slog.Warn("Snapshot failed; proceeding without CoW rollback capability",
			"agent", agentName, "dir", def.Dir, "err", snapErr)
	} else {
		m.AgentConnector.snapshotMu.Lock()
		m.AgentConnector.snapshots[snapshotKey] = hash
		m.AgentConnector.snapshotMu.Unlock()
	}

	// Boot or reuse: find an existing instance for this agent, or boot a new one.
	inst, found := m.InstanceManager.GetInstance(agentName)
	if !found {
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

	protoResp, errExec := m.executeHandoff(ctx, addr, domainToProtoHandoff(handoff))

	defer m.ReleaseInstance(inst.ID)

	if errExec == nil {
		m.snapshotMu.Lock()
		delete(m.snapshots, snapshotKey)
		m.snapshotMu.Unlock()
	}

	if errExec != nil {
		return nil, errExec
	}

	resp := protoToDomainHandoff(protoResp)

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

	// 2. MEMORY BARRIER: Protocol-based trigger. ADR-0049 D3: the step output is
	// already recorded by RecordExecution (the `step_N:` fact) and any mutations as
	// synchronous `mnemonic_action` records — so the barrier no longer re-ingests a
	// duplicate payload row. Retained as a barrier signal only.
	if val, ok := resp.Context["_kernel_sync"]; (ok && val == "true") || forceSync {
		slog.Info("⚡ Sync Barrier (step output already recorded; no duplicate ingest)", "agent", agentName)
	}

	return resp, nil
}

// domainToProtoHandoff converts a domain Handoff to proto for gRPC calls.
// ADR-0022 Phase 3: WorkingMemory is copied into proto working_memory so
// cognitive agents receive prior-step ContextRefs via the Python SDK.
func domainToProtoHandoff(d *domain.Handoff) *pb.Handoff {
	if d == nil {
		return nil
	}
	h := &pb.Handoff{
		Id:            d.ID,
		FromAgent:     d.FromAgent,
		ToAgent:       d.ToAgent,
		Confidence:    d.Confidence,
		Uncertainties: d.Uncertainties,
		Metadata:      d.Context,
	}
	if d.Payload != nil {
		h.Payload = &pb.Object{
			Id:       d.Payload.ID,
			Type:     d.Payload.Type,
			Data:     d.Payload.Data,
			Metadata: d.Payload.Metadata,
		}
	}
	for _, ref := range d.WorkingMemory {
		h.WorkingMemory = append(h.WorkingMemory, &pb.ContextRef{
			Cid:        string(ref.CID),
			Type:       ref.Type,
			Labels:     ref.Labels,
			Activation: ref.Activation,
			Snippet:    ref.Snippet,
			Precision:  ref.Precision,
		})
	}
	return h
}

// protoToDomainHandoff converts a proto Handoff to domain for internal use.
// ADR-0022 Phase 3: working_memory is copied back into domain WorkingMemory.
func protoToDomainHandoff(h *pb.Handoff) *domain.Handoff {
	if h == nil {
		return nil
	}
	d := &domain.Handoff{
		ID:            h.Id,
		FromAgent:     h.FromAgent,
		ToAgent:       h.ToAgent,
		Confidence:    h.Confidence,
		Uncertainties: h.Uncertainties,
		Context:       h.GetMetadata(),
	}
	if h.Payload != nil {
		d.Payload = &domain.Payload{
			ID:       h.Payload.Id,
			Type:     h.Payload.Type,
			Data:     h.Payload.Data,
			Metadata: h.Payload.Metadata,
		}
	}
	for _, ref := range h.WorkingMemory {
		d.WorkingMemory = append(d.WorkingMemory, domain.ContextRef{
			CID:        domain.CID(ref.Cid),
			Type:       ref.Type,
			Labels:     ref.Labels,
			Activation: ref.Activation,
			Snippet:    ref.Snippet,
			Precision:  ref.Precision,
		})
	}
	return d
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

func forwardPipe(ctx context.Context, r io.Reader, agentID string, isErr bool) {
	level := slog.LevelInfo
	if isErr {
		level = slog.LevelError
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

			logLevel := level
			if !isErr {
				if lvlStr, ok := fields["level"].(string); ok {
					switch strings.ToLower(lvlStr) {
					case "error", "err":
						logLevel = slog.LevelError
					case "warn", "warning":
						logLevel = slog.LevelWarn
					case "debug":
						logLevel = slog.LevelDebug
					}
				}
			}
			delete(fields, "level")

			args := []any{"agent_id", agentID}
			for k, v := range fields {
				args = append(args, k, v)
			}
			slog.Log(ctx, logLevel, msg, args...)
		} else {
			slog.Log(ctx, level, line, "agent_id", agentID)
		}
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
