package domain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
)

// LayeredGrantsStore is the kernel's tool grants view (ENG-1116): the grants an
// operator wrote, held in a ToolGrantStore (durable in Postgres when the kernel
// has a database), merged over the grants profile agents derive from their tool
// allowlists, held in memory and re-derived on every boot and MCP tool change.
//
// Why two layers: profile grants are a function of the profile file and the live
// registry, so persisting them would let a grant the allowlist no longer matches
// outlive a restart; operator grants have no other source, so losing them on a
// restart silently revoked them (incl. propose_knowledge / propose_ontology_change
// grants). Each layer is written by its own owner: the profile sync writes
// Profile(), the operator plane writes ReplaceGrants.
//
// Reads go to the operator layer on every call (read-through, no cache): a store
// error is returned, and the executor denies on an error (fail closed). A cache
// would also miss a grant written by another kernel on the same database.
type LayeredGrantsStore struct {
	operator ToolGrantStore
	profile  *InMemoryGrantsStore
	// mu serialises ReplaceGrants' read-split-write, so two writes for one agent
	// cannot interleave between reading the operator set and replacing it.
	mu sync.Mutex
}

var _ ToolGrantStore = (*LayeredGrantsStore)(nil)

// NewLayeredGrantsStore builds the view over the operator layer, which is
// required (AGENTS.md §2.3: no store attached later).
func NewLayeredGrantsStore(operator ToolGrantStore) (*LayeredGrantsStore, error) {
	if operator == nil {
		return nil, errors.New("tool grants: operator grant store is required")
	}
	return &LayeredGrantsStore{operator: operator, profile: NewInMemoryGrantsStore()}, nil
}

// Profile is the in-memory profile-allowlist layer the profile grant sync writes.
// Never persisted.
func (s *LayeredGrantsStore) Profile() *InMemoryGrantsStore { return s.profile }

// GrantsFor returns the agent's profile grants merged with its operator grants;
// for a tool in both, the operator's entry (its resource policy) wins. An empty
// agentID yields none; an operator-layer error is returned with no grants.
func (s *LayeredGrantsStore) GrantsFor(ctx context.Context, agentID string) ([]ToolGrant, error) {
	if agentID == "" {
		return nil, nil
	}
	op, err := s.operator.GrantsFor(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("tool grants: operator grants for %q: %w", agentID, err)
	}
	prof, _ := s.profile.GrantsFor(ctx, agentID) // in memory: never fails
	return mergeGrants(prof, op), nil
}

// AllGrants returns every agent's merged view (the operator plane's tool → agents
// reverse index).
func (s *LayeredGrantsStore) AllGrants(ctx context.Context) (map[string][]ToolGrant, error) {
	op, err := s.operator.AllGrants(ctx)
	if err != nil {
		return nil, fmt.Errorf("tool grants: all operator grants: %w", err)
	}
	prof := s.profile.All()
	out := make(map[string][]ToolGrant, len(op)+len(prof))
	for agent, gs := range prof {
		out[agent] = mergeGrants(gs, op[agent])
	}
	for agent, gs := range op {
		if _, done := out[agent]; !done {
			out[agent] = mergeGrants(nil, gs)
		}
	}
	return out, nil
}

// ReplaceGrants makes desired the agent's whole merged view, as the operator
// plane computes it from GrantsFor (grant/revoke one tool, or set one tool's
// policy). It splits desired between the layers:
//
//   - an entry equal to the agent's profile-derived grant, for a tool the operator
//     did not already hold, stays profile-only (not persisted);
//   - every other entry — an operator grant, or a policy set on a profile tool —
//     goes to the operator layer;
//   - a profile tool absent from desired is dropped from the profile layer until
//     the next profile sync re-derives it (the profile ceiling, not a grant,
//     governs profile agents — unchanged from the single in-memory store).
//
// The operator layer is written first; when that fails nothing changes.
func (s *LayeredGrantsStore) ReplaceGrants(ctx context.Context, agentID string, desired []ToolGrant) error {
	if agentID == "" {
		return errors.New("tool grants: agent id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oldOp, err := s.operator.GrantsFor(ctx, agentID)
	if err != nil {
		return fmt.Errorf("tool grants: read operator grants for %q: %w", agentID, err)
	}
	prof, _ := s.profile.GrantsFor(ctx, agentID)
	held := make(map[string]bool, len(oldOp))
	for _, g := range oldOp {
		held[g.Tool] = true
	}
	profBy := make(map[string]ToolGrant, len(prof))
	for _, g := range prof {
		profBy[g.Tool] = g
	}
	want := make(map[string]bool, len(desired))
	opNext := make([]ToolGrant, 0, len(desired))
	for _, g := range desired {
		if want[g.Tool] {
			continue // one grant per tool
		}
		want[g.Tool] = true
		if p, ok := profBy[g.Tool]; ok && !held[g.Tool] && reflect.DeepEqual(p.Policy, g.Policy) {
			continue
		}
		opNext = append(opNext, g)
	}
	if err := s.operator.ReplaceGrants(ctx, agentID, opNext); err != nil {
		return fmt.Errorf("tool grants: write operator grants for %q: %w", agentID, err)
	}
	profNext := make([]ToolGrant, 0, len(prof))
	for _, g := range prof {
		if want[g.Tool] {
			profNext = append(profNext, g)
		}
	}
	if len(profNext) != len(prof) {
		s.profile.Set(agentID, profNext)
	}
	return nil
}

// mergeGrants returns profile grants then operator grants, one entry per tool;
// a tool in both keeps its profile position with the operator's entry.
func mergeGrants(profile, operator []ToolGrant) []ToolGrant {
	if len(operator) == 0 {
		return append([]ToolGrant(nil), profile...)
	}
	opBy := make(map[string]ToolGrant, len(operator))
	for _, g := range operator {
		opBy[g.Tool] = g
	}
	out := make([]ToolGrant, 0, len(profile)+len(operator))
	seen := make(map[string]bool, len(profile)+len(operator))
	for _, g := range profile {
		if o, ok := opBy[g.Tool]; ok {
			g = o
		}
		out = append(out, g)
		seen[g.Tool] = true
	}
	for _, g := range operator {
		if !seen[g.Tool] {
			out = append(out, g)
			seen[g.Tool] = true
		}
	}
	return out
}
