package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/procenv"
	"github.com/cambrian-sh/core/internal/sandbox"
	"github.com/cambrian-sh/core/pkg/egress"
)

// ServerConfig is one MCP server's connection info, mapped from operator config
// at the composition root (ADR-0043 D2/D9). Auth secrets are read from the named
// environment variable, never inlined.
type ServerConfig struct {
	ID           string
	Transport    string   // "stdio" | "http"
	Endpoint     string   // URL (http) or command (stdio)
	Args         []string // stdio command args
	AuthType     string   // "none" | "bearer" | "header"
	AuthHeader   string   // header name when AuthType == "header"
	AuthTokenEnv string   // env var holding the credential
	// Env names the environment variables a STDIO server may inherit from the
	// kernel, on top of the OS essentials (ADR-0139 D8). Everything else is
	// withheld: the child no longer sees the kernel's provider keys,
	// CAMBRIAN_SECRET_KEY or Langfuse keys. A credential-shaped name passes only
	// when it is named here (firecrawl-mcp needs FIRECRAWL_API_KEY); the
	// kernel's own CAMBRIAN_* / LANGFUSE_* variables never pass. Ignored for
	// http/sse transports, which launch nothing.
	Env []string
	// EnvValues are literal, non-secret env values for this stdio server only
	// (config env_values). An args element "${NAME}" is replaced by
	// EnvValues[NAME] at launch.
	EnvValues map[string]string
	// SecretEnv names env variables whose values come from the encrypted store
	// (EnvSecretName), resolved at CONNECT time so a console change takes effect
	// on the next (re)connect. Injected into this server's process only.
	SecretEnv []string
	// Sandbox is the server's resolved trust-tier containment (ADR-0139 D8),
	// set at the composition root from mcp.sandbox + the integration catalog.
	// A zero Spec launches at the strict default tier with built-in limits
	// (sandbox.Spec.effective): a path that forgot to resolve it gets MORE
	// containment, never none. Ignored for http/sse.
	Sandbox sandbox.Spec
	// Tools is operator policy keyed by the server's advertised (unqualified)
	// tool name. Applied to the discovered SystemTool — the server's own metadata
	// is descriptive only (A1.5).
	Tools map[string]ToolPolicy
	// DefaultPolicy applies to every advertised tool with no entry in Tools, so
	// an operator can mark a whole server (sequential-thinking, a mock) read-only
	// without listing each tool. nil means FAIL CLOSED: an unlisted tool is
	// Dangerous with effects {read, write, egress} (UnlistedToolPolicy).
	DefaultPolicy *ToolPolicy
	// DefaultClassificationTags apply to every tool this server advertises unless a
	// per-tool policy overrides them. Without this, dynamically discovered tools are
	// untagged and no tag predicate can restrict them.
	DefaultClassificationTags []string

	// CredentialScope is the ADR-0140 credential model: "" / "shared" (one
	// credential for the server), "per_user" (every call uses the run
	// principal's own credential, never another) or "either" (the principal's
	// when connected, else the shared one only when SharedFallback).
	CredentialScope string
	// UserInject is how a principal's token reaches the server: "env"
	// (stdio, variable UserInjectName of a per-principal process), "bearer" or
	// "header" (http/sse, per request; header UserInjectName).
	UserInject     string
	UserInjectName string
	SharedFallback bool

	// AllowPrivateNetwork opts this http/sse endpoint into loopback / private /
	// link-local targets (config mcp.servers[].allow_private_network, ADR-0153).
	// Without it the guarded egress dialer refuses them.
	AllowPrivateNetwork bool
}

// egressPolicy is the guarded-egress policy this server's http/sse transport
// dials under (ADR-0153, ENG-1419): any port and cleartext allowed (the operator
// typed the URL), non-public address space only with the explicit opt-in.
func (s ServerConfig) egressPolicy() egress.Policy {
	return egress.EndpointPolicy(s.AllowPrivateNetwork)
}

// guardedHTTPClient is the http/sse transport's client over base: the guarded
// egress transport when base is nil, every redirect hop re-validated and
// stripped of credentials, no client-wide timeout (the SSE GET stream is
// long-lived; each call is bounded by its own ctx, execution.tools
// call_timeout_ms).
func (s ServerConfig) guardedHTTPClient(wrap func(base http.RoundTripper) http.RoundTripper) *http.Client {
	p := s.egressPolicy()
	var rt http.RoundTripper = egress.NewTransport(p, nil)
	if wrap != nil {
		rt = wrap(rt)
	}
	return &http.Client{Transport: rt, CheckRedirect: egress.CheckRedirect(p)}
}

// ToolPolicy is the operator-set policy for one discovered tool (ADR-0043 D4/D9).
type ToolPolicy struct {
	Dangerous      bool
	DataWriteKinds []string
	// ClassificationTags are the domain tags a policy can act on (ADR-0085 D2).
	// Operator-set like everything else here: a remote server does not get to
	// classify its own tools, for the same reason it does not get to declare itself
	// safe — that would let the thing being governed choose its own governance.
	ClassificationTags []string
	// Effects are the operator-declared effect classes (ADR-0086): "read",
	// "write", "egress", "spend", "admin". Declared effects win over inference;
	// without them the effects are inferred from the fields above, as for any
	// tool. An effect outside the closed set makes the registry refuse the tool.
	Effects []string
	// StagedInputs marks an execution tool whose `inputs` argument the kernel
	// stages into a per-run read-only folder (domain.SystemTool.StagedInputs).
	StagedInputs bool
}

// UnlistedToolPolicy is the policy for a discovered tool the operator has said
// nothing about — neither a per-tool entry nor a server DefaultPolicy (ADR-0139
// D8: "an unlisted MCP tool is Dangerous with effects {write, egress}"). A remote
// server's tools are discovered at runtime, and until this default every one of
// them registered as non-dangerous `read` — so super-shell's command tool and
// filesystem's write tool bypassed the approval gate and the egress policy.
// `read` is kept alongside write/egress because every tool observes something
// (domain.InferEffects adds it to every tool too).
func UnlistedToolPolicy() ToolPolicy {
	return ToolPolicy{
		Dangerous: true,
		Effects:   []string{string(domain.EffectRead), string(domain.EffectWrite), string(domain.EffectEgress)},
	}
}

// policyFor resolves the operator policy for one advertised tool: its own
// entry, else the server default, else the fail-closed UnlistedToolPolicy.
func policyFor(toolName string, policy map[string]ToolPolicy, def *ToolPolicy) ToolPolicy {
	if p, ok := policy[toolName]; ok {
		return p
	}
	if def != nil {
		return *def
	}
	return UnlistedToolPolicy()
}

// policyListed reports whether the operator said anything about toolName: its own
// entry or a server default. False means policyFor fell back to UnlistedToolPolicy.
func policyListed(toolName string, policy map[string]ToolPolicy, def *ToolPolicy) bool {
	if _, ok := policy[toolName]; ok {
		return true
	}
	return def != nil
}

// applyPolicy stamps operator policy onto a discovered tool.
func applyPolicy(st *domain.SystemTool, p ToolPolicy, defaultTags []string) {
	st.Dangerous = p.Dangerous
	st.DataWriteKinds = p.DataWriteKinds
	st.StagedInputs = p.StagedInputs
	st.ClassificationTags = defaultTags
	if len(p.ClassificationTags) > 0 {
		// A per-tool list REPLACES the server default rather than adding to it, so
		// one tool in a domain-bound server can be classified differently without
		// silently inheriting the boundary it was meant to leave.
		st.ClassificationTags = p.ClassificationTags
	}
	if len(p.Effects) > 0 {
		st.Effects = make([]domain.ToolEffect, 0, len(p.Effects))
		for _, e := range p.Effects {
			st.Effects = append(st.Effects, domain.ToolEffect(e))
		}
	}
}

// Connector dials configured MCP servers, discovers their tools, and serves the
// live client sessions to the Handler (it implements SessionSource). It is the
// only place outside this package's Handler that touches the SDK. One warm
// session per server; an unreachable server is skipped (graceful degradation).
type Connector struct {
	client   *mcpsdk.Client
	mu       sync.RWMutex
	sessions map[string]*mcpsdk.ClientSession
	// watches tracks the per-server health/reconnect goroutine's cancel func, so
	// a server saved or removed at RUNTIME (contract 0097) can have its loop
	// replaced or stopped without touching the others.
	watches map[string]context.CancelFunc

	// health is each server's live state (integrations surface). gens guards
	// it against a stale writer: every arm/drop bumps a server's generation, and
	// a watch loop may only record health under the generation it was armed
	// with, so a loop racing its own cancellation cannot overwrite "stopped".
	health   map[string]*ServerHealth
	gens     map[string]uint64
	observer func(ServerHealth)

	// pingWait bounds one health ping; 0 ⇒ defaultPingTimeout
	// (mcp.ping_timeout_ms, set at construction by WithTimeouts).
	pingWait time.Duration
	// handshakeWait bounds one connect handshake and one boot tools/list;
	// 0 ⇒ defaultHandshakeTimeout (mcp.handshake_timeout_ms, set at
	// construction by WithTimeouts; handshake.go).
	handshakeWait time.Duration

	// configs is each armed server's config, for per-call session selection
	// (ADR-0140: a per_user server's call needs its scope and injection mode).
	configs map[string]ServerConfig
	// users is the per-(server, principal) session pool (peruser.go).
	users *userPool

	// containment is each stdio server's effective containment from its last
	// launch (sandbox.go); its zero value is ready to use.
	containment containmentStore

	// creds are the connector's credential sources, fixed at construction
	// (secrets.go CredentialSources).
	creds CredentialSources
	// toolLists persists each per_user server's last discovered tool list
	// (toollists.go); nil means not persisted.
	toolLists domain.SealedRecordStore
}

// ConnectorOption configures an optional, non-credential dependency of a
// connector at construction.
type ConnectorOption func(*Connector)

// ConnectorTimeouts are the connector's operator-set time bounds
// (mcp.handshake_timeout_ms / mcp.ping_timeout_ms, ENG-2003: the Zero-Hardcode
// Rule — a threshold is config, not a code constant). A zero field keeps the
// built-in default (60 s handshake, 10 s ping).
type ConnectorTimeouts struct {
	// Handshake bounds one connect handshake (transport connect + initialize)
	// and one boot tools/list, on the shared and the per-user path alike.
	Handshake time.Duration
	// Ping bounds one health ping of a connected server.
	Ping time.Duration
}

// WithTimeouts sets the connector's time bounds at construction. Applied only
// through NewConnector, so a bound is never changed under a running session.
func WithTimeouts(t ConnectorTimeouts) ConnectorOption {
	return func(c *Connector) {
		c.handshakeWait = t.Handshake
		c.pingWait = t.Ping
	}
}

// NewConnector constructs an empty connector with a single shared MCP client,
// resolving every credential from creds (the zero value: environment
// variables only, no per-user accounts — the bare OSS shape).
func NewConnector(creds CredentialSources, opts ...ConnectorOption) *Connector {
	c := newConnector(mcpsdk.NewClient(kernelClientInfo, nil), creds)
	for _, o := range opts {
		o(c)
	}
	return c
}

func newConnector(client *mcpsdk.Client, creds CredentialSources) *Connector {
	return &Connector{
		client:   client,
		sessions: map[string]*mcpsdk.ClientSession{},
		watches:  map[string]context.CancelFunc{},
		health:   map[string]*ServerHealth{},
		gens:     map[string]uint64{},
		configs:  map[string]ServerConfig{},
		users:    newUserPool(creds.User, creds.UserLimits),
		creds:    creds,
	}
}

// Session implements SessionSource.
func (c *Connector) Session(serverID string) (*mcpsdk.ClientSession, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.sessions[serverID]
	return s, ok
}

// ConnectAll dials every configured server, discovers its tools, and returns them
// as namespaced SystemTools (mcp:<id>/<tool>) for registration. A server that
// fails to connect or discover is logged and skipped — it never fails the others
// or the kernel (ADR-0043 D8).
//
// Servers are dialled CONCURRENTLY (T-20261102-kernel-boot-connects-mcp-servers-one-after):
// each connect + discovery is independent, and sequentially they cost every boot the
// SUM of the servers' start-up times (stdio servers spawn a process; 4-6 s each, ~25 s
// for five). The result is still assembled in configured order, so tool registration
// order is deterministic and identical to the sequential loop's.
func (c *Connector) ConnectAll(ctx context.Context, servers []ServerConfig) []domain.SystemTool {
	perServer := make([][]domain.SystemTool, len(servers))
	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Add(1)
		go func(i int, s ServerConfig) {
			defer wg.Done()
			perServer[i] = c.connectAndDiscover(ctx, s)
		}(i, s)
	}
	wg.Wait()
	var tools []domain.SystemTool
	for _, ts := range perServer {
		tools = append(tools, ts...)
	}
	return tools
}

// connectAndDiscover is one server's boot connection: dial, store the session, discover
// its tools. A failure at either step is logged and yields no tools (ADR-0043 D8).
func (c *Connector) connectAndDiscover(ctx context.Context, s ServerConfig) []domain.SystemTool {
	start := time.Now()
	gen := c.generation(s.ID)
	c.rememberConfig(s)
	if s.PerUser() {
		// ADR-0140: no shared session exists for a per_user server; Watch
		// discovers its tools through the first connected principal.
		c.setHealth(s.ID, gen, func(h *ServerHealth) { h.State = HealthAwaitingUser })
		return nil
	}
	c.setHealth(s.ID, gen, func(h *ServerHealth) { h.State = HealthStarting })
	sess, secrets, err := c.connect(ctx, s)
	if err != nil {
		msg := redact(err.Error(), secrets)
		slog.Warn("ADR-0043: MCP server connect failed (skipping)", "server", s.ID, "err", msg)
		c.setHealth(s.ID, gen, func(h *ServerHealth) { h.State, h.LastError, h.ToolCount = HealthError, msg, 0 })
		return nil
	}
	c.mu.Lock()
	c.sessions[s.ID] = sess
	c.mu.Unlock()

	discovered, derr := c.boundedDiscover(ctx, s, sess)
	if derr != nil {
		msg := redact(derr.Error(), secrets)
		slog.Warn("ADR-0043: MCP tool discovery failed", "server", s.ID, "err", msg)
		c.setHealth(s.ID, gen, func(h *ServerHealth) {
			h.State, h.LastError, h.ToolCount = HealthError, "connected, but listing tools failed: "+msg, 0
		})
		return nil
	}
	slog.Info("ADR-0043: MCP server connected", "server", s.ID, "tools", len(discovered),
		"elapsed_ms", time.Since(start).Milliseconds())
	c.setHealth(s.ID, gen, func(h *ServerHealth) {
		h.State, h.LastError, h.ToolCount, h.LastHandshake = HealthReady, "", len(discovered), time.Now()
	})
	return discovered
}

// connect dials one server. It returns the secret values it injected so the
// caller can redact them from any error before the error is logged or shown.
func (c *Connector) connect(ctx context.Context, s ServerConfig) (*mcpsdk.ClientSession, []string, error) {
	var transport mcpsdk.Transport
	var secrets []string
	switch s.Transport {
	case "http": // Streamable HTTP (2025-03-26 spec) — single endpoint, POST + optional SSE
		hc, tok := s.httpClient(ctx, c.creds)
		secrets = appendSecret(secrets, tok)
		transport = &mcpsdk.StreamableClientTransport{
			Endpoint:   s.Endpoint,
			HTTPClient: hc,
			// Stateless Streamable-HTTP servers (e.g. FastMCP httpStream stateless:true,
			// which self-hosted Firecrawl uses) don't serve the optional standalone
			// server→client SSE GET stream. Disable it so init doesn't fail on the
			// follow-up GET; request/response still works (responses ride the POST).
			DisableStandaloneSSE: true,
		}
	case "sse": // legacy HTTP+SSE (2024-11-05 spec) — e.g. self-hosted Firecrawl's /sse
		hc, tok := s.httpClient(ctx, c.creds)
		secrets = appendSecret(secrets, tok)
		transport = &mcpsdk.SSEClientTransport{Endpoint: s.Endpoint, HTTPClient: hc}
	case "stdio":
		explicit, injected := s.explicitEnv(ctx, c.creds)
		secrets = append(secrets, injected...)
		// ADR-0139 D8: deny-by-default environment, never the kernel's own, and
		// the trust tier's process containment (sandbox.go).
		t, err := c.stdioTransport(ctx, s, explicit)
		if err != nil {
			return nil, secrets, err
		}
		transport = t
	default:
		return nil, nil, fmt.Errorf("unknown transport %q (want stdio|http|sse)", s.Transport)
	}
	sess, err := c.connectSession(ctx, transport) // handshake bounded (ENG-1528)
	return sess, secrets, err
}

// explicitEnv builds the explicit environment for a stdio server: its literal
// env_values plus each secret_env name resolved through the store (with the D5
// env-first precedence when the name is also passed through). It returns the
// secret values separately, for redaction.
func (s ServerConfig) explicitEnv(ctx context.Context, creds CredentialSources) (map[string]string, []string) {
	if len(s.EnvValues) == 0 && len(s.SecretEnv) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(s.EnvValues)+len(s.SecretEnv))
	for k, v := range s.EnvValues {
		out[k] = v
	}
	passthrough := make(map[string]bool, len(s.Env))
	for _, n := range s.Env {
		passthrough[procenv.Key(n)] = true
	}
	var secrets []string
	for _, name := range s.SecretEnv {
		envVar := ""
		if passthrough[procenv.Key(name)] {
			envVar = name
		}
		if v := creds.envSecretFor(ctx, s.ID, name, envVar); v != "" {
			out[name] = v
			secrets = append(secrets, v)
		}
	}
	return out, secrets
}

// expandArgs replaces an args element that is exactly "${NAME}" with
// values[NAME]. An element naming a value that is unset is DROPPED rather than
// passed literally: a server handed the string "${ROOT}" as a path would act on
// a directory of that name. Partial interpolation is deliberately not
// supported: one element, one value, so a value can never split into several
// arguments or smuggle a flag.
func expandArgs(args []string, values map[string]string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if name, ok := Placeholder(a); ok {
			if v, kernel := KernelLaunchValue(name); kernel {
				// A kernel-provided value: no setting can supply or override it
				// (env_values refuses CAMBRIAN_* names).
				if v != "" {
					out = append(out, v)
				}
				continue
			}
			if v := values[name]; v != "" {
				out = append(out, v)
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

// Placeholder reports whether a is exactly "${NAME}" and returns NAME.
func Placeholder(a string) (string, bool) {
	if len(a) > 3 && a[0] == '$' && a[1] == '{' && a[len(a)-1] == '}' {
		return a[2 : len(a)-1], true
	}
	return "", false
}

func appendSecret(list []string, v string) []string {
	if v == "" {
		return list
	}
	return append(list, v)
}

// httpClient builds the HTTP client for an http/sse transport, injecting the
// static credential when configured (ADR-0043 D9). The token resolves through
// tokenFor (env first, then the ADR-0101 store), so a token saved from the
// console is picked up on the next (re)connect without a restart.
//
// It dials through the guarded egress transport (ADR-0153, ENG-1419): before,
// it used a bare http.Client over http.DefaultTransport, so an operator (or a
// model granted register_mcp) could point a server at loopback, RFC1918 or the
// cloud metadata address.
func (s ServerConfig) httpClient(ctx context.Context, creds CredentialSources) (*http.Client, string) {
	var tok string
	if s.AuthType != "" && s.AuthType != "none" {
		tok = creds.tokenFor(ctx, s.ID, s.AuthTokenEnv)
		return s.guardedHTTPClient(func(base http.RoundTripper) http.RoundTripper {
			return &authRoundTripper{base: base, authType: s.AuthType, header: s.AuthHeader, token: tok}
		}), tok
	}
	return s.guardedHTTPClient(nil), tok
}

// ToolSink receives a server's current tool set on each (re)discovery, and a
// removal when a server drops (ADR-0043 D8 health-gating / ADR-0044 re-sync). The
// kernel implements it to keep the tool registry AND the semantic-retrieval index
// in sync — registering/de-registering tools and adding/removing their vectors.
type ToolSink interface {
	SetServerTools(ctx context.Context, serverID string, tools []domain.SystemTool)
	RemoveServerTools(ctx context.Context, serverID string)
}

// Watch runs a background health + reconnect loop per server (ADR-0043 D8),
// blocking until ctx is done. For a connected server it pings every interval; on
// failure it drops the server's tools (sink.RemoveServerTools → they leave the
// menu) and reconnects with exponential backoff, re-discovering and re-publishing
// on success (sink.SetServerTools → re-registered + re-indexed, ADR-0044). A
// server unreachable at boot is retried here until it comes up.
//
// It blocks on ctx rather than on the goroutines, because the server set is no
// longer fixed at boot: AddServer (contract 0097) attaches loops under this same
// ctx after Watch has started, and a WaitGroup taken over the boot list would
// declare the watcher finished while runtime-added servers are still being
// health-checked.
func (c *Connector) Watch(ctx context.Context, servers []ServerConfig, sink ToolSink, interval time.Duration) {
	for _, s := range servers {
		// armWatch, NOT AddServer: these servers were already connected by
		// ConnectAll and their tools are registered — the teardown AddServer does
		// for a runtime re-save would churn every healthy boot connection.
		c.armWatch(ctx, s, sink, interval)
	}
	// ADR-0140: per-principal sessions idle past the limit are closed (their
	// processes killed) by this loop; all of them are closed at shutdown.
	c.users.mu.Lock()
	idle := c.users.limits.Idle
	c.users.mu.Unlock()
	tick := idle / 2
	if tick <= 0 || tick > 30*time.Second {
		tick = 30 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.closeAllUsers()
			return
		case <-t.C:
			c.ReapIdleUsers()
		}
	}
}

// AddServer arms one server under the connector's lifecycle at RUNTIME: any
// existing loop, session and published tools for the same id are dropped first
// (a SAVE of an existing server must reconnect with the NEW config, and its old
// tools must not linger if the new config never comes up), then a
// health/reconnect loop is started. The loop's first act on a server with no
// session is to connect, so tools appear via the sink as soon as the server
// answers — and a server that is down is retried with backoff rather than
// failed, exactly like a server that was unreachable at boot.
func (c *Connector) AddServer(ctx context.Context, s ServerConfig, sink ToolSink, interval time.Duration) {
	// A re-arm of a server that was running is a RESTART (reconfigure, a
	// credential change, a bounce); counted so the console can show it.
	wasRunning := c.dropServer(ctx, s.ID, sink)
	if wasRunning {
		c.bumpRestarts(s.ID)
	}
	c.armWatch(ctx, s, sink, interval)
}

// armWatch starts (or replaces) the health/reconnect goroutine for one server
// without touching its session or published tools.
func (c *Connector) armWatch(ctx context.Context, s ServerConfig, sink ToolSink, interval time.Duration) {
	wctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if prev, ok := c.watches[s.ID]; ok {
		prev()
	}
	c.watches[s.ID] = cancel
	c.gens[s.ID]++
	gen := c.gens[s.ID]
	c.configs[s.ID] = s
	c.mu.Unlock()
	if s.PerUser() {
		c.users.mu.Lock()
		if c.users.nudge[s.ID] == nil {
			c.users.nudge[s.ID] = make(chan struct{}, 1)
		}
		c.users.mu.Unlock()
		go c.watchUserServer(wctx, s, sink, interval, gen)
		return
	}
	go c.watchServer(wctx, s, sink, interval, gen)
}

// RemoveServer stops a server's loop, closes its session and withdraws its
// tools, and forgets its health. Removing an unknown id is a no-op — the
// post-condition already holds.
func (c *Connector) RemoveServer(ctx context.Context, id string, sink ToolSink) {
	c.StopServer(ctx, id, sink)
	c.forgetToolList(ctx, id)
	c.mu.Lock()
	delete(c.health, id)
	delete(c.gens, id)
	delete(c.configs, id)
	c.mu.Unlock()
}

// StopServer is RemoveServer that KEEPS the server's health record, reporting
// it "stopped": a disabled integration is still installed, and the console
// must show it as off rather than as unknown.
func (c *Connector) StopServer(ctx context.Context, id string, sink ToolSink) {
	c.dropServer(ctx, id, sink)
	gen := c.generation(id)
	c.setHealth(id, gen, func(h *ServerHealth) { h.State, h.ToolCount = HealthStopped, 0 })
}

// dropServer is the shared teardown half: cancel the watch, close the session,
// withdraw the tools. Ordered so the loop cannot re-publish tools between the
// withdrawal and its own cancellation. It reports whether anything was running.
func (c *Connector) dropServer(ctx context.Context, id string, sink ToolSink) bool {
	c.mu.Lock()
	cancel := c.watches[id]
	delete(c.watches, id)
	sess := c.sessions[id]
	delete(c.sessions, id)
	c.gens[id]++ // any loop still running now writes under a dead generation
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if sess != nil {
		_ = sess.Close()
	}
	// ADR-0140: a re-saved or removed server's per-principal sessions run the
	// OLD config; close them and forget that its tools were published.
	c.dropUsers(id, "")
	c.users.mu.Lock()
	published := c.users.published[id]
	delete(c.users.published, id)
	c.users.mu.Unlock()
	if sink != nil && (cancel != nil || sess != nil || published) {
		sink.RemoveServerTools(ctx, id)
	}
	return cancel != nil || sess != nil
}

// ProbeResult is what one ephemeral connection attempt learned (contract 0097).
type ProbeResult struct {
	OK        bool
	LatencyMs int64
	// ToolNames is what the server actually advertises — the fact nothing else
	// in the console can reveal before committing to a config.
	ToolNames []string
	Err       string
}

// Probe dials a server config once, lists its tools, and disconnects. It uses a
// throwaway client so a live session for the same id — possibly running on the
// OLD config the operator is about to replace — is never touched. The probe
// resolves credentials from c's sources, exactly as a real connect would.
func (c *Connector) Probe(ctx context.Context, s ServerConfig) ProbeResult {
	start := time.Now()
	tmp := newProbeConnector(c.creds)
	sess, secrets, err := tmp.connect(ctx, s)
	if err != nil {
		return ProbeResult{Err: redact(err.Error(), secrets), LatencyMs: time.Since(start).Milliseconds()}
	}
	defer func() { _ = sess.Close() }()

	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		return ProbeResult{Err: "connected, but listing tools failed: " + redact(err.Error(), secrets),
			LatencyMs: time.Since(start).Milliseconds()}
	}
	names := make([]string, 0, len(res.Tools))
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	return ProbeResult{OK: true, ToolNames: names, LatencyMs: time.Since(start).Milliseconds()}
}

func (c *Connector) watchServer(ctx context.Context, s ServerConfig, sink ToolSink, interval time.Duration, gen uint64) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	// dropped marks a reconnect caused by losing a live session, so the
	// successful reconnect after it counts as one restart (not one per backoff
	// attempt).
	dropped := false
	// silent counts consecutive unanswered pings (liveness.go: hysteresis
	// applies to silence only).
	silent := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if sess, ok := c.Session(s.ID); ok {
			// Healthy path: wait, then ping.
			if !sleepCtx(ctx, interval) {
				return
			}
			pctx, cancel := context.WithTimeout(ctx, c.pingTimeout())
			err := sess.Ping(pctx, pingParams(sess))
			verdict := classifyPing(pctx, err)
			cancel()
			if ctx.Err() != nil {
				return // the loop was stopped mid-ping; not a server failure
			}
			switch {
			case verdict == alive:
				if err != nil {
					// Answered with a JSON-RPC error (no ping support, strict
					// params): the round trip IS the liveness signal.
					slog.Debug("ADR-0043: MCP ping answered with an error — server is alive", "server", s.ID, "err", err)
				}
				silent = 0
				continue // still healthy
			case verdict == unresponsive && silent+1 < pingTimeoutsBeforeDrop:
				silent++
				slog.Warn("ADR-0043: MCP server did not answer a health ping — keeping it, will re-check",
					"server", s.ID, "missed", silent, "drop_after", pingTimeoutsBeforeDrop)
				continue
			}
			silent = 0
			slog.Warn("ADR-0043: MCP server unhealthy — dropping tools, will reconnect", "server", s.ID, "err", err)
			sink.RemoveServerTools(ctx, s.ID)
			c.mu.Lock()
			delete(c.sessions, s.ID)
			c.mu.Unlock()
			_ = sess.Close()
			c.setHealth(s.ID, gen, func(h *ServerHealth) {
				h.State, h.ToolCount, h.LastError = HealthError, 0, "health ping failed: "+err.Error()
			})
			dropped = true
			backoff = time.Second
			continue
		}
		// Reconnect path.
		c.setHealth(s.ID, gen, func(h *ServerHealth) {
			if h.State != HealthError { // keep the last error visible while retrying
				h.State = HealthStarting
			}
		})
		newSess, secrets, err := c.connect(ctx, s)
		if err != nil {
			if ctx.Err() == nil {
				msg := redact(err.Error(), secrets)
				c.setHealth(s.ID, gen, func(h *ServerHealth) { h.State, h.LastError, h.ToolCount = HealthError, msg, 0 })
			}
			if !sleepCtx(ctx, backoff) {
				return
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		c.mu.Lock()
		c.sessions[s.ID] = newSess
		c.mu.Unlock()
		tools, derr := c.boundedDiscover(ctx, s, newSess)
		if derr != nil {
			msg := redact(derr.Error(), secrets)
			slog.Warn("ADR-0043: MCP re-discovery failed", "server", s.ID, "err", msg)
			c.setHealth(s.ID, gen, func(h *ServerHealth) {
				h.State, h.LastError = HealthError, "connected, but listing tools failed: "+msg
			})
			continue
		}
		// A loop cancelled mid-discovery (its server was removed or re-saved)
		// must not publish: these would be ghost tools for a config that no
		// longer exists, registered after the teardown withdrew them.
		if ctx.Err() != nil {
			return
		}
		sink.SetServerTools(ctx, s.ID, tools)
		slog.Info("ADR-0043: MCP server (re)synced", "server", s.ID, "tools", len(tools))
		restarted := dropped
		dropped = false
		c.setHealth(s.ID, gen, func(h *ServerHealth) {
			h.State, h.LastError, h.ToolCount, h.LastHandshake = HealthReady, "", len(tools), time.Now()
			if restarted {
				h.Restarts++
			}
		})
		backoff = time.Second
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Close closes all live sessions.
func (c *Connector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, s := range c.sessions {
		_ = s.Close()
		delete(c.sessions, id)
	}
	c.closeAllUsers()
}

// discover lists a server's tools and maps them to namespaced SystemTools. The
// advertised metadata is descriptive only — policy is bound by operator config
// keyed by the identity (A1.5 / ADR-0043 D3).
func discover(ctx context.Context, s ServerConfig, sess *mcpsdk.ClientSession) ([]domain.SystemTool, error) {
	adv, err := listAdvertised(ctx, sess)
	if err != nil {
		return nil, err
	}
	return toolsFor(s, adv), nil
}

// advertisedTool is what a server says about one tool: the only part of a
// discovered tool worth persisting (toollists.go). Policy is re-applied from
// the CURRENT config every time (toolsFor), never stored.
type advertisedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

func listAdvertised(ctx context.Context, sess *mcpsdk.ClientSession) ([]advertisedTool, error) {
	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]advertisedTool, 0, len(res.Tools))
	for _, t := range res.Tools {
		var schema json.RawMessage
		if t.InputSchema != nil {
			if b, merr := json.Marshal(t.InputSchema); merr == nil {
				schema = b
			}
		}
		out = append(out, advertisedTool{Name: t.Name, Description: t.Description, Schema: schema})
	}
	return out, nil
}

// toolsFor turns a server's advertised tools into namespaced, policy-applied
// SystemTools.
func toolsFor(s ServerConfig, adv []advertisedTool) []domain.SystemTool {
	out := make([]domain.SystemTool, 0, len(adv))
	for _, t := range adv {
		st := domain.SystemTool{
			Name:        ToolNamePrefix + s.ID + "/" + t.Name,
			Description: t.Description,
			Schema:      t.Schema,
		}
		// Apply operator policy (dangerous-flag + effects + data-egress kinds + domain
		// tags) — never the server's own advertised metadata (A1.5 / ADR-0043 D4/D9).
		// An unlisted tool on a server with no default policy fails closed.
		applyPolicy(&st, policyFor(t.Name, s.Tools, s.DefaultPolicy), s.DefaultClassificationTags)
		// The fail-closed default bounds authorization but says nothing about what
		// the tool does: effect-checked dispatch must not read it as a capability.
		st.EffectsDefaulted = !policyListed(t.Name, s.Tools, s.DefaultPolicy)
		out = append(out, st)
	}
	return out
}

// SlogEgressAuditor is the default EgressAuditor — it logs each remote-tool data
// egress (ADR-0043 D4). Operators can replace it with a structured sink.
type SlogEgressAuditor struct{}

// RecordEgress implements domain.EgressAuditor.
func (SlogEgressAuditor) RecordEgress(agentID, toolName string, dataClasses []string) {
	slog.Info("ADR-0043: MCP data egress",
		"agent_id", agentID, "tool", toolName, "data_classes", dataClasses)
}

// authRoundTripper injects a static credential on every request to a server
// (ADR-0043 D9 static auth). The credential is bound to this server's transport
// and never reaches another.
type authRoundTripper struct {
	base     http.RoundTripper
	authType string
	header   string
	token    string
}

func (a *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	switch a.authType {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+a.token)
	case "header":
		if a.header != "" {
			req.Header.Set(a.header, a.token)
		}
	}
	return a.base.RoundTrip(req)
}
