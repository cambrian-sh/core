package app

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"

	"github.com/cambrian-sh/core/domain"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// Plugin is a compile-time kernel extension unit (ADR-0074). A downstream module
// implements Plugin and, in Register, declares its contributions to the kernel's
// extension points through the Registry. A thin distribution binary composes the set of
// plugins it wants and passes them via Options.Plugins, then calls Run.
//
// This is deliberately NOT dynamic loading: Go's `plugin` package requires CGO, does not
// work on Windows, and version-locks host and plugin to identical toolchains + deps.
// Plugins are compiled into a distribution binary instead — type-safe, cross-platform,
// no CGO, and (unlike an out-of-process gRPC plugin) they keep in-process access to the
// kernel capability bundles they extend (e.g. ReactiveServices). ADR-0057 established the
// interface seam; ADR-0074 generalizes it from a fixed set of Option hooks into an
// open registry so N independent plugins can contribute without the OSS core naming them.
type Plugin interface {
	// Name identifies the plugin (logging, diagnostics, error attribution).
	Name() string
	// Register declares the plugin's contributions on the Registry. Called once during
	// boot, before the kernel consumes any extension point.
	Register(*Registry) error
}

// Lifecycle is a background component a plugin needs started at boot and drained on
// shutdown (e.g. the reactive engine's worker pools + REACT-06 scheduler). Start is
// non-blocking (it launches goroutines and returns); Stop drains them.
type Lifecycle struct {
	Name  string
	Start func(context.Context)
	Stop  func()
}

// DiscoveredAgent is one agent an AgentSource contributes: its definition plus an
// OPTIONAL manifest (ADR-0075). Carrying the manifest lets a source persist the extras
// (PythonDeps/MemoryLimitMB/schemas) a plain definition drops — so a source can fully
// replace the built-in filesystem scan, not just approximate it. A nil Manifest degrades
// to a record-only registration (the model-agent path).
type DiscoveredAgent struct {
	Definition domain.AgentDefinition
	Manifest   *domain.AgentManifest
}

// AgentSource discovers agents to register at boot (ADR-0075). It is the unifying seam
// over the ways agents enter the registry — the built-in filesystem scan is itself a
// FilesystemAgentSource, model-config is another, and a plugin contributes more via
// Registry.AddAgentSource / AddAgent (regular, Tier-2 add-many) or AddSystemAgent
// (privileged, an explicit + logged grant). The composition root discovers each source
// and registers its agents (with manifests) uniformly.
type AgentSource interface {
	// Name identifies the source (logging / attribution).
	Name() string
	// DiscoverAgents returns the agents this source contributes.
	DiscoverAgents(context.Context) ([]DiscoveredAgent, error)
}

// MCPServerSpec is the plugin-facing, boundary-safe description of an external MCP tool
// server (ADR-0075 / ADR-0043). It deliberately mirrors only the fields a plugin needs
// so plugins never import a core-internal `mcp.ServerConfig` — the composition root maps
// it. A plugin contributes one via Registry.AddMCPServer (Tier-2 add-many); its tools are
// connected + registered as `mcp:<id>/<tool>` alongside the config-declared servers.
type MCPServerSpec struct {
	ID           string
	Transport    string // "stdio" | "streamable-http"
	Endpoint     string // command (stdio) or URL (http)
	Args         []string
	AuthType     string
	AuthHeader   string
	AuthTokenEnv string
	Tools        []MCPToolSpec
}

// MCPToolSpec is a per-tool policy carried on an MCPServerSpec.
type MCPToolSpec struct {
	Name           string
	Dangerous      bool
	DataWriteKinds []string
}

// staticAgentSource wraps a fixed set of definitions (Registry.AddAgent/AddSystemAgent).
type staticAgentSource struct {
	name string
	defs []domain.AgentDefinition
}

func (s staticAgentSource) Name() string { return s.name }
func (s staticAgentSource) DiscoverAgents(context.Context) ([]DiscoveredAgent, error) {
	out := make([]DiscoveredAgent, len(s.defs))
	for i, d := range s.defs {
		out[i] = DiscoveredAgent{Definition: d}
	}
	return out, nil
}

// Registry collects plugin contributions to the kernel's extension points. A plugin
// mutates it in Register; the composition root folds the result into the effective
// Options + lifecycle set. Not safe for concurrent use — Register is called serially.
type Registry struct {
	traceWrappers    []func(domain.Generator, string) domain.Generator
	agentCallLogger  subnetwork.AgentCallLogger
	signalReceiver   func(ReactiveServices) (domain.SignalReceiver, domain.WatchConfigHandler)
	signalOwner      string
	grpcServices     []func(*grpc.Server)
	lifecycles       []Lifecycle
	resourceSelector domain.ResourceSelector
	selectorOwner    string
	agentSources     []AgentSource
	mcpServers       []MCPServerSpec
}

// AddTraceWrapper contributes a generator trace wrapper (composed over any others).
func (r *Registry) AddTraceWrapper(f func(domain.Generator, string) domain.Generator) {
	if f != nil {
		r.traceWrappers = append(r.traceWrappers, f)
	}
}

// SetAgentCallLogger sets the agent-call logger (last writer wins).
func (r *Registry) SetAgentCallLogger(l subnetwork.AgentCallLogger) { r.agentCallLogger = l }

// SetSignalReceiver installs the reactive signal-receiver factory. It is a singleton —
// exactly one plugin may own the reactive lane; a second registration is an error.
func (r *Registry) SetSignalReceiver(owner string, f func(ReactiveServices) (domain.SignalReceiver, domain.WatchConfigHandler)) error {
	if r.signalReceiver != nil {
		return fmt.Errorf("signal receiver already registered by plugin %q; %q cannot also own it", r.signalOwner, owner)
	}
	r.signalReceiver = f
	r.signalOwner = owner
	return nil
}

// AddGRPCService contributes an extra gRPC service registrar, mounted on the kernel
// server (behind the operator auth interceptors) before Serve. ADR-0073.
func (r *Registry) AddGRPCService(f func(*grpc.Server)) {
	if f != nil {
		r.grpcServices = append(r.grpcServices, f)
	}
}

// AddLifecycle registers a background component to Start at boot and Stop on shutdown.
func (r *Registry) AddLifecycle(l Lifecycle) { r.lifecycles = append(r.lifecycles, l) }

// SetResourceSelector installs the routing ResourceSelector (ADR-0037), the arm that
// picks which agent handles an intent from the offered candidates. Tier-1 replace-one
// (ADR-0074): at most one plugin may own it; a second registration is an error. A
// plugin-provided selector overrides the config-driven (auction/EFE) default. This is a
// selection *mechanism* — the Zero-Hardcode routing *policy* (merit-based, not authored)
// still holds; the selector receives candidates and ranks them, it does not hardcode
// agent identities.
func (r *Registry) SetResourceSelector(owner string, sel domain.ResourceSelector) error {
	if r.resourceSelector != nil {
		return fmt.Errorf("resource selector already registered by plugin %q; %q cannot also own it", r.selectorOwner, owner)
	}
	r.resourceSelector = sel
	r.selectorOwner = owner
	return nil
}

// AddAgentSource contributes an agent discovery source (ADR-0075). Tier-2 add-many:
// its definitions are registered alongside the built-in filesystem + model sources.
func (r *Registry) AddAgentSource(src AgentSource) {
	if src != nil {
		r.agentSources = append(r.agentSources, src)
	}
}

// AddAgent contributes a single REGULAR agent definition. System privilege is forced
// off here — a regular-agent registration can never confer system status (that must go
// through AddSystemAgent). Tier-2 add-many.
func (r *Registry) AddAgent(def domain.AgentDefinition) {
	def.System = false
	r.agentSources = append(r.agentSources, staticAgentSource{name: "agent:" + def.ID, defs: []domain.AgentDefinition{def}})
}

// AddMCPServer contributes an external MCP tool server (ADR-0075 / ADR-0043). Tier-2
// add-many: connected + tool-registered alongside the config-declared servers.
func (r *Registry) AddMCPServer(spec MCPServerSpec) {
	if spec.ID != "" {
		r.mcpServers = append(r.mcpServers, spec)
	}
}

// AddSystemAgent contributes a PRIVILEGED system agent (bypasses auction/Gatekeeper by
// construction). System status is a policy decision, so this is an EXPLICIT, auditable
// grant: it stamps System=true and the composition root logs the grant at registration.
// Only a compiled-in (trusted) plugin can reach this; an untrusted plugin must never get
// system status. ADR-0074 Tier-3 boundary made visible.
func (r *Registry) AddSystemAgent(def domain.AgentDefinition) {
	def.System = true
	r.agentSources = append(r.agentSources, staticAgentSource{name: "system-agent:" + def.ID, defs: []domain.AgentDefinition{def}})
}

// composedPlugins is the resolved output of applyPlugins: the merged Options plus the
// contributions the composition root consumes at specific points (lifecycles at
// boot/shutdown, agent sources at registry-seed time). A struct keeps the seam
// extensible as more Tier-2 add-many points arrive (MCP sources, etc.).
type composedPlugins struct {
	opts         Options
	lifecycles   []Lifecycle
	agentSources []AgentSource
	mcpServers   []MCPServerSpec
}

// applyPlugins runs every plugin's Register and folds the collected contributions into
// the effective Options (composing with any directly-set fields). Direct Options fields
// and plugin contributions coexist: e.g. premium may set TraceWrapper directly (Langfuse)
// while a reactive plugin contributes the signal receiver + control service + lifecycle.
// ADR-0074 / ADR-0075.
func applyPlugins(opts Options) (composedPlugins, error) {
	if len(opts.Plugins) == 0 {
		return composedPlugins{opts: opts}, nil
	}
	reg := &Registry{}
	for _, p := range opts.Plugins {
		if p == nil {
			continue
		}
		if err := p.Register(reg); err != nil {
			return composedPlugins{opts: opts}, fmt.Errorf("plugin %q register: %w", p.Name(), err)
		}
		slog.Info("ADR-0074: plugin registered", "name", p.Name())
	}

	// TraceWrapper: chain registered wrappers over any directly-set one.
	if len(reg.traceWrappers) > 0 {
		base := opts.TraceWrapper
		wrappers := reg.traceWrappers
		opts.TraceWrapper = func(g domain.Generator, sub string) domain.Generator {
			if base != nil {
				g = base(g, sub)
			}
			for _, w := range wrappers {
				g = w(g, sub)
			}
			return g
		}
	}
	// AgentCallLogger: plugin wins only if not set directly.
	if opts.AgentCallLogger == nil && reg.agentCallLogger != nil {
		opts.AgentCallLogger = reg.agentCallLogger
	}
	// NewSignalReceiver: plugin wins only if not set directly.
	if opts.NewSignalReceiver == nil && reg.signalReceiver != nil {
		opts.NewSignalReceiver = reg.signalReceiver
	}
	// ResourceSelector: plugin wins only if not set directly (ADR-0074 replace-one).
	if opts.ResourceSelector == nil && reg.resourceSelector != nil {
		opts.ResourceSelector = reg.resourceSelector
	}
	// ExtraServices: compose every registered gRPC service with any directly-set one.
	if len(reg.grpcServices) > 0 {
		base := opts.ExtraServices
		services := reg.grpcServices
		opts.ExtraServices = func(s *grpc.Server) {
			if base != nil {
				base(s)
			}
			for _, reg := range services {
				reg(s)
			}
		}
	}
	return composedPlugins{opts: opts, lifecycles: reg.lifecycles, agentSources: reg.agentSources, mcpServers: reg.mcpServers}, nil
}
