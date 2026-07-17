package discovery

import (
	"context"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Defaults for the deterministic scan governors (ADR-0051 D5 kept, ADR-0078 D9).
const (
	defaultMaxProbes    = 5
	defaultProbeTimeout = 4 * time.Second
)

// Registry holds the deterministic discovery sources keyed by Kind and runs the bounded
// probe pass (ADR-0078 D2). Data-driven like the chunker_registry (ADR-0060): an unknown
// target Kind is skipped-and-stamped, never a code branch. Safe for concurrent use after
// construction (sources are read-only; the map is not mutated after Register).
type Registry struct {
	sources      map[string]domain.DiscoverySource
	maxProbes    int
	probeTimeout time.Duration
}

// NewRegistry builds a Registry from the given sources. Later registrations of the same
// Kind win. Zero governors fall back to the package defaults.
func NewRegistry(sources ...domain.DiscoverySource) *Registry {
	r := &Registry{
		sources:      make(map[string]domain.DiscoverySource, len(sources)),
		maxProbes:    defaultMaxProbes,
		probeTimeout: defaultProbeTimeout,
	}
	for _, s := range sources {
		r.Register(s)
	}
	return r
}

// Register adds (or replaces) a source by its Kind.
func (r *Registry) Register(s domain.DiscoverySource) {
	if s == nil {
		return
	}
	r.sources[s.Kind()] = s
}

// WithGovernors overrides the scan cap and per-probe timeout (0 = keep current).
func (r *Registry) WithGovernors(maxProbes int, probeTimeout time.Duration) *Registry {
	if maxProbes > 0 {
		r.maxProbes = maxProbes
	}
	if probeTimeout > 0 {
		r.probeTimeout = probeTimeout
	}
	return r
}

// Empty reports whether any source is registered (nothing registered ⇒ the deterministic
// path is a no-op and the caller degrades to one-shot / env-only).
func (r *Registry) Empty() bool { return r == nil || len(r.sources) == 0 }

// Discover runs the deterministic probes selected from userInput and returns the observed
// entities plus the canonical kind:ref of any target left unobserved (over the scan cap,
// no matching source, or a probe error). No LLM (ADR-0078 D1). Never returns an error: a
// probe failure degrades to an unobserved stamp (ADR-0051 D8 — never discard, never crash).
func (r *Registry) Discover(ctx context.Context, userInput string) (entities []domain.DiscoveredEntity, unobserved []string) {
	if r.Empty() {
		return nil, nil
	}
	targets := SelectTargets(userInput)
	scans := 0
	for _, t := range targets {
		src, ok := r.sources[t.Kind]
		if !ok {
			continue // no source for this kind — silently skip (not an observation gap)
		}
		if scans >= r.maxProbes {
			unobserved = append(unobserved, t.Kind+":"+t.Ref)
			continue
		}
		scans++
		pctx, cancel := context.WithTimeout(ctx, r.probeTimeout)
		ents, err := src.Probe(pctx, t)
		cancel()
		if err != nil {
			slog.Debug("discovery: probe failed; stamping unobserved",
				"kind", t.Kind, "ref", t.Ref, "err", err)
			unobserved = append(unobserved, t.Kind+":"+t.Ref)
			continue
		}
		entities = append(entities, ents...)
	}
	return entities, unobserved
}
