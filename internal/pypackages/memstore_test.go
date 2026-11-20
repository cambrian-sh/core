package pypackages

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// memStore is an in-process domain.PythonPackageStore for tests; the durable
// adapter is postgres.PgPythonPackageStore (python_package_store_pg_test.go
// holds it to the same behaviour).
type memStore struct {
	mu     sync.Mutex
	recs   map[string]domain.PythonPackageApproval
	events map[string][]domain.PythonPackageEvent
	seq    int
	order  map[string]int
}

func newMemStore() *memStore {
	return &memStore{recs: map[string]domain.PythonPackageApproval{}, events: map[string][]domain.PythonPackageEvent{}, order: map[string]int{}}
}

func (m *memStore) appendLocked(e domain.PythonPackageEvent) {
	e.Seq = int64(len(m.events[e.ApprovalID]) + 1)
	e.ID = domain.PythonPackageEventID(e.ApprovalID, e.Seq)
	m.events[e.ApprovalID] = append(m.events[e.ApprovalID], e)
}

func (m *memStore) Open(_ context.Context, a domain.PythonPackageApproval, actor string) (domain.PythonPackageApproval, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.recs[a.ID]; ok {
		return cur, false, nil
	}
	if a.State == "" {
		a.State = domain.PythonPackagePending
	}
	m.recs[a.ID] = a
	m.seq++
	m.order[a.ID] = m.seq
	m.appendLocked(domain.PythonPackageEvent{ApprovalID: a.ID, Target: a.Target, Name: a.Name, Version: a.Version,
		Action: domain.PythonPackageActionRequested, Actor: actor, At: a.RequestedAt})
	return a, true, nil
}

func (m *memStore) Decide(_ context.Context, id, state, by, detail string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return domain.ErrPythonPackageNotFound
	}
	if r.State == state {
		return nil
	}
	r.State, r.DecidedBy, r.DecidedAt, r.Detail = state, by, at, detail
	m.recs[id] = r
	m.appendLocked(domain.PythonPackageEvent{ApprovalID: id, Target: r.Target, Name: r.Name, Version: r.Version, Action: state, Actor: by, Detail: detail, At: at})
	return nil
}

func (m *memStore) AttachCard(_ context.Context, id, approvalID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return domain.ErrPythonPackageNotFound
	}
	r.ApprovalID = approvalID
	m.recs[id] = r
	return nil
}

func (m *memStore) Reopen(_ context.Context, id, approvalID, actor string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return domain.ErrPythonPackageNotFound
	}
	r.State, r.ApprovalID, r.DecidedBy, r.DecidedAt, r.Detail = domain.PythonPackagePending, approvalID, "", time.Time{}, ""
	m.recs[id] = r
	m.appendLocked(domain.PythonPackageEvent{ApprovalID: id, Target: r.Target, Name: r.Name, Version: r.Version,
		Action: domain.PythonPackageActionRequested, Actor: actor, At: at})
	return nil
}

func (m *memStore) Get(_ context.Context, id string) (domain.PythonPackageApproval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return r, domain.ErrPythonPackageNotFound
	}
	return r, nil
}

func (m *memStore) filter(f func(domain.PythonPackageApproval) bool) []domain.PythonPackageApproval {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.PythonPackageApproval
	for _, r := range m.recs {
		if f(r) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return m.order[out[i].ID] > m.order[out[j].ID] })
	return out
}

func (m *memStore) ForTarget(_ context.Context, target string) ([]domain.PythonPackageApproval, error) {
	return m.filter(func(r domain.PythonPackageApproval) bool { return r.Target == target }), nil
}

func (m *memStore) ForPackage(_ context.Context, name, version string) ([]domain.PythonPackageApproval, error) {
	n := domain.NormalizePythonName(name)
	return m.filter(func(r domain.PythonPackageApproval) bool { return r.Name == n && r.Version == version }), nil
}

func (m *memStore) List(_ context.Context) ([]domain.PythonPackageApproval, error) {
	return m.filter(func(domain.PythonPackageApproval) bool { return true }), nil
}

func (m *memStore) Remove(_ context.Context, id, by, detail, generation string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return domain.ErrPythonPackageNotFound
	}
	if r.State != domain.PythonPackageApproved {
		return fmt.Errorf("python package %s is %s, not approved", id, r.State)
	}
	r.State, r.DecidedBy, r.DecidedAt, r.Detail = domain.PythonPackageRemoved, by, at, detail
	m.recs[id] = r
	m.appendLocked(domain.PythonPackageEvent{ApprovalID: id, Target: r.Target, Name: r.Name, Version: r.Version,
		Action: domain.PythonPackageActionRemoved, Actor: by, Detail: detail, Generation: generation, At: at})
	return nil
}

func (m *memStore) AppendEvent(_ context.Context, e domain.PythonPackageEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.recs[e.ApprovalID]; !ok {
		return fmt.Errorf("no record %s", e.ApprovalID)
	}
	m.appendLocked(e)
	return nil
}

func (m *memStore) Events(_ context.Context, id string) ([]domain.PythonPackageEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.PythonPackageEvent(nil), m.events[id]...), nil
}

func (m *memStore) actions(id string) []string {
	evs, _ := m.Events(context.Background(), id)
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Action
	}
	return out
}
