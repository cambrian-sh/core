// Package integrations is the curated MCP integration catalog: the data that
// lets a non-developer connect a tool (mail, drive, calendar, a database, web
// search) from the operator console without editing mcp.json
// (T-20261106-integrations-manage-mcp-servers-from-the).
//
// The catalog is DATA, not code: catalog.json is embedded in the binary and an
// optional deployment file (mcp.catalog_file) adds or replaces entries. There is
// no per-server Go branch anywhere (Zero-Hardcode Rule) — an entry carries the
// launch spec, the credential fields, the default tool policy and the probe the
// generic install path needs.
//
// Owner directive (decisions/agent-stack-v2-mcp-tools-and-profiles.md): tools
// come only from curated, pinned MCP servers; every server declares a policy
// and an unlisted tool fails closed. Load enforces the second half: an entry
// that declares no policy and does not say fail_closed is refused.
package integrations

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/procenv"
)

//go:embed catalog.json
var builtinCatalog []byte

// Field kinds.
const (
	KindEnv   = "env"   // an environment variable of the server process
	KindToken = "token" // the http/sse auth credential (mcp:<id>:token)
)

// Trust tiers.
const (
	TrustOfficial  = "official"
	TrustCommunity = "community"
	// TrustCurated is a community server Cambrian has reviewed and pins, which may
	// launch with the vetted containment tier (owner decision 2026-11-06 for
	// code-sandbox, whose code already runs in its own Docker container;
	// T-20261106-mark-code-sandbox-vetted-add-a-curated-catalog). It is provenance
	// plus review, not a claim that the vendor published it ("official").
	TrustCurated = "curated"
)

// Sources of a catalog entry.
const (
	SourceBuiltin    = "builtin"
	SourceDeployment = "deployment"
)

// Field is one value an integration needs.
type Field struct {
	Name         string `json:"name"`
	Label        string `json:"label"`
	Secret       bool   `json:"secret,omitempty"`
	Required     bool   `json:"required,omitempty"`
	Kind         string `json:"kind,omitempty"` // "env" (default) | "token"
	Help         string `json:"help,omitempty"`
	DefaultValue string `json:"default_value,omitempty"`
	Placeholder  string `json:"placeholder,omitempty"`
}

// ProbeTool is a harmless tool TestIntegration may call once.
type ProbeTool struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// Entry is one catalog integration.
type Entry struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	Category      string                 `json:"category"`
	Publisher     string                 `json:"publisher,omitempty"`
	License       string                 `json:"license,omitempty"`
	TrustTier     string                 `json:"trust_tier"`
	DocsURL       string                 `json:"docs_url,omitempty"`
	Version       string                 `json:"version"`
	Runtimes      []string               `json:"runtimes,omitempty"`
	Platforms     []string               `json:"platforms,omitempty"`
	MultiInstance bool                   `json:"multi_instance,omitempty"`
	FailClosed    bool                   `json:"fail_closed,omitempty"`
	Server        config.MCPServerConfig `json:"server"`
	Fields        []Field                `json:"fields,omitempty"`
	Probe         *ProbeTool             `json:"probe_tool,omitempty"`
	Notes         string                 `json:"notes,omitempty"`
	// Source is set by the loader, never read from a file.
	Source string `json:"-"`
}

// PlatformSupported reports whether the entry runs on this kernel's OS.
func (e Entry) PlatformSupported() bool {
	if len(e.Platforms) == 0 {
		return true
	}
	for _, p := range e.Platforms {
		if p == runtime.GOOS {
			return true
		}
	}
	return false
}

// FieldByName returns the entry's field with that name.
func (e Entry) FieldByName(name string) (Field, bool) {
	for _, f := range e.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

type file struct {
	SchemaVersion int     `json:"schema_version"`
	Entries       []Entry `json:"entries"`
}

// Catalog is the loaded, validated catalog in display order.
type Catalog struct {
	entries []Entry
	byID    map[string]int
}

// Entries returns the entries in display order (a copy).
func (c *Catalog) Entries() []Entry {
	if c == nil {
		return nil
	}
	return append([]Entry(nil), c.entries...)
}

// Get returns one entry.
func (c *Catalog) Get(id string) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	i, ok := c.byID[id]
	if !ok {
		return Entry{}, false
	}
	return c.entries[i], true
}

// Categories returns the distinct categories in first-seen order.
func (c *Catalog) Categories() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range c.Entries() {
		if !seen[e.Category] {
			seen[e.Category] = true
			out = append(out, e.Category)
		}
	}
	return out
}

// Load returns the built-in catalog merged with the deployment file at
// deploymentPath ("" ⇒ none). A deployment entry with a built-in id REPLACES it
// in place; new ids are appended. Any invalid entry fails the whole load: the
// catalog is what the console offers to install, and a half-loaded catalog
// would hide the entry an operator is looking for without saying why.
func Load(deploymentPath string) (*Catalog, error) {
	base, err := parse(builtinCatalog, SourceBuiltin)
	if err != nil {
		return nil, fmt.Errorf("integration catalog (built-in): %w", err)
	}
	cat := &Catalog{byID: map[string]int{}}
	for _, e := range base {
		cat.byID[e.ID] = len(cat.entries)
		cat.entries = append(cat.entries, e)
	}
	if deploymentPath == "" {
		return cat, nil
	}
	raw, err := os.ReadFile(deploymentPath)
	if err != nil {
		return nil, fmt.Errorf("integration catalog: read %s: %w", deploymentPath, err)
	}
	extra, err := parse(raw, SourceDeployment)
	if err != nil {
		return nil, fmt.Errorf("integration catalog %s: %w", deploymentPath, err)
	}
	for _, e := range extra {
		if i, ok := cat.byID[e.ID]; ok {
			cat.entries[i] = e
			continue
		}
		cat.byID[e.ID] = len(cat.entries)
		cat.entries = append(cat.entries, e)
	}
	return cat, nil
}

// ResolvePath resolves a configured catalog path against the config file's dir.
func ResolvePath(path, configPath string) string {
	if path == "" || filepath.IsAbs(path) || configPath == "" {
		return path
	}
	return filepath.Join(filepath.Dir(configPath), path)
}

func parse(raw []byte, source string) ([]Entry, error) {
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if f.SchemaVersion != 1 {
		return nil, fmt.Errorf("schema_version %d is not supported (want 1)", f.SchemaVersion)
	}
	seen := map[string]bool{}
	var errs []string
	for i := range f.Entries {
		e := &f.Entries[i]
		e.Source = source
		for j := range e.Fields {
			if e.Fields[j].Kind == "" {
				e.Fields[j].Kind = KindEnv
			}
		}
		if seen[e.ID] {
			errs = append(errs, fmt.Sprintf("entry %q appears twice", e.ID))
		}
		seen[e.ID] = true
		errs = append(errs, Validate(*e)...)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return f.Entries, nil
}

var validTransports = map[string]bool{"stdio": true, "http": true, "sse": true}
var validPlatforms = map[string]bool{"linux": true, "darwin": true, "windows": true}

// Validate checks one entry. Exported so tests and a future "validate my
// deployment catalog" CLI share one statement of the rules.
func Validate(e Entry) []string {
	var errs []string
	add := func(format string, a ...any) {
		errs = append(errs, fmt.Sprintf("entry %q: ", e.ID)+fmt.Sprintf(format, a...))
	}
	if !config.ValidMCPServerID(e.ID) {
		add("id must be lowercase letters, digits, '.', '_' or '-' (max 63)")
	}
	if e.Name == "" || e.Category == "" || e.Version == "" {
		add("name, category and version are required")
	}
	if e.TrustTier != TrustOfficial && e.TrustTier != TrustCommunity && e.TrustTier != TrustCurated {
		add("trust_tier must be %q, %q or %q", TrustOfficial, TrustCurated, TrustCommunity)
	}
	for _, p := range e.Platforms {
		if !validPlatforms[p] {
			add("unknown platform %q", p)
		}
	}
	s := e.Server
	if s.ID != "" {
		add("server.id must be empty (the install picks the id)")
	}
	if !validTransports[s.Transport] {
		add("server.transport %q is not stdio, http or sse", s.Transport)
	}
	if s.Endpoint == "" {
		add("server.endpoint is required")
	}
	if s.Transport != "stdio" && !strings.HasPrefix(s.Endpoint, "https://") && !strings.HasPrefix(s.Endpoint, "http://") {
		add("server.endpoint must be a URL for %s", s.Transport)
	}
	if len(s.EnvValues) > 0 || len(s.SecretEnv) > 0 {
		add("server.env_values / secret_env come from fields, not the server spec")
	}
	// ADR-0139 D8: the entry's trust_tier decides the containment tier (through
	// mcp.sandbox.catalog_tiers); the server spec only declares its needs.
	if s.TrustTier != "" {
		add("server.trust_tier must be empty: the entry's trust_tier decides the containment tier")
	}
	if s.Sandbox != nil && s.Transport != "stdio" {
		add("server.sandbox applies only to stdio servers")
	}
	for _, msg := range config.ValidateMCPServerSandbox("server", "", s.Sandbox) {
		add("%s", msg)
	}
	// ADR-0140: the per-user credential model is checked by the same rules a
	// console write and a boot are (scope, provider, injection fits the
	// transport).
	withID := s
	withID.ID = e.ID
	for _, msg := range config.ValidateMCPServer(withID) {
		add("%s", msg)
	}

	// Policy: declared, or explicitly fail-closed. Never silent.
	if len(s.Tools) == 0 && s.DefaultToolPolicy == nil && !e.FailClosed {
		add("declares no tool policy: list tools, set default_tool_policy, or say fail_closed: true")
	}
	if e.FailClosed && s.DefaultToolPolicy != nil {
		add("fail_closed contradicts default_tool_policy")
	}
	if d := s.DefaultToolPolicy; d != nil && !d.Dangerous && hasMutatingEffect(d.Effects) {
		// configs/MCP.md: never give a server that can write a non-dangerous
		// default — every tool a future version adds would be safe by default.
		add("default_tool_policy is non-dangerous but declares write/spend/admin effects")
	}
	policies := append([]config.MCPToolConfig(nil), s.Tools...)
	if s.DefaultToolPolicy != nil {
		policies = append(policies, *s.DefaultToolPolicy)
	}
	for _, p := range policies {
		for _, eff := range p.Effects {
			if !validEffect(eff) {
				add("tool %q: effect %q is outside the closed set", p.Name, eff)
			}
		}
	}

	// Fields.
	names := map[string]Field{}
	for _, f := range e.Fields {
		if _, dup := names[f.Name]; dup {
			add("field %q appears twice", f.Name)
		}
		names[f.Name] = f
		switch {
		case f.Name == "" || f.Label == "":
			add("every field needs a name and a label")
		case procenv.IsKernelName(f.Name):
			add("field %q is a kernel variable and can never be given to a server", f.Name)
		}
		switch f.Kind {
		case KindEnv:
			if !f.Secret && procenv.IsSecretName(f.Name) {
				add("field %q looks like a credential and must be secret: true", f.Name)
			}
			if f.Secret && f.DefaultValue != "" {
				add("secret field %q cannot carry a default_value", f.Name)
			}
			if s.Transport != "stdio" {
				add("env field %q on a %s server: only stdio servers have an environment", f.Name, s.Transport)
			}
		case KindToken:
			if !f.Secret {
				add("token field %q must be secret", f.Name)
			}
			if s.Transport == "stdio" || s.Auth.Type == "" || s.Auth.Type == "none" {
				add("token field %q needs an http/sse server with auth.type bearer or header", f.Name)
			}
		default:
			add("field %q: kind %q is not env or token", f.Name, f.Kind)
		}
	}
	for _, a := range s.Args {
		if name, ok := mcp.Placeholder(a); ok {
			f, declared := names[name]
			if !declared || f.Secret || f.Kind != KindEnv {
				add("arg %q must name a non-secret env field", a)
			}
		}
	}

	// Probe: must be harmless under the entry's OWN policy.
	if e.Probe != nil {
		sc := mcp.ServerConfig{ID: e.ID, Tools: map[string]mcp.ToolPolicy{}}
		for _, t := range s.Tools {
			sc.Tools[t.Name] = mcp.ToolPolicy{Dangerous: t.Dangerous, Effects: t.Effects, DataWriteKinds: t.DataWriteKinds}
		}
		if d := s.DefaultToolPolicy; d != nil {
			sc.DefaultPolicy = &mcp.ToolPolicy{Dangerous: d.Dangerous, Effects: d.Effects, DataWriteKinds: d.DataWriteKinds}
		}
		rep := mcp.DescribeToolPolicy(sc, e.Probe.Name)
		if rep.Dangerous || hasMutatingEffect(rep.Effects) {
			add("probe_tool %q is not read-only under the entry's policy", e.Probe.Name)
		}
	}
	return errs
}

func hasMutatingEffect(effects []string) bool {
	for _, e := range effects {
		if e != string(domain.EffectRead) && e != string(domain.EffectEgress) {
			return true
		}
	}
	return false
}

func validEffect(name string) bool {
	for _, e := range domain.AllToolEffects {
		if string(e) == name {
			return true
		}
	}
	return false
}
