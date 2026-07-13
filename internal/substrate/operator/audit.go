package operator

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// AuditStore / AuditFilter are the domain port (ADR-0047 D15/0047-24); aliased
// here so the operator code reads naturally and the Postgres infra adapter can
// implement the same interface without an infra→substrate import.
type AuditStore = domain.AuditStore
type AuditFilter = domain.AuditFilter

// InMemoryAuditStore is the default AuditStore (V1 / tests). A Postgres-backed
// store (operator_audit table) is the production impl behind the same port.
type InMemoryAuditStore struct {
	mu      sync.Mutex
	byCmd   map[string]struct{}
	entries []domain.AuditEntry // insertion order
}

// NewInMemoryAuditStore constructs an empty store.
func NewInMemoryAuditStore() *InMemoryAuditStore {
	return &InMemoryAuditStore{byCmd: make(map[string]struct{})}
}

// Record implements AuditStore. The CommandID uniqueness check is the dedup.
func (s *InMemoryAuditStore) Record(_ context.Context, entry domain.AuditEntry) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byCmd[entry.CommandID]; ok {
		return true, nil
	}
	s.byCmd[entry.CommandID] = struct{}{}
	s.entries = append(s.entries, entry)
	return false, nil
}

// Query implements AuditStore (most recent first).
func (s *InMemoryAuditStore) Query(_ context.Context, f AuditFilter) ([]domain.AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.AuditEntry
	for _, e := range s.entries {
		if f.Actor != "" && e.Actor != f.Actor {
			continue
		}
		if f.TargetType != "" && e.TargetType != f.TargetType {
			continue
		}
		if f.TargetID != "" && e.TargetID != f.TargetID {
			continue
		}
		if f.ActionType != "" && e.ActionType != f.ActionType {
			continue
		}
		if f.CommandID != "" && e.CommandID != f.CommandID {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// toolNamesJSON renders a grant set as a stable JSON array of tool names — the
// before/after value for a tool-grant mutation.
func toolNamesJSON(grants []domain.ToolGrant) string {
	names := make([]string, 0, len(grants))
	for _, g := range grants {
		names = append(names, g.Tool)
	}
	sort.Strings(names)
	return "[" + strings.Join(quoteAll(names), ",") + "]"
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = "\"" + s + "\""
	}
	return out
}
