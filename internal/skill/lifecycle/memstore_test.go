package lifecycle

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// memStore is the in-memory domain.SkillLifecycleStore the service tests run
// on. It mirrors the Postgres adapter's contract (PgSkillLifecycleStore), which
// the TestPg_ tests in internal/infrastructure/postgres hold to the same rules.
type memStore struct {
	mu        sync.Mutex
	proposals map[string]domain.SkillProposal
	skills    map[string]domain.SkillRecord
	versions  map[string]domain.SkillVersion // name\x00version
	ledger    []domain.SkillLedgerEntry
	uses      map[string]map[string]bool // run → skill → settled
}

func newMemStore() *memStore {
	return &memStore{
		proposals: map[string]domain.SkillProposal{}, skills: map[string]domain.SkillRecord{},
		versions: map[string]domain.SkillVersion{}, uses: map[string]map[string]bool{},
	}
}

var _ domain.SkillLifecycleStore = (*memStore)(nil)

func (m *memStore) InsertProposal(_ context.Context, p domain.SkillProposal) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.proposals[p.ID]; ok {
		return false, nil
	}
	m.proposals[p.ID] = p
	return true, nil
}

func (m *memStore) GetProposal(_ context.Context, id string) (domain.SkillProposal, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.proposals[id]
	return p, ok, nil
}

func (m *memStore) ListProposals(_ context.Context, f domain.SkillProposalFilter) ([]domain.SkillProposal, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []domain.SkillProposal
	for _, p := range m.proposals {
		if (f.State == "" || p.State == f.State) && (f.SkillName == "" || p.Name == f.SkillName) && (f.Proposer == "" || p.ProposerAgentID == f.Proposer) {
			all = append(all, p)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	return page(all, f.Limit, f.Offset), len(all), nil
}

func page[T any](all []T, limit, offset int) []T {
	if offset > len(all) {
		offset = len(all)
	}
	all = all[offset:]
	if limit > 0 && limit < len(all) {
		all = all[:limit]
	}
	return all
}

func (m *memStore) GetSkill(_ context.Context, name string) (domain.SkillRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.skills[name]
	return r, ok, nil
}

func (m *memStore) ListSkills(context.Context) ([]domain.SkillRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.SkillRecord, 0, len(m.skills))
	for _, r := range m.skills {
		out = append(out, r)
	}
	return out, nil
}

func (m *memStore) GetVersion(_ context.Context, name, version string) (domain.SkillVersion, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.versions[name+"\x00"+version]
	return v, ok, nil
}

func (m *memStore) CountVersions(_ context.Context, name string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, v := range m.versions {
		if v.Name == name {
			n++
		}
	}
	return n, nil
}

func (m *memStore) head(name string) int64 {
	var h int64
	for _, e := range m.ledger {
		if e.SkillName == name && e.Seq > h {
			h = e.Seq
		}
	}
	return h
}

func (m *memStore) LedgerHead(_ context.Context, name string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.head(name), nil
}

func (m *memStore) Commit(_ context.Context, c domain.SkillCommit) (domain.SkillLedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := c.Entry.SkillName
	if m.head(name) != c.ExpectSeq {
		return domain.SkillLedgerEntry{}, domain.ErrSkillLedgerConflict
	}
	if c.Entry.CommandID != "" {
		for _, e := range m.ledger {
			if e.CommandID == c.Entry.CommandID {
				return domain.SkillLedgerEntry{}, domain.ErrSkillLedgerConflict
			}
		}
	}
	e := c.Entry
	e.Seq = c.ExpectSeq + 1
	e.ID = domain.SkillLedgerEntryID(name, e.Seq)
	if c.NewVersion != nil {
		k := c.NewVersion.Name + "\x00" + c.NewVersion.Version
		if _, ok := m.versions[k]; !ok { // versions are immutable
			m.versions[k] = *c.NewVersion
		}
	}
	if c.Record != nil {
		r := *c.Record
		r.LedgerSeq, r.LedgerEntryID = e.Seq, e.ID
		// Usage telemetry is owned by RecordUse/RecordRunOutcome; a commit never
		// overwrites it with the caller's (possibly older) read.
		if old, ok := m.skills[r.Name]; ok {
			r.UseCount, r.SuccessCount, r.LastUsedAt = old.UseCount, old.SuccessCount, old.LastUsedAt
		}
		m.skills[r.Name] = r
	}
	if c.Proposal != nil {
		m.proposals[c.Proposal.ID] = *c.Proposal
	}
	m.ledger = append(m.ledger, e)
	return e, nil
}

func (m *memStore) ListLedger(_ context.Context, name string, limit, offset int) ([]domain.SkillLedgerEntry, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []domain.SkillLedgerEntry
	for i := len(m.ledger) - 1; i >= 0; i-- {
		if name == "" || m.ledger[i].SkillName == name {
			all = append(all, m.ledger[i])
		}
	}
	return page(all, limit, offset), len(all), nil
}

func (m *memStore) GetLedgerEntry(_ context.Context, id string) (domain.SkillLedgerEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.ledger {
		if e.ID == id {
			return e, true, nil
		}
	}
	return domain.SkillLedgerEntry{}, false, nil
}

func (m *memStore) LedgerByCommand(_ context.Context, cmd string) (domain.SkillLedgerEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.ledger {
		if e.CommandID == cmd {
			return e, true, nil
		}
	}
	return domain.SkillLedgerEntry{}, false, nil
}

func (m *memStore) RecordUse(_ context.Context, name, runID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.skills[name]
	if !ok {
		return nil
	}
	r.UseCount++
	r.LastUsedAt = at
	m.skills[name] = r
	if runID != "" {
		if m.uses[runID] == nil {
			m.uses[runID] = map[string]bool{}
		}
		if _, seen := m.uses[runID][name]; !seen {
			m.uses[runID][name] = false
		}
	}
	return nil
}

func (m *memStore) RecordRunOutcome(_ context.Context, runID string, success bool) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var credited []string
	for name, settled := range m.uses[runID] {
		if settled {
			continue
		}
		m.uses[runID][name] = true
		if success {
			r := m.skills[name]
			r.SuccessCount++
			m.skills[name] = r
			credited = append(credited, name)
		}
	}
	return credited, nil
}
