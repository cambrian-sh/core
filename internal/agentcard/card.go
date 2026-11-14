// Package agentcard builds, signs, verifies and publishes A2A 1.0 Agent Cards
// for the kernel's registered agents (ADR-0139 C2/D4; A2A specification §4.4
// AgentCard and §8.4 Agent Card Signing).
//
// A card is DERIVED, never declared a second time: it is built from the agent's
// registered definition and manifest (domain.AgentDefinition +
// domain.AgentManifest), which for a profile agent the kernel built from
// agents/profiles/<name>.yaml (internal/storage/profile_discovery.go). ADR-0139's
// Phase-2 observation is the rule: "a D4 card built later should read the
// profile (or the registered manifest/definition), not a second declaration".
//
// The kernel signs each card with its own Ed25519 key: a detached JWS (RFC 7515,
// alg EdDSA) over the RFC 8785 canonical form of the card without its
// `signatures` member, which is what A2A §8.4 specifies. ADR-0139 says cards are
// signed "at qualification"; D5 qualification does not exist yet, so cards are
// signed when they are served (the signature attests "this kernel registered this
// agent with these declarations", not "this agent passed a probe suite").
//
// Remote cards (agents other deployments publish) are verified against a
// configured key set before registration (sign.go Verify, remote.go RemoteFetcher).
package agentcard

import (
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// ProtocolVersion is the A2A protocol version the cards declare (Major.Minor;
// the spec says patch numbers SHOULD NOT appear in cards).
const ProtocolVersion = "1.0"

// WellKnownPath is the A2A discovery path, relative to an agent's base URL
// (A2A §8.2). The pre-0.3 path was /.well-known/agent.json; RemoteFetcher's
// inner client falls back to it for older servers.
const WellKnownPath = "/.well-known/agent-card.json"

// ExtensionURI identifies the Cambrian metadata extension every kernel card
// carries in capabilities.extensions. A URN, not a URL: nothing is served there.
const ExtensionURI = "urn:cambrian:a2a:ext:agent:v1"

// Trust tiers (ADR-0139 D8). The kernel stamps them; an agent never self-asserts one.
const (
	TierSystem      = "system"
	TierFirstParty  = "first-party"
	TierThirdParty  = "third-party"
	bearerSchemeKey = "cambrian_bearer"
)

// Card is an A2A 1.0 AgentCard (§4.4.1). Optional members carry omitempty and
// the builder never emits a default value (false, "", [] on an optional
// member), because §8.4.1 has signers and verifiers drop default-valued
// members before canonicalising — a card that emitted one would verify only
// against a verifier that happened to keep it.
type Card struct {
	Name                 string                    `json:"name"`
	Description          string                    `json:"description"`
	SupportedInterfaces  []Interface               `json:"supportedInterfaces"`
	Provider             *Provider                 `json:"provider,omitempty"`
	Version              string                    `json:"version"`
	DocumentationURL     string                    `json:"documentationUrl,omitempty"`
	Capabilities         Capabilities              `json:"capabilities"`
	SecuritySchemes      map[string]SecurityScheme `json:"securitySchemes,omitempty"`
	SecurityRequirements []SecurityRequirement     `json:"securityRequirements,omitempty"`
	DefaultInputModes    []string                  `json:"defaultInputModes"`
	DefaultOutputModes   []string                  `json:"defaultOutputModes"`
	Skills               []Skill                   `json:"skills"`
	Signatures           []Signature               `json:"signatures,omitempty"`
}

// Interface is an AgentInterface (§4.4.6).
type Interface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
	Tenant          string `json:"tenant,omitempty"`
}

// Provider is an AgentProvider (§4.4.2); both members are required by the spec.
type Provider struct {
	Organization string `json:"organization"`
	URL          string `json:"url"`
}

// Capabilities is AgentCapabilities (§4.4.3).
type Capabilities struct {
	Streaming         bool        `json:"streaming,omitempty"`
	PushNotifications bool        `json:"pushNotifications,omitempty"`
	ExtendedAgentCard bool        `json:"extendedAgentCard,omitempty"`
	Extensions        []Extension `json:"extensions,omitempty"`
}

// Extension is an AgentExtension (§4.4.4).
type Extension struct {
	URI         string         `json:"uri,omitempty"`
	Description string         `json:"description,omitempty"`
	Required    bool           `json:"required,omitempty"`
	Params      map[string]any `json:"params,omitempty"`
}

// Skill is an AgentSkill (§4.4.5).
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// SecurityScheme is the §4.5.1 union; the kernel only ever declares HTTP auth.
type SecurityScheme struct {
	HTTPAuth *HTTPAuthScheme `json:"httpAuthSecurityScheme,omitempty"`
}

// HTTPAuthScheme is HTTPAuthSecurityScheme.
type HTTPAuthScheme struct {
	Scheme       string `json:"scheme"`
	BearerFormat string `json:"bearerFormat,omitempty"`
	Description  string `json:"description,omitempty"`
}

// SecurityRequirement maps a scheme name to its required scopes.
type SecurityRequirement struct {
	Schemes map[string]ScopeList `json:"schemes"`
}

// ScopeList is the scope list of one requirement entry. Bearer auth has no
// scopes, and an empty list is a default value, so it is omitted (see Card).
type ScopeList struct {
	List []string `json:"list,omitempty"`
}

// Signature is an AgentCardSignature (§4.4.7): a JWS in its flattened
// detached form — protected header and signature, the payload being the card.
type Signature struct {
	Protected string         `json:"protected"`
	Signature string         `json:"signature"`
	Header    map[string]any `json:"header,omitempty"`
}

// InterfaceTemplate is one configured public interface. URLTemplate may hold
// the token {agent_id}.
type InterfaceTemplate struct {
	URLTemplate     string
	ProtocolBinding string
}

// BuildOptions are the deployment facts a card states that no agent declares.
type BuildOptions struct {
	// Provider, when Organization and URL are both set, becomes card.provider.
	Provider Provider
	// DocumentationURL, optional.
	DocumentationURL string
	// Interfaces are the public A2A interfaces a deployment fronts its agents
	// with. Empty is honest and the default: the kernel itself does not serve
	// the A2A task protocol, so a card with no configured interface lists none.
	Interfaces []InterfaceTemplate
}

// Eligible reports whether a registered agent gets a kernel card, and why not.
//
//   - model-trait agents are LLM generators, not task agents;
//   - remote A2A agents publish their own card: re-signing it would be the
//     kernel vouching for declarations it did not make;
//   - system agents are kernel organs, never assigned a user task, so they are
//     published only when the deployment asks (includeSystem).
func Eligible(def domain.AgentDefinition, includeSystem bool) (bool, string) {
	switch {
	case def.Trait == domain.TraitModel:
		return false, "model agent"
	case def.Runtime == domain.RuntimeA2A:
		return false, "remote A2A agent (its own card is authoritative)"
	case domain.IsSystemDefinition(def) && !includeSystem:
		return false, "system agent"
	}
	return true, ""
}

// TrustTier is the tier a card states for a registered agent: the kernel's
// view, never the agent's claim. A profile's declared tier is kept except that
// "system" is granted only by the composition root (domain.IsSystemDefinition),
// exactly as discovery logs it; a local agent with no declared tier is
// first-party (it runs from the deployment's own agents directory); a remote
// A2A agent is always third-party.
func TrustTier(def domain.AgentDefinition) string {
	switch {
	case def.Runtime == domain.RuntimeA2A:
		return TierThirdParty
	case domain.IsSystemDefinition(def):
		return TierSystem
	case def.TrustTier == TierSystem:
		return TierFirstParty
	case def.TrustTier != "":
		return def.TrustTier
	}
	return TierFirstParty
}

// Build derives an unsigned card from one registered agent. man may be nil.
func Build(def domain.AgentDefinition, man *domain.AgentManifest, opt BuildOptions) Card {
	if man == nil {
		man = &domain.AgentManifest{}
	}
	name := def.Name
	if strings.TrimSpace(name) == "" {
		name = def.ID
	}
	modes := mediaTypes(man.SupportedFormats)

	caps := uniqueSorted(nonEmpty(man.Capabilities, def.Capabilities))
	examples := nonEmptyStrings(man.Examples)
	skills := make([]Skill, 0, len(caps))
	for _, c := range caps {
		skills = append(skills, Skill{
			ID:   c,
			Name: humanize(c),
			// The profile has no per-capability text; its description is the
			// routing text the kernel embeds for this agent (ProfileSeeder), so
			// the card says what the router reads.
			Description: def.Description,
			Tags:        []string{c},
			Examples:    examples,
		})
	}

	ifaces := []Interface{}
	for _, t := range opt.Interfaces {
		u := strings.ReplaceAll(t.URLTemplate, "{agent_id}", def.ID)
		if u == "" || t.ProtocolBinding == "" {
			continue
		}
		ifaces = append(ifaces, Interface{URL: u, ProtocolBinding: t.ProtocolBinding, ProtocolVersion: ProtocolVersion})
	}

	params := map[string]any{
		"agent_id":   def.ID,
		"trust_tier": TrustTier(def),
		"runtime":    runtimeName(def),
	}
	if def.SourceHash != "" {
		params["source_hash"] = def.SourceHash
	}
	if def.Profile != "" {
		params["profile"] = def.Profile
	}
	if tools := nonEmptyStrings(man.ToolAllowlist); len(tools) > 0 {
		// Names (globs) only: what the agent may reach, never grant internals.
		params["tool_allowlist"] = tools
	}
	if sk := nonEmptyStrings(man.Skills); len(sk) > 0 {
		params["skills"] = sk
	}
	if man.StreamsOutput {
		// ADR-0139 amendment 2026-11-06: the agent pushes its own live step
		// output (EmitStepOutput). Emitted only when true — a default-valued member
		// is never emitted (A2A §8.4.1 canonicalisation).
		params["streams_output"] = true
	}

	c := Card{
		Name:                name,
		Description:         def.Description,
		SupportedInterfaces: ifaces,
		Version:             cardVersion(def, man),
		DocumentationURL:    opt.DocumentationURL,
		Capabilities: Capabilities{
			// streaming / pushNotifications / extendedAgentCard are all false
			// (omitted): the kernel serves no A2A task endpoint of its own.
			Extensions: []Extension{{
				URI:         ExtensionURI,
				Description: "Cambrian kernel metadata: the kernel-assigned trust tier (ADR-0139 D8) and the agent's tool allowlist.",
				Params:      params,
			}},
		},
		SecuritySchemes: map[string]SecurityScheme{
			bearerSchemeKey: {HTTPAuth: &HTTPAuthScheme{
				Scheme: "Bearer",
				Description: "Every Cambrian plane requires a bearer credential issued by the deployment's operator; " +
					"there is no anonymous access.",
			}},
		},
		SecurityRequirements: []SecurityRequirement{{Schemes: map[string]ScopeList{bearerSchemeKey: {}}}},
		DefaultInputModes:    modes,
		DefaultOutputModes:   modes,
		Skills:               skills,
	}
	if opt.Provider.Organization != "" && opt.Provider.URL != "" {
		p := opt.Provider
		c.Provider = &p
	}
	return c
}

// cardVersion is the manifest version plus the source hash as semver build
// metadata, so two cards with one version but different code are told apart.
func cardVersion(def domain.AgentDefinition, man *domain.AgentManifest) string {
	v := man.Version
	if v == "" {
		v = def.ManifestVersion
	}
	if v == "" {
		v = "0.0.0"
	}
	if h := shortHash(def.SourceHash); h != "" {
		v += "+" + h
	}
	return v
}

func shortHash(h string) string {
	h = strings.TrimSpace(h)
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[i+1:]
	}
	var b strings.Builder
	for _, r := range h {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
		if b.Len() == 12 {
			break
		}
	}
	return b.String()
}

func runtimeName(def domain.AgentDefinition) string {
	if def.Runtime == "" {
		return "unknown"
	}
	return string(def.Runtime)
}

// mediaTypes maps manifest formats to A2A media types. A manifest that says
// nothing gets text/plain, which is what every Cambrian agent accepts.
func mediaTypes(formats []string) []string {
	known := map[string]string{
		"text":     "text/plain",
		"json":     "application/json",
		"markdown": "text/markdown",
		"md":       "text/markdown",
		"html":     "text/html",
		"pdf":      "application/pdf",
		"csv":      "text/csv",
	}
	var out []string
	for _, f := range formats {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		if m, ok := known[f]; ok {
			f = m
		} else if !strings.Contains(f, "/") {
			continue // not a media type and not one we can map: say nothing rather than guess
		}
		out = append(out, f)
	}
	out = uniqueSorted(out)
	if len(out) == 0 {
		return []string{"text/plain"}
	}
	return out
}

func humanize(id string) string {
	words := strings.FieldsFunc(id, func(r rune) bool { return r == '_' || r == '-' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func nonEmpty(a, b []string) []string {
	if len(nonEmptyStrings(a)) > 0 {
		return a
	}
	return b
}

func nonEmptyStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
