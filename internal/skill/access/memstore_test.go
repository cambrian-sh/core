package access

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// memStore is an in-memory domain.SkillGrantStore with the Postgres adapter's
// semantics (one row per pair; audit only on change; command_id unique).
type memStore struct {
	mu       sync.Mutex
	grants   map[string]domain.SkillGrant
	requests map[string]domain.SkillAccessRequest
	events   []domain.SkillGrantEvent
	failRead error
}

func newMemStore() *memStore {
	return &memStore{grants: map[string]domain.SkillGrant{}, requests: map[string]domain.SkillAccessRequest{}}
}

func (m *memStore) audit(e domain.SkillGrantEvent) {
	var seq int64
	for _, x := range m.events {
		if x.AgentID == e.AgentID && x.SkillName == e.SkillName && x.Seq > seq {
			seq = x.Seq
		}
	}
	e.Seq = seq + 1
	e.ID = domain.SkillGrantEventID(e.AgentID, e.SkillName, e.Seq)
	m.events = append(m.events, e)
}

func (m *memStore) Grant(_ context.Context, g domain.SkillGrant, commandID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := domain.SkillGrantID(g.AgentID, g.SkillName)
	if cur, ok := m.grants[id]; ok && cur.Active() {
		return false, nil
	}
	g.ID = id
	g.RevokedAt, g.RevokedBy, g.RevokeReason = time.Time{}, "", ""
	m.grants[id] = g
	rid := domain.SkillAccessRequestID(g.AgentID, g.SkillName)
	if r, ok := m.requests[rid]; ok && r.State != domain.SkillAccessGranted {
		r.State, r.DecidedAt, r.DecidedBy = domain.SkillAccessGranted, g.GrantedAt, g.GrantedBy
		m.requests[rid] = r
	}
	m.audit(domain.SkillGrantEvent{AgentID: g.AgentID, SkillName: g.SkillName, Action: domain.SkillGrantActionGranted,
		Source: g.Source, Actor: g.GrantedBy, Reason: g.Reason, At: g.GrantedAt, CommandID: commandID})
	return true, nil
}

func (m *memStore) Revoke(_ context.Context, agentID, skill, by, reason, commandID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := domain.SkillGrantID(agentID, skill)
	g, ok := m.grants[id]
	if !ok || !g.Active() {
		return domain.ErrSkillGrantNotFound
	}
	g.RevokedAt, g.RevokedBy, g.RevokeReason = at, by, reason
	m.grants[id] = g
	m.audit(domain.SkillGrantEvent{AgentID: agentID, SkillName: skill, Action: domain.SkillGrantActionRevoked,
		Actor: by, Reason: reason, At: at, CommandID: commandID})
	return nil
}

func (m *memStore) ActiveGrants(ctx context.Context, agentID string) ([]domain.SkillGrant, error) {
	if m.failRead != nil {
		return nil, m.failRead
	}
	return m.ListGrants(ctx, domain.SkillGrantFilter{AgentID: agentID})
}

func (m *memStore) ListGrants(_ context.Context, f domain.SkillGrantFilter) ([]domain.SkillGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.SkillGrant
	for _, g := range m.grants {
		if (f.AgentID == "" || g.AgentID == f.AgentID) && (f.SkillName == "" || g.SkillName == f.SkillName) && (f.IncludeRevoked || g.Active()) {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) OpenRequest(_ context.Context, r domain.SkillAccessRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.ID = domain.SkillAccessRequestID(r.AgentID, r.SkillName)
	r.State, r.DecidedAt, r.DecidedBy = domain.SkillAccessPending, time.Time{}, ""
	m.requests[r.ID] = r
	m.audit(domain.SkillGrantEvent{AgentID: r.AgentID, SkillName: r.SkillName, Action: domain.SkillGrantActionRequested,
		Actor: r.AgentID, Reason: r.Rationale, RunID: r.RunID, At: r.RequestedAt})
	return nil
}

func (m *memStore) DenyRequest(_ context.Context, agentID, skill, by string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := domain.SkillAccessRequestID(agentID, skill)
	if r, ok := m.requests[id]; ok {
		r.State, r.DecidedAt, r.DecidedBy = domain.SkillAccessDenied, at, by
		m.requests[id] = r
	}
	m.audit(domain.SkillGrantEvent{AgentID: agentID, SkillName: skill, Action: domain.SkillGrantActionDenied, Actor: by, At: at})
	return nil
}

func (m *memStore) ListRequests(_ context.Context, f domain.SkillGrantFilter) ([]domain.SkillAccessRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.SkillAccessRequest
	for _, r := range m.requests {
		if (f.AgentID == "" || r.AgentID == f.AgentID) && (f.SkillName == "" || r.SkillName == f.SkillName) && (f.State == "" || r.State == f.State) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memStore) EventByCommand(_ context.Context, commandID string) (domain.SkillGrantEvent, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if commandID != "" && e.CommandID == commandID {
			return e, true, nil
		}
	}
	return domain.SkillGrantEvent{}, false, nil
}

func (m *memStore) ListEvents(_ context.Context, f domain.SkillGrantFilter, _ int) ([]domain.SkillGrantEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.SkillGrantEvent
	for _, e := range m.events {
		if (f.AgentID == "" || e.AgentID == f.AgentID) && (f.SkillName == "" || e.SkillName == f.SkillName) {
			out = append(out, e)
		}
	}
	return out, nil
}
