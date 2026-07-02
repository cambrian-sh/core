package agentmgr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// InstanceManager is the pure process/Phenotype lifecycle controller.
// It owns instance boot, PID tracking, socket cleanup, and process
// termination — nothing related to agent selection, A2A, or gRPC.
type InstanceManager struct {
	mu            sync.Mutex
	instances     map[string]*domain.Instance
	agentIndex    map[string][]string // agentID → []instanceID
	cmds          map[string]*exec.Cmd // instanceID → cmd
	pythonPath    string
	substrateAddr string
}

// NewInstanceManager creates an InstanceManager with empty maps.
func NewInstanceManager(pythonPath, substrateAddr string) *InstanceManager {
	return &InstanceManager{
		instances:     make(map[string]*domain.Instance),
		agentIndex:    make(map[string][]string),
		cmds:          make(map[string]*exec.Cmd),
		pythonPath:    pythonPath,
		substrateAddr: substrateAddr,
	}
}

func (im *InstanceManager) getInstances() map[string]*domain.Instance {
	return im.instances
}

func (im *InstanceManager) getAgentIndex() map[string][]string {
	return im.agentIndex
}

// GetInstanceIDs returns all instance IDs for a given agent.
func (im *InstanceManager) GetInstanceIDs(agentID string) []string {
	im.mu.Lock()
	defer im.mu.Unlock()
	ids := im.agentIndex[agentID]
	out := make([]string, len(ids))
	copy(out, ids)
	return out
}

// GetInstance returns the first live instance for a given agent ID.
func (im *InstanceManager) GetInstance(agentID string) (*domain.Instance, bool) {
	im.mu.Lock()
	defer im.mu.Unlock()
	for _, id := range im.agentIndex[agentID] {
		if inst, ok := im.instances[id]; ok {
			return inst, true
		}
	}
	return nil, false
}

// EvictInstance removes a single instance by UUID.
func (im *InstanceManager) EvictInstance(instanceID string) {
	im.mu.Lock()
	defer im.mu.Unlock()

	inst, ok := im.instances[instanceID]
	if !ok {
		return
	}
	if cmd, hasCMD := im.cmds[instanceID]; hasCMD && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	delete(im.cmds, instanceID)
	if inst.SocketPath != "" {
		_ = os.Remove(inst.SocketPath)
	}
	delete(im.instances, instanceID)

	ids := im.agentIndex[inst.AgentID]
	for i, id := range ids {
		if id == instanceID {
			im.agentIndex[inst.AgentID] = append(ids[:i], ids[i+1:]...)
			break
		}
	}
	if len(im.agentIndex[inst.AgentID]) == 0 {
		delete(im.agentIndex, inst.AgentID)
	}
}

func (im *InstanceManager) killAllInstances() {
	im.mu.Lock()
	defer im.mu.Unlock()
	for id, inst := range im.instances {
		if cmd, hasCMD := im.cmds[id]; hasCMD && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		if inst.SocketPath != "" {
			_ = os.Remove(inst.SocketPath)
		}
	}
	im.instances = make(map[string]*domain.Instance)
	im.agentIndex = make(map[string][]string)
	im.cmds = make(map[string]*exec.Cmd)
}

func socketPath(agentID, instanceID string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("cambrian_%s_%s.sock", agentID, instanceID))
}

// buildAgentCmd constructs the OS command for an agent process.
// streamID is only used when def.Trait == TraitDaemon; it is ignored otherwise.
// An empty streamID omits the --stream-id flag even for daemon agents. ADR-0033.
func (im *InstanceManager) buildAgentCmd(def *domain.AgentDefinition, inst *domain.Instance, streamID string) (*exec.Cmd, error) {
	sockPath := socketPath(def.ID, inst.ID)
	var cmd *exec.Cmd
	switch def.Runtime {
	case domain.RuntimePython:
		cmd = exec.Command(im.pythonPath, def.ExecPath,
			"--socket", sockPath,
			"--substrate-addr", im.substrateAddr)
		cmd.Env = append(os.Environ(),
			"PYTHONIOENCODING=utf-8",
			"PYTHONUTF8=1",
		)
	case domain.RuntimeBinary:
		cmd = exec.Command(def.ExecPath,
			"--socket", sockPath,
			"--substrate-addr", im.substrateAddr)
	default:
		return nil, fmt.Errorf("desteklenmeyen runtime: %s", def.Runtime)
	}
	cmd.Dir = def.Dir

	// ADR-0033: daemon-specific flags.
	if def.Trait == domain.TraitDaemon {
		cmd.Args = append(cmd.Args, "--daemon-mode")
		if streamID != "" {
			cmd.Args = append(cmd.Args, "--stream-id", streamID)
		}
	}
	return cmd, nil
}

// GetOrBootInstance returns a running Instance for an agent.
func (im *InstanceManager) GetOrBootInstance(ctx context.Context, def *domain.AgentDefinition, excludeInstanceID string) *domain.Instance {
	im.mu.Lock()
	for _, id := range im.agentIndex[def.ID] {
		if excludeInstanceID != "" && id == excludeInstanceID {
			continue
		}
		if inst, ok := im.instances[id]; ok {
			im.mu.Unlock()
			return inst
		}
	}
	im.mu.Unlock()

	if err := im.bootAgent(ctx, def); err != nil {
		return nil
	}

	im.mu.Lock()
	defer im.mu.Unlock()
	if ids := im.agentIndex[def.ID]; len(ids) > 0 {
		for _, id := range ids {
			if excludeInstanceID != "" && id == excludeInstanceID {
				continue
			}
			return im.instances[id]
		}
	}
	return nil
}

// ReleaseInstance conditionally evicts a JIT instance.
func (im *InstanceManager) ReleaseInstance(instanceID string) {
	im.mu.Lock()
	defer im.mu.Unlock()

	inst, ok := im.instances[instanceID]
	if !ok {
		return
	}
	if inst.Mode == domain.ModeJIT {
		if cmd, hasCMD := im.cmds[instanceID]; hasCMD && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		delete(im.cmds, instanceID)
		if inst.SocketPath != "" {
			_ = os.Remove(inst.SocketPath)
		}
		delete(im.instances, instanceID)
		ids := im.agentIndex[inst.AgentID]
		for i, id := range ids {
			if id == instanceID {
				im.agentIndex[inst.AgentID] = append(ids[:i], ids[i+1:]...)
				break
			}
		}
		if len(im.agentIndex[inst.AgentID]) == 0 {
			delete(im.agentIndex, inst.AgentID)
		}
	}
}

// FindInstanceByToken finds an Instance with the given auth token.
func (im *InstanceManager) FindInstanceByToken(token string) *domain.Instance {
	im.mu.Lock()
	defer im.mu.Unlock()
	for _, inst := range im.instances {
		if inst.AuthToken == token && token != "" {
			return inst
		}
	}
	return nil
}

// EvictAgent kills all instances of an agent.
func (im *InstanceManager) EvictAgent(agentID string) {
	im.mu.Lock()
	defer im.mu.Unlock()
	ids := append([]string{}, im.agentIndex[agentID]...)
	for _, id := range ids {
		if cmd, hasCMD := im.cmds[id]; hasCMD && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		delete(im.cmds, id)
		delete(im.instances, id)
	}
	delete(im.agentIndex, agentID)
}

// KillAllAgents kills every instance in parallel and clears all tracking maps.
func (im *InstanceManager) KillAllAgents(ctx context.Context) error {
	im.mu.Lock()
	defer im.mu.Unlock()

	if len(im.instances) == 0 {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for id := range im.instances {
		cmd, hasCMD := im.cmds[id]
		if !hasCMD || cmd.Process == nil {
			continue
		}
		wg.Add(1)
		go func(p *os.Process) {
			defer wg.Done()
			_ = p.Kill()
		}(cmd.Process)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
		im.instances = make(map[string]*domain.Instance)
		im.agentIndex = make(map[string][]string)
		im.cmds = make(map[string]*exec.Cmd)
		return nil
	case <-cleanupCtx.Done():
		return fmt.Errorf("agent cleanup timed out: %w", cleanupCtx.Err())
	}
}

func (im *InstanceManager) bootAgent(ctx context.Context, def *domain.AgentDefinition) error {
	inst := domain.NewInstance(def.ID)

	cmd, err := im.buildAgentCmd(def, inst, "") // streamID "" for non-daemon boot path
	if err != nil {
		return err
	}
	inst.SocketPath = socketPath(def.ID, inst.ID)

	token, err := generateAuthToken()
	if err != nil {
		return fmt.Errorf("token generation failed: %w", err)
	}
	inst.AuthToken = token
	cmd.Args = append(cmd.Args, "--auth-token", token)

	cmd.Dir = def.Dir
	configureSysProcAttr(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe error: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe error: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go forwardPipe(ctx, stdout, def.ID, false)
	go forwardPipe(ctx, stderr, def.ID, true)

	// Phase 1: Poll for the UDS socket file to appear (fast inode-level check).
	const pollInterval = 50 * time.Millisecond
	const bootTimeout = 10 * time.Second
	deadline := time.Now().Add(bootTimeout)
	for {
		if _, statErr := os.Stat(inst.SocketPath); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return fmt.Errorf("agent %s: socket %s did not appear within %s", def.ID, inst.SocketPath, bootTimeout)
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	// Phase 2: gRPC health check — confirms the gRPC stack is SERVING, not just
	// that the socket inode exists. The Python SDK wires grpc.health.v1 in
	// start_server(); this phase is skipped with a warning if it times out so
	// that agents without the health servicer still boot (graceful degradation).
	if err := waitHealthy(ctx, inst.SocketPath, 5*time.Second); err != nil {
		slog.Warn("agent health check inconclusive — proceeding without confirmation",
			"agent", def.ID, "err", err)
	}

	im.mu.Lock()
	im.cmds[inst.ID] = cmd
	im.instances[inst.ID] = inst
	im.agentIndex[def.ID] = append(im.agentIndex[def.ID], inst.ID)
	im.mu.Unlock()
	return nil
}

// bootDaemonAgent boots a TraitDaemon agent process. Unlike bootAgent it:
//   - Passes streamID to buildAgentCmd so --stream-id is injected.
//   - Serialises params to --daemon-params JSON.
//   - Sets inst.Mode = ModeDaemon before returning.
//
// The caller (SpawnDaemon on AgentManager) is responsible for launching the
// crash-watcher goroutine. ADR-0033.
func (im *InstanceManager) bootDaemonAgent(ctx context.Context, def *domain.AgentDefinition, streamID string, params map[string]any) (*domain.Instance, error) {
	inst := domain.NewInstance(def.ID)
	inst.Mode = domain.ModeDaemon

	cmd, err := im.buildAgentCmd(def, inst, streamID)
	if err != nil {
		return nil, err
	}
	inst.SocketPath = socketPath(def.ID, inst.ID)

	// Append --daemon-params JSON before token injection.
	if len(params) > 0 {
		paramsBytes, jsonErr := json.Marshal(params)
		if jsonErr == nil {
			cmd.Args = append(cmd.Args, "--daemon-params", string(paramsBytes))
		} else {
			slog.Warn("bootDaemonAgent: could not marshal daemon params", "err", jsonErr)
		}
	}

	token, err := generateAuthToken()
	if err != nil {
		return nil, fmt.Errorf("daemon token generation failed: %w", err)
	}
	inst.AuthToken = token
	cmd.Args = append(cmd.Args, "--auth-token", token)
	cmd.Dir = def.Dir
	configureSysProcAttr(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("daemon stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("daemon stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go forwardPipe(ctx, stdout, def.ID, false)
	go forwardPipe(ctx, stderr, def.ID, true)

	// Wait for socket file.
	const pollInterval = 50 * time.Millisecond
	const bootTimeout = 10 * time.Second
	deadline := time.Now().Add(bootTimeout)
	for {
		if _, statErr := os.Stat(inst.SocketPath); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("daemon %s: socket %s did not appear within %s", def.ID, inst.SocketPath, bootTimeout)
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if err := waitHealthy(ctx, inst.SocketPath, 5*time.Second); err != nil {
		slog.Warn("daemon health check inconclusive", "agent", def.ID, "err", err)
	}

	im.mu.Lock()
	im.cmds[inst.ID] = cmd
	im.instances[inst.ID] = inst
	im.agentIndex[def.ID] = append(im.agentIndex[def.ID], inst.ID)
	im.mu.Unlock()

	return inst, nil
}

// GetCmd returns the exec.Cmd for a given instanceID (used by crash watcher). ADR-0033.
func (im *InstanceManager) GetCmd(instanceID string) *exec.Cmd {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.cmds[instanceID]
}

// waitHealthy dials the agent's UDS socket and polls grpc.health.v1 until the
// server reports SERVING or the timeout elapses. Returns nil on SERVING.
func waitHealthy(ctx context.Context, socketPath string, timeout time.Duration) error {
	const pollInterval = 100 * time.Millisecond
	deadline := time.Now().Add(timeout)

	for {
		conn, err := grpc.NewClient(
			"unix:"+socketPath,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err == nil {
			hc := grpc_health_v1.NewHealthClient(conn)
			checkCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			resp, checkErr := hc.Check(checkCtx, &grpc_health_v1.HealthCheckRequest{Service: ""})
			cancel()
			_ = conn.Close()
			if checkErr == nil && resp.Status == grpc_health_v1.HealthCheckResponse_SERVING {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("health check timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// generateAuthToken produces a random hex token for Instance authentication.
func generateAuthToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// unlinkSocket removes a socket file if it exists.
func unlinkSocket(path string) {
	_ = os.Remove(path)
}
