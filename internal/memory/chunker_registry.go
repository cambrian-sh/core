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
//
// The routing config is config.ChunkerConfig — the operator-facing schema, the
// one parsed from the `chunker` block and validated against KnownChunkerNames.
// This package used to declare its OWN structurally-identical ChunkerConfig and
// LateChunkerConfig, described in a comment as "the spec-shaped mirror of the
// future config.ChunkerConfig … T-1.11 will promote it". T-1.11 was done and the
// promoted types exist, but the mirror was never deleted and was the copy the
// ingestion path actually used — so the operator's `chunker` block was parsed,
// defaulted and validated, and then had no effect on anything. Two types meant
// two sources of truth, and the authoritative-looking one was inert.
package memory

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

const lateChunkerName = "late"

// ChunkerConfig and LateChunkerConfig were removed from this package. The
// routing block is config.ChunkerConfig / config.LateChunkerConfig — see the
// package comment for why having two of them meant the operator-facing one did
// nothing. Use the config types directly.

// NewDefaultChunkers builds the full set of chunkers the registry can route to.
//
// Every name in config.KnownChunkerNames is registered here, and that is the
// point: the config layer validates an operator's `chunker.default` and
// `chunker.routes` against KnownChunkerNames, so a name that validates but is
// not registered is a promise the kernel cannot keep. Previously only
// "option_c" was ever registered, so setting `chunker.default: markdown_header`
// passed validation and then silently did nothing — the other four
// implementations were unreachable code that the benchmark suite nonetheless
// measured, via a hand-written Python port.
//
// embedder feeds the late chunker, which needs to embed a whole document before
// splitting it. A plain Embedder is adapted through domain.EmbedBatchForwarder;
// a backend that can vectorise a batch in one call satisfies domain.BatchEmbedder
// directly and is used as-is (ADR-0060 D3).
func NewDefaultChunkers(embedder domain.Embedder, cfg config.ChunkerConfig) map[string]domain.Chunker {
	chunkers := map[string]domain.Chunker{
		OptionCChunker{}.Name():                   OptionCChunker{},
		ASTGoChunker{}.Name():                     ASTGoChunker{},
		MarkdownHeaderChunker{}.Name():            MarkdownHeaderChunker{},
		NewRecursiveCharacterChunker(0, 0).Name(): NewRecursiveCharacterChunker(0, 0),
	}
	// The late chunker is only registrable when there is something to embed with.
	// A nil embedder is a legitimate state in tests and in a degraded boot, and
	// registering a chunker that would nil-panic on first use is worse than not
	// offering it: NewRegistry then rejects a route naming it, loudly, at startup.
	if embedder != nil {
		chunkers[lateChunkerName] = NewLateChunker(asBatchEmbedder(embedder), cfg.Late.MaxDocTokens)
	}
	return chunkers
}

// asBatchEmbedder adapts a plain Embedder to the BatchEmbedder the late chunker
// wants, using the domain-provided forwarder when the backend has no native
// batch call.
func asBatchEmbedder(e domain.Embedder) domain.BatchEmbedder {
	if be, ok := e.(domain.BatchEmbedder); ok {
		return be
	}
	return forwardingBatchEmbedder{Embedder: e}
}

type forwardingBatchEmbedder struct{ domain.Embedder }

func (f forwardingBatchEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	return domain.EmbedBatchForwarder(f.Embedder, ctx, texts)
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
func NewRegistry(chunkers map[string]domain.Chunker, cfg config.ChunkerConfig) (*Registry, error) {
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
