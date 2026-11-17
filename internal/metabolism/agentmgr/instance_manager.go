package agentmgr

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/procenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// InstanceManager is the pure process/Phenotype lifecycle controller.
// It owns instance boot, PID tracking, socket cleanup, and process
// termination — nothing related to agent selection, A2A, or gRPC.
type InstanceManager struct {
	// bootCount is the process-lifetime tally of agent SPAWNS (not cache hits).
	// ADR-0100 P2 reads a delta around a selection decision to measure how many
	// agent processes that decision cost — the auction booted every candidate to
	// ask it for a bid; dispatch boots only the winner. Atomic and first in the
	// struct so it stays 64-bit aligned on 32-bit platforms.
	bootCount  atomic.Uint64
	mu         sync.Mutex
	instances  map[string]*domain.Instance
	agentIndex map[string][]string     // agentID → []instanceID
	cmds       map[string]*exec.Cmd    // instanceID → cmd
	exits      map[string]*processExit // instanceID → the one Wait per child (process_exit.go)
	// runtimeExes maps an interpreted runtime to its executable (ADR-0125):
	// python from metabolism.python_executable, bun/node from their config keys
	// (empty ⇒ $PATH fallback at spawn). RuntimeBinary needs no interpreter.
	runtimeExes   map[domain.AgentRuntime]string
	substrateAddr string
	// envPassthrough names extra non-secret environment variables an operator
	// wants agents to inherit beyond the OS base allowlist (SEC-01). Secrets are
	// stripped regardless. Empty by default.
	envPassthrough []string
	// envExtras are explicit KEY=VALUE pairs the KERNEL sets in every agent's
	// environment, derived from config (e.g. CAMBRIAN_TOOL_MENU_K from
	// execution.tools.tool_menu_k). Distinct from envPassthrough on purpose: a
	// passthrough only carries what the kernel's own environment happens to
	// hold, which makes a config knob depend on .env — extras flow
	// config → agent with the kernel's env not involved at all.
	envExtras []string
	// tokens is the in-memory registry of live per-instance agent-plane tokens
	// (ADR-0157 D4; agent_credential_env.go). Built by NewInstanceManager and
	// never replaced: every spawn mints into it, every teardown revokes from it,
	// and the agent-plane verifier and FindInstanceByToken read it.
	tokens *authz.InstanceTokens
	// agentTLSCA / agentTLSServerName are the TLS facts a spawned agent needs to
	// reach a pure-TLS kernel (ADR-0130 D4; SetAgentTransport). Empty ⇒ the
	// agent dials plaintext, which is right on loopback.
	agentTLSCA         string
	agentTLSServerName string
	// kernelSocket / agentUnixTLSServerName: how a kernel-only (network-denied)
	// sandboxed instance reaches the kernel (ADR-0157 S2; SetAgentTransport).
	kernelSocket           KernelSocketOpener
	agentUnixTLSServerName string
	// agentMemLimitMB is the GLOBAL default memory cap (SEC-01), used for agents
	// that don't declare their own. 0 = no default.
	agentMemLimitMB int
	// bootTimeout bounds the wait for a spawned agent's socket
	// (execution.agents.agent_boot_timeout_ms); 0 ⇒ agentBootTimeout.
	bootTimeout time.Duration
	// manifestFor resolves an agent's manifest so its per-agent memory_limit_mb
	// can override the global default (SEC-01). nil ⇒ only the global default is
	// used. Set by NewAgentManager (which holds the registry).
	manifestFor func(ctx context.Context, agentID string) *domain.AgentManifest
	// capCleanups holds the per-instance resource-cap teardown (Windows: closes
	// the Job Object handle, which reaps the whole process tree via
	// kill-on-close). Keyed by instance ID; invoked on eviction.
	capCleanups map[string]func()
	// containment is execution.agents.sandbox (containment.go); its zero value
	// refuses third-party agents (fail closed).
	containment AgentContainment
	// reported records the agent ids whose ADR-0157 containment report was
	// already logged at INFO/WARN (agent_sandbox.go reportContainment).
	reported sync.Map
}

// SetEnvPassthrough configures the operator-declared agent env allowlist extras
// (SEC-01). Secret-looking names are ignored even if listed.
func (im *InstanceManager) SetEnvPassthrough(names []string) {
	im.envPassthrough = names
}

// SetEnvExtras configures the config-derived KEY=VALUE pairs injected into
// every agent environment. Values come from kernel CONFIG, never from the
// kernel's own environment.
func (im *InstanceManager) SetEnvExtras(kv []string) {
	im.envExtras = kv
}

// SetAgentMemoryLimitMB configures the GLOBAL default memory cap (SEC-01). 0
// disables the default; per-agent manifest limits still apply.
func (im *InstanceManager) SetAgentMemoryLimitMB(mb int) {
	im.agentMemLimitMB = mb
}

// SetAgentBootTimeout sets how long a boot waits for the agent's socket before
// it kills the child and fails (execution.agents.agent_boot_timeout_ms). <= 0
// keeps the built-in agentBootTimeout. A child that exits early still fails at
// once; this only bounds a child that is alive but slow to serve — e.g. the
// kg_extractor's spaCy load while the kernel boots its other agents
// (T-20261102-kg-extractor-cold-start-drops-the-first-kg).
func (im *InstanceManager) SetAgentBootTimeout(d time.Duration) {
	im.bootTimeout = d
}

// SetManifestResolver wires the per-agent manifest lookup used to honor an
// agent's declared memory_limit_mb over the global default (SEC-01).
func (im *InstanceManager) SetManifestResolver(f func(ctx context.Context, agentID string) *domain.AgentManifest) {
	im.manifestFor = f
}

// effectiveMemLimitMB returns the memory cap to apply to an agent (SEC-01):
//   - a per-agent declared manifest limit (> 0) always wins;
//   - privileged system organs (docling/reranker/kg_extractor/scout, …) are
//     trusted kernel infrastructure and are typically the memory-heavy ones
//     (Docling/torch/spaCy) — a fleet-wide default must NOT kill them, so they
//     are capped ONLY by their own declared limit (0 ⇒ uncapped);
//   - every other (user/untrusted) agent gets the global default.
//
// So enabling a global cap contains a runaway user agent without endangering the
// heavy system agents.
func (im *InstanceManager) effectiveMemLimitMB(ctx context.Context, agentID string) int {
	if im.manifestFor != nil {
		if m := im.manifestFor(ctx, agentID); m != nil && m.MemoryLimitMB > 0 {
			return m.MemoryLimitMB
		}
	}
	if domain.IsSystemAgent(agentID) {
		return 0 // trusted organ — never capped by the fleet-wide default
	}
	return im.agentMemLimitMB
}

// spawnMemLimitMB is the cap actually passed to applyResourceCaps for a spawn.
// On Linux, JS runtimes are exempted (ADR-0125 D8): the cap there is RLIMIT_AS
// (virtual address space) and V8 reserves multi-GB virtual ranges it never
// commits, so a limit sized for Python kills a healthy bun/node agent at boot.
// Windows Job Objects limit COMMITTED memory and apply to JS agents unchanged.
func (im *InstanceManager) spawnMemLimitMB(ctx context.Context, def *domain.AgentDefinition) int {
	if goruntime.GOOS == "linux" && domain.IsJSRuntime(def.Runtime) {
		return 0
	}
	return im.effectiveMemLimitMB(ctx, def.ID)
}

// NewInstanceManager creates an InstanceManager with empty maps.
func NewInstanceManager(pythonPath, substrateAddr string) *InstanceManager {
	return &InstanceManager{
		instances:     make(map[string]*domain.Instance),
		agentIndex:    make(map[string][]string),
		cmds:          make(map[string]*exec.Cmd),
		exits:         make(map[string]*processExit),
		capCleanups:   make(map[string]func()),
		tokens:        authz.NewInstanceTokens(),
		runtimeExes:   map[domain.AgentRuntime]string{domain.RuntimePython: pythonPath},
		substrateAddr: substrateAddr,
	}
}

// SetRuntimeExecutable configures the interpreter for an interpreted runtime
// (ADR-0125). An empty path clears any prior value (JS runtimes then fall back
// to $PATH at spawn; Python keeps failing at Start as before).
func (im *InstanceManager) SetRuntimeExecutable(rt domain.AgentRuntime, path string) {
	if path == "" {
		delete(im.runtimeExes, rt)
		return
	}
	im.runtimeExes[rt] = path
}

// runtimeExecutable resolves the interpreter for a JS runtime: the configured
// path wins; otherwise the runtime's own name is looked up on $PATH so a stock
// `bun`/`node` install works with zero config. The error names the config key.
func (im *InstanceManager) runtimeExecutable(rt domain.AgentRuntime) (string, error) {
	if p := im.runtimeExes[rt]; p != "" {
		return p, nil
	}
	if p, err := exec.LookPath(string(rt)); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("runtime %q has no configured executable and %q is not on PATH (set metabolism.%s_executable)", rt, rt, rt)
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

// GetInstanceByID returns the instance with the given UUID, if live. Used by CallDaemon
// (ADR-0080) to route a turn to the specific per-conversation daemon the kernel spawned.
func (im *InstanceManager) GetInstanceByID(instanceID string) (*domain.Instance, bool) {
	im.mu.Lock()
	defer im.mu.Unlock()
	inst, ok := im.instances[instanceID]
	return inst, ok
}

// EvictInstance removes a single instance by UUID.
func (im *InstanceManager) EvictInstance(instanceID string) {
	im.mu.Lock()
	defer im.mu.Unlock()

	inst, ok := im.instances[instanceID]
	if !ok {
		return
	}
	// ADR-0157 D4.2: the token dies with the instance, before the kill, so no
	// RPC is admitted on it while the process is being reaped.
	im.tokens.Revoke(instanceID)
	if cmd, hasCMD := im.cmds[instanceID]; hasCMD && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	delete(im.cmds, instanceID)
	delete(im.exits, instanceID)
	// SEC-01: close the Job Object (Windows kill-on-close reaps grandchildren the
	// bare Process.Kill above cannot reach) / release the rlimit handle.
	if cleanup, ok := im.capCleanups[instanceID]; ok {
		cleanup()
		delete(im.capCleanups, instanceID)
	}
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
		im.tokens.Revoke(id) // ADR-0157 D4.2
		if cmd, hasCMD := im.cmds[id]; hasCMD && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		if cleanup, ok := im.capCleanups[id]; ok {
			cleanup()
			delete(im.capCleanups, id)
		}
		if inst.SocketPath != "" {
			_ = os.Remove(inst.SocketPath)
		}
	}
	im.instances = make(map[string]*domain.Instance)
	im.agentIndex = make(map[string][]string)
	im.cmds = make(map[string]*exec.Cmd)
	im.exits = make(map[string]*processExit)
}

// maxSocketPathLen is the sockaddr_un limit every gRPC implementation enforces.
//
// It is 108 bytes including the NUL terminator, so 107 usable. This is not a
// tunable: a path one byte over is refused at bind time by the OS, and the
// agent dies during startup with a message about ports that says nothing about
// names being too long.
const maxSocketPathLen = 107

// socketPath builds an agent instance's socket path, BOUNDED.
//
// The obvious construction -- temp dir + agent id + a full UUID + ".sock" --
// runs to 109 characters for an agent named "telegram_ingress_support" under a
// normal Windows temp directory. The default Telegram ingress happened to land
// on exactly 107 and worked, so every per-bot agent failed to start while the
// one everybody tests with was fine, and the error named ports rather than
// lengths.
//
// The instance id is shortened to 8 hex characters (still 4 billion values
// within one agent id, and the full id remains in the logs), and the agent id is
// truncated only if the budget demands it -- with a short digest appended so two
// long ids that share a prefix cannot collide onto one socket.
func socketPath(agentID, instanceID string) string {
	return socketPathIn(os.TempDir(), agentID, instanceID)
}

// socketPathIn is socketPath under dir: the OS temp directory, or an
// instance's socket directory when agents are sandboxed (ADR-0157 D2.6).
func socketPathIn(dir, agentID, instanceID string) string {
	shortInstance := instanceID
	if len(shortInstance) > 8 {
		shortInstance = strings.ReplaceAll(shortInstance, "-", "")[:8]
	}

	// Everything the name needs beyond the agent id itself.
	fixed := len(filepath.Join(dir, "cambrian__.sock")) + len(shortInstance)
	budget := maxSocketPathLen - fixed
	name := agentID
	if budget < 1 {
		// A temp directory this long leaves no room for a readable name at all.
		// A digest still produces a working socket, which beats an agent that
		// cannot start.
		name = ""
	} else if len(name) > budget {
		digest := fmt.Sprintf("%x", sha1.Sum([]byte(agentID)))[:6]
		if budget > len(digest)+1 {
			name = name[:budget-len(digest)-1] + "-" + digest
		} else {
			name = digest[:min(budget, len(digest))]
		}
	}
	return filepath.Join(dir, fmt.Sprintf("cambrian_%s_%s.sock", name, shortInstance))
}

// buildAgentCmd constructs the OS command for an agent process.
// streamID is only used when def.Trait == TraitDaemon; it is ignored otherwise.
// An empty streamID omits the --stream-id flag even for daemon agents. ADR-0033.
func (im *InstanceManager) buildAgentCmd(def *domain.AgentDefinition, inst *domain.Instance, streamID string) (*exec.Cmd, error) {
	sockPath := im.agentSocketPath(def.ID, inst.ID)
	var cmd *exec.Cmd
	switch def.Runtime {
	case domain.RuntimePython:
		cmd = exec.Command(im.runtimeExes[domain.RuntimePython], def.ExecPath,
			"--socket", sockPath,
			"--substrate-addr", im.substrateAddr)
		// SEC-01: deny-by-default env — never inherit the kernel's secrets.
		cmd.Env = append(procenv.Allowlisted(im.envPassthrough),
			"PYTHONIOENCODING=utf-8",
			"PYTHONUTF8=1",
		)
		cmd.Env = append(cmd.Env, im.envExtras...)
	case domain.RuntimeBun, domain.RuntimeNode:
		// ADR-0125: same argv contract as Python; the interpreter comes from
		// config or $PATH (runtimeExecutable errors with the config key named).
		interp, err := im.runtimeExecutable(def.Runtime)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", def.ID, err)
		}
		cmd = exec.Command(interp, def.ExecPath,
			"--socket", sockPath,
			"--substrate-addr", im.substrateAddr)
		// SEC-01 deny-by-default env; NO_COLOR/FORCE_COLOR keep stdout as plain
		// one-line JSON for the kernel's log forwarder (forwardPipe).
		cmd.Env = append(procenv.Allowlisted(im.envPassthrough),
			"NO_COLOR=1",
			"FORCE_COLOR=0",
		)
		cmd.Env = append(cmd.Env, im.envExtras...)
	case domain.RuntimeBinary:
		cmd = exec.Command(def.ExecPath,
			"--socket", sockPath,
			"--substrate-addr", im.substrateAddr)
		// SEC-01: a nil cmd.Env would inherit os.Environ() (all secrets) — set
		// the allowlisted environment explicitly for native agents too.
		cmd.Env = append(procenv.Allowlisted(im.envPassthrough), im.envExtras...)
	default:
		return nil, fmt.Errorf("desteklenmeyen runtime: %s", def.Runtime)
	}
	cmd.Dir = def.Dir
	// Per-agent launch arguments, after the kernel's own flags. A declarative
	// profile agent is the one runtime worker_agent.py started as `--profile <name>
	// --agent-id <id>` (agent stack v2, storage/profile_discovery.go); every other
	// agent has none. argv, not env, for the same reason as --agent-id below: the
	// SEC-01 deny-by-default environment stays closed.
	cmd.Args = append(cmd.Args, def.LaunchArgs...)
	// The agent's own plane credential (ADR-0130 D1) is NOT added here: it is a
	// per-instance token minted by the boot path at spawn (ADR-0157 D4,
	// mintInstanceCredential), so building a command never issues a credential.
	// ADR-0130 D4: how to reach a TLS-only kernel (agent_credential_env.go).
	cmd.Env = append(cmd.Env, im.agentTransportEnv()...)

	// ADR-0033: daemon-specific flags.
	if def.Trait == domain.TraitDaemon {
		cmd.Args = append(cmd.Args, "--daemon-mode")
		if streamID != "" {
			cmd.Args = append(cmd.Args, "--stream-id", streamID)
		}
		// The REGISTRY id, so the process can declare itself as the identity the
		// kernel knows it by.
		//
		// Discovery derives the registry id from the filename while a signal's
		// sender comes from the id the process declares for itself, and nothing
		// made the two agree — when they diverge a reply is generated and then has
		// nowhere to go. Passing it removes the divergence by construction, and it
		// is what lets ONE script run as several distinct principals: N bots, N
		// registrations, N surfaces, one file.
		//
		// A flag rather than an environment variable deliberately. Per-agent env
		// would mean reopening the SEC-01 deny-by-default environment, which is
		// the boundary that stops kernel secrets reaching agent processes; an
		// argv flag carries no such risk. Unknown flags are ignored by the SDK,
		// so older agents are unaffected.
		cmd.Args = append(cmd.Args, "--agent-id", def.ID)
	}
	return cmd, nil
}

// GetOrBootInstance returns a running Instance for an agent, booting one if
// none is tracked. A failed boot returns its cause (socket timeout, the child
// exiting first, a missing runtime dependency) and logs it at WARN: it used to
// return nil with the error discarded, so callers could only say "boot agent
// failed", and the kg_extractor's cold-start timeouts were invisible
// (T-20261102-kg-extractor-cold-start-drops-the-first-kg).
//
// A tracked JIT instance whose process has exited is evicted and replaced, not
// returned: before, a crashed or killed agent stayed "running" for the life of
// the kernel and every call to it failed against its dead socket.
func (im *InstanceManager) GetOrBootInstance(ctx context.Context, def *domain.AgentDefinition, excludeInstanceID string) (*domain.Instance, error) {
	im.mu.Lock()
	var dead []string
	for _, id := range im.agentIndex[def.ID] {
		if excludeInstanceID != "" && id == excludeInstanceID {
			continue
		}
		inst, ok := im.instances[id]
		if !ok {
			continue
		}
		if inst.Mode != domain.ModeDaemon && im.exitedLocked(id) {
			dead = append(dead, id) // daemons are restarted by their crash watcher
			continue
		}
		im.mu.Unlock()
		return inst, nil
	}
	im.mu.Unlock()
	for _, id := range dead {
		slog.WarnContext(ctx, "agent instance has exited; evicting it and booting a new one",
			"agent_id", def.ID, "instance_id", id)
		im.EvictInstance(id)
	}

	t := time.Now()
	if err := im.bootAgent(ctx, def); err != nil {
		slog.WarnContext(ctx, "agent boot failed", "agent_id", def.ID,
			"elapsed_ms", time.Since(t).Milliseconds(), "cause", err)
		return nil, fmt.Errorf("boot agent %s: %w", def.ID, err)
	}

	im.mu.Lock()
	defer im.mu.Unlock()
	if ids := im.agentIndex[def.ID]; len(ids) > 0 {
		for _, id := range ids {
			if excludeInstanceID != "" && id == excludeInstanceID {
				continue
			}
			return im.instances[id], nil
		}
	}
	return nil, fmt.Errorf("boot agent %s: booted but no instance is tracked", def.ID)
}

// exitedLocked reports whether the child registered under instanceID has
// exited. Callers hold im.mu.
func (im *InstanceManager) exitedLocked(instanceID string) bool {
	pe, ok := im.exits[instanceID]
	if !ok {
		return false
	}
	select {
	case <-pe.done:
		return true
	default:
		return false
	}
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
		im.tokens.Revoke(instanceID) // ADR-0157 D4.2
		if cmd, hasCMD := im.cmds[instanceID]; hasCMD && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		delete(im.cmds, instanceID)
		delete(im.exits, instanceID)
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

// FindInstanceByToken finds the live Instance a per-instance token was minted
// for (the SignalStream gate, Watcher.ValidateToken). It answers from the SAME
// registry the agent-plane verifier reads (ADR-0157 D4.3), so a revoked token
// fails both checks at once. An unknown, revoked or empty token is nil.
//
// The token registry is the authority, not the instance map: a token is live
// exactly while its process is (minted before the start, revoked at exit or
// teardown). An instance still booting — started, but not yet registered while
// the boot waits for its socket and health probe — is answered from the
// token's identity, because a daemon may open its SignalStream and emit its
// first signal inside that window; requiring the map entry dropped it.
func (im *InstanceManager) FindInstanceByToken(token string) *domain.Instance {
	id, ok := im.tokens.Lookup(token)
	if !ok {
		return nil
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	if inst, ok := im.instances[id.InstanceID]; ok && inst.AgentID == id.AgentID {
		return inst
	}
	return &domain.Instance{ID: id.InstanceID, AgentID: id.AgentID}
}

// EvictAgent kills all instances of an agent.
func (im *InstanceManager) EvictAgent(agentID string) {
	im.mu.Lock()
	defer im.mu.Unlock()
	ids := append([]string{}, im.agentIndex[agentID]...)
	for _, id := range ids {
		im.tokens.Revoke(id) // ADR-0157 D4.2: every instance of the agent
		if cmd, hasCMD := im.cmds[id]; hasCMD && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		delete(im.cmds, id)
		delete(im.exits, id)
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
		im.tokens.Revoke(id) // ADR-0157 D4.2
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
		im.exits = make(map[string]*processExit)
		return nil
	case <-cleanupCtx.Done():
		return fmt.Errorf("agent cleanup timed out: %w", cleanupCtx.Err())
	}
}

// BootCount returns the process-lifetime number of agent spawns. A delta across
// a selection decision is the cost, in cold starts, of making that decision
// (ADR-0100 P2).
func (im *InstanceManager) BootCount() uint64 { return im.bootCount.Load() }

func (im *InstanceManager) bootAgent(ctx context.Context, def *domain.AgentDefinition) error {
	_, err := im.bootAgentInstance(ctx, def, true)
	return err
}

// BootDedicatedInstance boots a fresh instance of def for ONE caller (ADR-0139
// D12: a subagent child, which must not queue behind its own single-threaded
// parent on the shared instance). The instance is tracked — so EvictInstance
// kills it and process-exit watching applies — but NOT indexed under the agent
// id, so no ordinary call (GetInstance, GetOrBootInstance) is ever routed to it.
// The caller owns its life and must EvictInstance it when done.
func (im *InstanceManager) BootDedicatedInstance(ctx context.Context, def *domain.AgentDefinition) (*domain.Instance, error) {
	return im.bootAgentInstance(ctx, def, false)
}

func (im *InstanceManager) bootAgentInstance(ctx context.Context, def *domain.AgentDefinition, index bool) (*domain.Instance, error) {
	im.bootCount.Add(1)
	inst := domain.NewInstance(def.ID)

	// ADR-0139 D8: the agent's trust tier decides whether it may start and its
	// memory cap (containment.go) — before anything is spawned.
	memMB, err := im.admitLaunch(ctx, def)
	if err != nil {
		return nil, err
	}
	// PLAT-01: fail fast with the missing dep named, not a silent spawn crash.
	if err := im.verifyRuntimeDeps(ctx, def); err != nil {
		return nil, err
	}

	cmd, err := im.buildAgentCmd(def, inst, "") // streamID "" for non-daemon boot path
	if err != nil {
		return nil, err
	}
	inst.SocketPath = im.agentSocketPath(def.ID, inst.ID)

	// ADR-0157 D4: a fresh per-instance token, in the environment only. It is
	// revoked if this boot fails, and otherwise when the process exits or the
	// instance is torn down.
	credEnv, err := im.mintInstanceCredential(def.ID, inst.ID)
	if err != nil {
		return nil, err
	}
	booted := false
	defer func() {
		if !booted {
			im.tokens.Revoke(inst.ID)
		}
	}()
	cmd.Env = append(cmd.Env, credEnv...)

	cmd.Dir = def.Dir

	var stdout, stderr io.ReadCloser
	var onExit func()
	if im.containment.Sandbox != nil {
		// ADR-0157 S1: the same argv/env, launched through internal/sandbox.
		if cmd, stdout, stderr, onExit, err = im.startSandboxed(ctx, def, inst, cmd, memMB); err != nil {
			return nil, err
		}
	} else {
		configureSysProcAttr(cmd)

		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("stdout pipe error: %v", err)
		}
		stderr, err = cmd.StderrPipe()
		if err != nil {
			return nil, fmt.Errorf("stderr pipe error: %v", err)
		}
		// SEC-01 / lifetime: bind the child's life to ours before it exists, so a hard kill
		// of the kernel cannot leave it running.
		containLifetime(cmd)
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		// SEC-01: cap the agent's memory after start (Windows Job Object / Unix
		// RLIMIT_AS). A failure here is non-fatal — the agent still runs uncapped and
		// the error is logged rather than aborting boot.
		if cleanup, capErr := applyResourceCaps(cmd, memMB); capErr != nil {
			if agentTrustTier(def) == agentTierThirdParty {
				// A third-party agent's memory cap is a containment control, not
				// best-effort: no cap, no agent.
				_ = cmd.Process.Kill()
				return nil, fmt.Errorf("agent %s (third-party): memory cap not applied: %w", def.ID, capErr)
			}
			slog.Warn("SEC-01: failed to apply agent resource caps", "agent_id", def.ID, "err", capErr)
		} else if cleanup != nil {
			im.mu.Lock()
			im.capCleanups[inst.ID] = cleanup
			im.mu.Unlock()
		}
	}
	go forwardPipe(ctx, stdout, def.ID, false)
	go forwardPipe(ctx, stderr, def.ID, true)

	// Phase 1: wait for the UDS socket (fast inode-level check) — or for the boot
	// timeout, the caller's ctx, or the child EXITING, whichever comes first
	// (process_exit.go).
	exit := im.watchExitThen(inst.ID, cmd, onExit)
	if err := awaitSocket(ctx, cmd, exit, inst.SocketPath, "agent "+def.ID, im.bootTimeout); err != nil {
		im.forgetExit(inst.ID)
		return nil, err
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
	if index {
		im.agentIndex[def.ID] = append(im.agentIndex[def.ID], inst.ID)
	}
	im.mu.Unlock()
	booted = true
	return inst, nil
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

	// ADR-0139 D8: tier admission and memory cap (containment.go).
	memMB, err := im.admitLaunch(ctx, def)
	if err != nil {
		return nil, err
	}
	// PLAT-01: fail fast with the missing dep named, not a silent spawn crash.
	if err := im.verifyRuntimeDeps(ctx, def); err != nil {
		return nil, err
	}

	cmd, err := im.buildAgentCmd(def, inst, streamID)
	if err != nil {
		return nil, err
	}
	inst.SocketPath = im.agentSocketPath(def.ID, inst.ID)

	// Per-daemon launch data rides argv (SEC-01); the credential does not.
	if len(params) > 0 {
		paramsBytes, jsonErr := json.Marshal(params)
		if jsonErr == nil {
			cmd.Args = append(cmd.Args, "--daemon-params", string(paramsBytes))
		} else {
			slog.Warn("bootDaemonAgent: could not marshal daemon params", "err", jsonErr)
		}
	}

	// ADR-0157 D4: the daemon instance's own token, env only; revoked on a
	// failed boot, at exit and at teardown.
	credEnv, err := im.mintInstanceCredential(def.ID, inst.ID)
	if err != nil {
		return nil, err
	}
	booted := false
	defer func() {
		if !booted {
			im.tokens.Revoke(inst.ID)
		}
	}()
	cmd.Env = append(cmd.Env, credEnv...)
	cmd.Dir = def.Dir

	var stdout, stderr io.ReadCloser
	var onExit func()
	if im.containment.Sandbox != nil {
		// ADR-0157 S1: the same argv/env, launched through internal/sandbox.
		if cmd, stdout, stderr, onExit, err = im.startSandboxed(ctx, def, inst, cmd, memMB); err != nil {
			return nil, err
		}
	} else {
		configureSysProcAttr(cmd)

		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("daemon stdout pipe: %w", err)
		}
		stderr, err = cmd.StderrPipe()
		if err != nil {
			return nil, fmt.Errorf("daemon stderr pipe: %w", err)
		}
		// SEC-01 / lifetime: bind the child's life to ours before it exists, so a hard kill
		// of the kernel cannot leave it running.
		containLifetime(cmd)
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		// SEC-01: daemons are the longest-lived, most-exposed agents — cap first.
		if cleanup, capErr := applyResourceCaps(cmd, memMB); capErr != nil {
			if agentTrustTier(def) == agentTierThirdParty {
				_ = cmd.Process.Kill()
				return nil, fmt.Errorf("daemon %s (third-party): memory cap not applied: %w", def.ID, capErr)
			}
			slog.Warn("SEC-01: failed to apply daemon resource caps", "agent_id", def.ID, "err", capErr)
		} else if cleanup != nil {
			im.mu.Lock()
			im.capCleanups[inst.ID] = cleanup
			im.mu.Unlock()
		}
	}
	go forwardPipe(ctx, stdout, def.ID, false)
	go forwardPipe(ctx, stderr, def.ID, true)

	// Wait for the socket file — or for the child exiting first (process_exit.go).
	// The crash watcher in daemon_spawner.go consumes the SAME exit, so the
	// child is waited on exactly once.
	exit := im.watchExitThen(inst.ID, cmd, onExit)
	if err := awaitSocket(ctx, cmd, exit, inst.SocketPath, "daemon "+def.ID, im.bootTimeout); err != nil {
		im.forgetExit(inst.ID)
		return nil, err
	}
	if err := waitHealthy(ctx, inst.SocketPath, 5*time.Second); err != nil {
		slog.Warn("daemon health check inconclusive", "agent", def.ID, "err", err)
	}

	im.mu.Lock()
	im.cmds[inst.ID] = cmd
	im.instances[inst.ID] = inst
	im.agentIndex[def.ID] = append(im.agentIndex[def.ID], inst.ID)
	im.mu.Unlock()
	booted = true

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

// unlinkSocket removes a socket file if it exists.
func unlinkSocket(path string) {
	_ = os.Remove(path)
}
