package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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
// kernel capability bundles they extend (e.g. KernelServices). ADR-0057 established the
// interface seam; ADR-0074 generalizes it from a fixed set of Option hooks into an
// open registry so N independent plugins can contribute without the OSS core naming them.
type Plugin interface {
	// Manifest is the plugin's self-description (ADR-0082 D1). Manifest().ID is the
	// canonical identity used for logging, error attribution, dependency resolution, and
	// (once licensing lands) entitlement. It replaces the former Name() method so a
	// plugin has exactly one source of identity.
	Manifest() PluginManifest
	// Register declares the plugin's contributions on the Registry. Called once during
	// boot, before the kernel consumes any extension point.
	Register(*Registry) error
}

// PluginManifest is a plugin's self-description (ADR-0082 D1). It is deliberately DATA
// rather than Go type identity: the same description serves an in-process plugin today and
// an out-of-process one later, so the manifest/entitlement layer survives that transition
// instead of being rewritten by it.
type PluginManifest struct {
	// ID is the stable plugin key ("reactive"). It is the entitlement key and the
	// dependency-graph node name — never change it for a shipped plugin.
	ID string
	// DisplayName is the human label for operator surfaces ("Reactive Engine").
	DisplayName string
	// Version is the plugin's OWN version line — unrelated to the operator contract
	// version, which describes the kernel's proto surface.
	Version string
	// Requires lists plugin IDs this plugin is built on (ADR-0082 D10). The composition
	// root registers dependencies first and reports unmet ones rather than failing the
	// boot: with subscriptions an unmet dependency is a billing combination, and a paying
	// customer must never get a kernel that refuses to start.
	Requires []string
	// Capabilities are the operator capability strings this plugin advertises. The kernel
	// folds them into the handshake WITHOUT knowing what any of them mean (ADR-0082 D2) —
	// this is what keeps premium vocabulary out of the OSS core.
	Capabilities []string
	// Panels describes the operator surfaces this plugin contributes. Carried so a UI can
	// enumerate them; today panels are capability-gated with UI-side components, so this is
	// descriptive metadata that makes descriptor-driven rendering an additive future step.
	Panels []PanelSpec
}

// PanelSpec describes one operator surface a plugin contributes (ADR-0082 D1/D9).
type PanelSpec struct {
	// ID is the stable panel key the UI keys its component off ("watches").
	ID string
	// Title is the human label ("Watches").
	Title string
	// Capability is the capability string that must be live for this panel to render.
	Capability string
}

// Builder is the OPTIONAL second plugin phase (ADR-0082 D12): construct runtime objects
// from the kernel capability bundle. The composition root type-asserts for it, so a plugin
// with nothing to construct implements only Register.
//
// The three phases are Register (declare — no kernel exists) → Build (construct — stacks
// built, nothing running) → Lifecycle Start/Stop (run). Build exists to kill the
// pointer-capture pattern, where a plugin built its engine inside a hook and stashed it for
// a later hook to find, relying on undocumented ordering between the two.
type Builder interface {
	Build(KernelServices) error
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
	signalReceiver   func(KernelServices) (domain.SignalReceiver, domain.WatchConfigHandler)
	signalOwner      string
	grpcServices     []func(*grpc.Server)
	lifecycles       []Lifecycle
	resourceSelector domain.ResourceSelector
	selectorOwner    string
	agentSources     []AgentSource
	mcpServers       []MCPServerSpec
	capabilities     []string
	authorizer       domain.Authorizer
	policyAdmin      domain.PolicyAdmin
	authzOwner       string
	ingressResolver  domain.IngressResolver
	identityResolver domain.IdentityResolver
	identityOwner    string
	ingressOwner     string
}

// SetAuthorizer installs the access-control decision point (ADR-0085). Tier-1
// replace-one: at most one plugin may own the decision; a second registration is
// an error, because two decision points would mean two answers to the same
// question and no way to say which held.
//
// The admin surface travels with it: whoever decides also administers. Passing a
// nil admin is allowed (a decision point with no authoring UI).
func (r *Registry) SetAuthorizer(owner string, a domain.Authorizer, admin domain.PolicyAdmin) error {
	if r.authorizer != nil {
		return fmt.Errorf("authorizer already registered by plugin %q; %q cannot also own it", r.authzOwner, owner)
	}
	r.authorizer = a
	r.policyAdmin = admin
	r.authzOwner = owner
	return nil
}

// SetIngressResolver installs the registry that says which principals are entry
// points into Cambrian (ADR-0090 D2). Tier-1 replace-one, for the same reason the
// authorizer is: two registries could disagree about what a daemon is permitted
// to be, and there would be no way to say which answer held.
//
// A resolver may be registered before it has its backing store — the composition
// root hands plugins their database only at Build. Register the value, populate
// it later; that is why this takes an interface rather than a constructor.
func (r *Registry) SetIngressResolver(owner string, res domain.IngressResolver) error {
	if r.ingressResolver != nil {
		return fmt.Errorf("ingress resolver already registered by plugin %q; %q cannot also own it", r.ingressOwner, owner)
	}
	r.ingressResolver = res
	r.ingressOwner = owner
	return nil
}

// SetIdentityResolver installs the registry that answers "who is this external
// sender?" on the inbound path (contract 0077). Tier-1 replace-one, for the same
// reason the ingress resolver is: two registries could disagree about who an
// external id maps to, and a binding decides REACH — there would be no way to
// say which answer held, and the safe-looking one is not necessarily the one
// that ran.
//
// Leaving it unset is valid and means the SURFACE IS THE IDENTITY: every sender
// who finds the entry point has identical reach, because nobody is
// distinguished. That is the pre-0077 behaviour and it is named rather than
// treated as a neutral default.
func (r *Registry) SetIdentityResolver(owner string, res domain.IdentityResolver) error {
	if r.identityResolver != nil {
		return fmt.Errorf("identity resolver already registered by plugin %q; %q cannot also own it", r.identityOwner, owner)
	}
	r.identityResolver = res
	r.identityOwner = owner
	return nil
}

// AddCapability advertises an operator capability string beyond those declared statically
// in the manifest — for surfaces a plugin can only decide at registration time. Manifest
// capabilities are the norm; this is the escape hatch. Tier-2 add-many; deduped by the
// composition root. ADR-0082 D2.
func (r *Registry) AddCapability(caps ...string) {
	for _, c := range caps {
		if c != "" {
			r.capabilities = append(r.capabilities, c)
		}
	}
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
func (r *Registry) SetSignalReceiver(owner string, f func(KernelServices) (domain.SignalReceiver, domain.WatchConfigHandler)) error {
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
	// capabilities are the operator capability strings contributed by entitled plugins,
	// deduped and order-stable. The kernel appends these to its own base set at handshake
	// time without interpreting any of them (ADR-0082 D2).
	capabilities []string
	// statuses records every declared plugin and whether it actually registered — the
	// data behind the future ListPlugins RPC (ADR-0082 D9).
	statuses []PluginStatus
	// built is the ordered set of plugins that registered, retained so the composition
	// root can drive their optional Build phase once the kernel stacks exist (D12).
	built []Plugin
}

// PluginStatus pairs a plugin's declaration with what actually happened to it at boot.
// Declaration (PluginManifest) and runtime state are kept separate on purpose: the manifest
// is immutable data the plugin ships, the status is this deployment's outcome.
type PluginStatus struct {
	Manifest PluginManifest
	// State is one of the PluginState* constants.
	State string
	// Missing lists required plugin IDs unavailable in this deployment, when State is
	// "deps_unmet". A dependency counts as unavailable whether it was never built or was
	// merely not entitled — the two are deliberately indistinguishable here.
	Missing []string
	// Reason is operator-facing detail from the entitlement provider, when it declined or
	// flagged the plugin.
	Reason string
	// ExpiresAt, when set, is the entitlement expiry — surfaced so a UI can warn ahead.
	ExpiresAt *time.Time
}

// Plugin states surfaced to operators (ADR-0082 D9). A UI renders each differently:
// active ⇒ the plugin's panels; not_entitled ⇒ a locked panel with an upgrade path;
// expired ⇒ still working inside grace, with a renewal prompt; deps_unmet ⇒ "requires X".
const (
	PluginStateActive      = "active"
	PluginStateDepsUnmet   = "deps_unmet"
	PluginStateNotEntitled = "not_entitled"
	PluginStateExpired     = "expired"
)

// orderPlugins returns the plugins in dependency order (dependencies first) along with the
// status of each. A plugin whose Requires are not all present is SKIPPED, not fatal
// (ADR-0082 D10): with subscriptions an unmet dependency is a billing combination, and a
// paying customer must never get a kernel that refuses to boot because of one. Skipping
// cascades — a plugin depending on a skipped plugin is itself unmet.
//
// Cycles are a packaging bug, not a customer state, so they are reported as an error.
func orderPlugins(plugins []Plugin) ([]Plugin, []PluginStatus, error) {
	present := make(map[string]Plugin, len(plugins))
	order := make([]string, 0, len(plugins))
	for _, p := range plugins {
		if p == nil {
			continue
		}
		id := p.Manifest().ID
		if id == "" {
			return nil, nil, fmt.Errorf("plugin with empty Manifest().ID; every plugin needs a stable id")
		}
		if _, dup := present[id]; dup {
			return nil, nil, fmt.Errorf("duplicate plugin id %q", id)
		}
		present[id] = p
		order = append(order, id)
	}

	// Resolve which ids can actually activate: iterate to a fixed point so that skipping
	// cascades transitively through dependents.
	ok := make(map[string]bool, len(present))
	for id := range present {
		ok[id] = true
	}
	for changed := true; changed; {
		changed = false
		for id, p := range present {
			if !ok[id] {
				continue
			}
			for _, dep := range p.Manifest().Requires {
				if !ok[dep] {
					ok[id] = false
					changed = true
					break
				}
			}
		}
	}

	// Kahn over the activatable subset, seeded in declaration order for determinism.
	var (
		sorted   []Plugin
		statuses []PluginStatus
		done     = make(map[string]bool, len(present))
	)
	for progress := true; progress; {
		progress = false
		for _, id := range order {
			if done[id] || !ok[id] {
				continue
			}
			ready := true
			for _, dep := range present[id].Manifest().Requires {
				if !done[dep] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			done[id] = true
			sorted = append(sorted, present[id])
			progress = true
		}
	}

	for _, id := range order {
		p := present[id]
		if done[id] {
			statuses = append(statuses, PluginStatus{Manifest: p.Manifest(), State: PluginStateActive})
			continue
		}
		if ok[id] {
			// Activatable but never became ready ⇒ a dependency cycle.
			return nil, nil, fmt.Errorf("plugin dependency cycle involving %q", id)
		}
		var missing []string
		for _, dep := range p.Manifest().Requires {
			if _, exists := present[dep]; !exists {
				missing = append(missing, dep)
			} else if !ok[dep] {
				missing = append(missing, dep)
			}
		}
		statuses = append(statuses, PluginStatus{Manifest: p.Manifest(), State: PluginStateDepsUnmet, Missing: missing})
	}
	return sorted, statuses, nil
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
	// ADR-0082 D3: the entitlement chokepoint. Evaluated BEFORE dependency resolution, so
	// a plugin depending on an unentitled one correctly reports deps_unmet — "not paid for"
	// and "not present" are deliberately indistinguishable to everything downstream.
	var (
		entitled    []Plugin
		declined    []PluginStatus
		entitlement = make(map[string]Entitlement, len(opts.Plugins))
	)
	for _, p := range opts.Plugins {
		if p == nil {
			continue
		}
		m := p.Manifest()
		e := resolveEntitlement(opts.Entitlements, m)
		entitlement[m.ID] = e
		if !e.Allowed {
			declined = append(declined, PluginStatus{
				Manifest: m, State: e.State, Reason: e.Reason, ExpiresAt: e.ExpiresAt,
			})
			slog.Info("ADR-0082: plugin not activated",
				"plugin", m.ID, "state", e.State, "reason", e.Reason)
			continue
		}
		entitled = append(entitled, p)
	}

	ordered, statuses, err := orderPlugins(entitled)
	if err != nil {
		return composedPlugins{opts: opts}, err
	}
	// Carry entitlement detail onto the resolved statuses — a plugin can be active yet
	// EXPIRED (running inside its grace period), which operators must be able to see.
	for i := range statuses {
		e := entitlement[statuses[i].Manifest.ID]
		if statuses[i].State == PluginStateActive && e.State == PluginStateExpired {
			statuses[i].State = PluginStateExpired
		}
		if statuses[i].Reason == "" {
			statuses[i].Reason = e.Reason
		}
		statuses[i].ExpiresAt = e.ExpiresAt
	}
	statuses = append(statuses, declined...)
	for _, st := range statuses {
		if st.State == PluginStateDepsUnmet {
			slog.Warn("ADR-0082: plugin skipped — unmet dependencies",
				"plugin", st.Manifest.ID, "missing", st.Missing)
		}
	}

	reg := &Registry{}
	for _, p := range ordered {
		m := p.Manifest()
		if err := p.Register(reg); err != nil {
			return composedPlugins{opts: opts}, fmt.Errorf("plugin %q register: %w", m.ID, err)
		}
		reg.AddCapability(m.Capabilities...)
		slog.Info("ADR-0074: plugin registered",
			"plugin", m.ID, "version", m.Version, "capabilities", len(m.Capabilities))
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
	// Authorizer + PolicyAdmin: plugin wins only if not set directly.
	if opts.Authorizer == nil && reg.authorizer != nil {
		opts.Authorizer = reg.authorizer
	}
	if opts.PolicyAdmin == nil && reg.policyAdmin != nil {
		opts.PolicyAdmin = reg.policyAdmin
	}
	if opts.IdentityResolver == nil && reg.identityResolver != nil {
		opts.IdentityResolver = reg.identityResolver
	}
	if opts.IngressResolver == nil && reg.ingressResolver != nil {
		opts.IngressResolver = reg.ingressResolver
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
	// Capabilities: dedupe, preserving first-seen order so the handshake list is stable
	// across boots (a UI diffing capabilities should not see spurious churn).
	var caps []string
	seen := make(map[string]bool, len(reg.capabilities))
	for _, c := range reg.capabilities {
		if !seen[c] {
			seen[c] = true
			caps = append(caps, c)
		}
	}

	return composedPlugins{
		opts:         opts,
		lifecycles:   reg.lifecycles,
		agentSources: reg.agentSources,
		mcpServers:   reg.mcpServers,
		capabilities: caps,
		statuses:     statuses,
		built:        ordered,
	}, nil
}

// buildPlugins drives the optional Build phase (ADR-0082 D12) in dependency order, after
// the kernel stacks exist and before gRPC registration + handshake — so a plugin's services
// and capabilities may depend on what Build constructed. Plugins that do not implement
// Builder are skipped.
func buildPlugins(plugins []Plugin, svc KernelServices) error {
	for _, p := range plugins {
		b, ok := p.(Builder)
		if !ok {
			continue
		}
		if err := b.Build(svc); err != nil {
			return fmt.Errorf("plugin %q build: %w", p.Manifest().ID, err)
		}
		slog.Info("ADR-0082: plugin built", "plugin", p.Manifest().ID)
	}
	return nil
}
