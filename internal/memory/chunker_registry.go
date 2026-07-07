// Package memory — Chunker Registry.
//
// Registry routes (sourceType, ext) to a registered Chunker via config-driven
// precedence: match(SourceType) → match(ext) → default. Data-driven, NOT
// Go if/else (Zero-Hardcode Rule per AGENTS.md).
//
// The registry is the single switchboard the IngestionManager uses to pick
// a Chunker for every incoming document (ADR-0060 D5 / D9). The default
// name, the sourceType→chunker map, and the ext→chunker map are all
// config values — no Go branching on sourceType values exists on the
// path (Zero-Hardcode Rule, AGENTS.md).
package memory

import (
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

const lateChunkerName = "late"

// ChunkerConfig is the routing block the registry reads. It is the
// spec-shaped mirror of the future `config.ChunkerConfig` (ADR-0060 D7,
// T-1.11) and lives in this package for now so the registry is
// testable in isolation; T-1.11 will promote it to
// internal/config/config.go and have the registry read it from there.
//
// Field shape matches ADR-0060 D7 verbatim:
//
//	Default   string                       // default "option_c"
//	Routes    map[string]string            // sourceType → chunker name
//	ExtRoutes map[string]string            // ext → chunker name (second precedence)
//	Late      LateChunkerConfig            // late-chunking gate (T-2.4)
type ChunkerConfig struct {
	// Default is the chunker name used when no route matches. The spec
	// default is "option_c" (ADR-0060 D5) — the back-compat floor. An
	// empty Default at NewRegistry time is a config error.
	Default string
	// Routes maps SourceType → chunker name (first precedence in
	// Resolve). The keys are the values the IngestionManager passes as
	// ExternalDocument.SourceType (e.g. "file_drop", "slack", "email").
	Routes map[string]string
	// ExtRoutes maps file extension → chunker name (second precedence
	// in Resolve). The keys carry the leading dot (e.g. ".go", ".md"),
	// matching the convention the chunkers use in their Supports check.
	ExtRoutes map[string]string
	// Late is the late-chunking gate config (ADR-0060 D6). T-2.4 will
	// wire the gate into Resolve; for T-1.8 the registry just carries
	// the config so the schema is in place.
	Late LateChunkerConfig
}

// LateChunkerConfig is the late-chunking gate config. Resolved in T-2.4;
// included here so the registry's config struct matches the ADR shape
// (D7) and T-1.11's promotion is a pure cut/paste.
type LateChunkerConfig struct {
	// Enabled gates the late chunker on/off. Default false per spec.
	Enabled bool
	// MaxDocTokens caps the body size that late chunking will accept;
	// larger bodies fall back to OptionCChunker (ADR-0060 D6). Default
	// 8192, matching the existing nomic-embed-text 8K context window.
	MaxDocTokens int
}

// Registry routes (sourceType, ext) to a registered Chunker via
// config-driven precedence: match(sourceType) → match(ext) → default.
// The routing is data-driven (a map lookup), NOT a Go if/else / switch
// on sourceType values (Zero-Hardcode Rule per cambrian-core/AGENTS.md).
//
// The internal `routes` field is a single map[string]string holding
// BOTH sourceType→chunker_name and ext→chunker_name entries; Resolve
// looks up sourceType first, then ext, then falls back to defaultChr.
// On a key collision between Routes and ExtRoutes, the sourceType
// entry (Routes) wins — matching the Resolve precedence.
type Registry struct {
	chunkers   map[string]domain.Chunker
	routes     map[string]string
	defaultChr string
	lateGate   func() bool
}

// NewRegistry validates that every route + default in cfg points to a
// known chunker name (in the chunkers map). Returns an error if any
// unknown name is found.
//
// The validation is strict: an unknown name is a config error, not a
// silent fallback (ADR-0060 D7). The default name is the floor; a
// misconfigured default fails closed at startup rather than silently
// routing every doc to the wrong chunker.
func NewRegistry(chunkers map[string]domain.Chunker, cfg ChunkerConfig) (*Registry, error) {
	if chunkers == nil {
		return nil, fmt.Errorf("chunker_registry: chunkers map is nil")
	}
	if cfg.Default == "" {
		return nil, fmt.Errorf("chunker_registry: cfg.Default is empty")
	}
	if _, ok := chunkers[cfg.Default]; !ok {
		return nil, fmt.Errorf("chunker_registry: default chunker %q is not registered", cfg.Default)
	}

	routes := make(map[string]string, len(cfg.Routes)+len(cfg.ExtRoutes))
	for k, v := range cfg.Routes {
		if _, ok := chunkers[v]; !ok {
			return nil, fmt.Errorf("chunker_registry: route[%q] -> %q: chunker not registered", k, v)
		}
		routes[k] = v
	}
	for k, v := range cfg.ExtRoutes {
		if _, ok := chunkers[v]; !ok {
			return nil, fmt.Errorf("chunker_registry: ext_route[%q] -> %q: chunker not registered", k, v)
		}
		// On a key collision, the sourceType entry (cfg.Routes) wins —
		// matching the Resolve precedence. The ExtRoutes entry is
		// silently skipped in that case; collision is operator error,
		// not a silent data loss (a warning is loud, an overwrite is a
		// bug), so we never overwrite a known entry.
		if _, taken := routes[k]; !taken {
			routes[k] = v
		}
	}

	return &Registry{
		chunkers:   chunkers,
		routes:     routes,
		defaultChr: cfg.Default,
	}, nil
}

func (r *Registry) SetLateGate(gate func() bool) {
	r.lateGate = gate
}

// Resolve picks the right chunker for a (sourceType, ext) pair. The
// precedence is:
//
//  1. If cfg.Routes[sourceType] is set AND the chunker Supports the
//     (sourceType, ext), use it.
//  2. If cfg.Routes[ext] is set AND the chunker Supports the (sourceType, ext),
//     use it.
//  3. Use defaultChr (the configured default; spec default "option_c").
//
// Internally both maps are merged into the single `routes` field at
// NewRegistry time (cfg.Routes processed first, cfg.ExtRoutes second
// with cfg.Routes winning on key collision), so the two lookups below
// are pure map reads against the merged table.
//
// The two lookup steps are pure map reads — no Go if/else / switch on
// sourceType values. The Supports check is what makes the registry
// safe: a route that points at a chunker that does not support the
// actual (sourceType, ext) pair falls through to the next level
// (matches the TestRegistry_Resolve_SupportsFalse regression bar).
//
// Returns the chosen chunker, or an error if the default is unknown.
// A misconfigured default is a NewRegistry-time error, not a
// Resolve-time error, so reaching the error branch here means the
// registry was constructed directly (not via NewRegistry) — defensive.
func (r *Registry) Resolve(sourceType, ext string) (domain.Chunker, error) {
	if name, ok := r.routes[sourceType]; ok {
		if c, ok := r.chunkers[name]; ok && c.Supports(sourceType, ext) {
			if chosen, ok := r.applyLateGate(c, sourceType, ext); ok {
				return chosen, nil
			}
		}
	}
	if name, ok := r.routes[ext]; ok {
		if c, ok := r.chunkers[name]; ok && c.Supports(sourceType, ext) {
			if chosen, ok := r.applyLateGate(c, sourceType, ext); ok {
				return chosen, nil
			}
		}
	}
	c, ok := r.chunkers[r.defaultChr]
	if !ok {
		return nil, fmt.Errorf("chunker_registry: default chunker %q is not registered", r.defaultChr)
	}
	return c, nil
}

func (r *Registry) applyLateGate(c domain.Chunker, sourceType, ext string) (domain.Chunker, bool) {
	if c.Name() != lateChunkerName {
		return c, true
	}
	if r.lateGate == nil || r.lateGate() {
		return c, true
	}
	slog.Warn("chunker_registry: late chunker gated, falling back to default",
		"source_type", sourceType,
		"ext", ext,
		"default", r.defaultChr)
	return nil, false
}
