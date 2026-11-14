package domain

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// The durable action record (ADR-0154; owner decision 2026-11-14, ENG-812 +
// ENG-1428): ONE record of every side-effecting action, keyed deterministically
// (AGENTS.md §2.6), walking a forward-only lifecycle. Core owns the record and
// the key; premium's L4 executor drives approval routing and write-back
// confirmation over it.

// ActionKeyMetaKey is the MCP request _meta key carrying a side-effecting call's
// deterministic action key to the target (ADR-0154 D4). Reverse-DNS prefixed,
// as the MCP spec asks of _meta keys. A server that knows it can de-duplicate a
// retried action; one that does not ignores it.
const ActionKeyMetaKey = "sh.cambrian/action-key"

// ActionState is a step of the action lifecycle (ADR-0154 D3).
type ActionState string

const (
	// ActionStateProposed: the action is known but has not been released to run.
	ActionStateProposed ActionState = "proposed"
	// ActionStateApproved: an approver released it.
	ActionStateApproved ActionState = "approved"
	// ActionStateDispatched: sent to the target. A failed or ambiguous dispatch
	// (timeout, transport error) STAYS here with the error recorded: the outcome
	// is unknown, a retry re-dispatches the same record, and confirmation can
	// query the target before retrying.
	ActionStateDispatched ActionState = "dispatched"
	// ActionStateAccepted: the target answered success (2xx) — accepted, not yet
	// confirmed to have taken effect.
	ActionStateAccepted ActionState = "accepted"
	// ActionStateConfirmed: write-back confirmation found the effect in the target.
	ActionStateConfirmed ActionState = "confirmed"
	// ActionStateNotConfirmed: confirmation established the effect did not happen.
	ActionStateNotConfirmed ActionState = "not_confirmed"
)

// actionNext lists the states each state may move to. Forward only.
var actionNext = map[ActionState][]ActionState{
	ActionStateProposed:   {ActionStateApproved, ActionStateDispatched},
	ActionStateApproved:   {ActionStateDispatched},
	ActionStateDispatched: {ActionStateAccepted, ActionStateNotConfirmed},
	ActionStateAccepted:   {ActionStateConfirmed, ActionStateNotConfirmed},
}

// Valid reports whether s is a known state.
func (s ActionState) Valid() bool {
	switch s {
	case ActionStateProposed, ActionStateApproved, ActionStateDispatched, ActionStateAccepted, ActionStateConfirmed, ActionStateNotConfirmed:
		return true
	}
	return false
}

// CanAdvanceTo reports whether the lifecycle allows s → to. Staying in the same
// state is allowed (an idempotent re-write, e.g. a re-dispatch).
func (s ActionState) CanAdvanceTo(to ActionState) bool {
	if s == to {
		return true
	}
	for _, n := range actionNext[s] {
		if n == to {
			return true
		}
	}
	return false
}

// Settled reports whether the action has an answer a retry may reuse instead of
// running again: the target accepted it (or confirmation settled it).
func (s ActionState) Settled() bool {
	return s == ActionStateAccepted || s == ActionStateConfirmed || s == ActionStateNotConfirmed
}

// DurableAction is one side-effecting action.
type DurableAction struct {
	// Key is the deterministic action key (for a plan-step call the ADR-0145 D9
	// IdempotencyKey). The record's identity.
	Key       string
	Tool      string
	RunID     RunID
	StepIndex int
	// Principal is the person (run principal) the action was done for; "" when
	// none was resolved.
	Principal string
	SessionID SessionID
	// ArgsHash is the hash of the canonical arguments (never the arguments).
	ArgsHash string
	State    ActionState
	// Result is the tool's response once the target accepted the action, kept
	// so a retry after a restart is answered without running again.
	Result *ToolCallResponse
	// Error is the last dispatch error (the action stays dispatched).
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ActionUpdate is what an Advance writes besides the state.
type ActionUpdate struct {
	Result *ToolCallResponse
	Error  string
}

// Sentinels callers distinguish.
var (
	ErrActionRecordNotFound = errors.New("action record not found")
	ErrActionTransition     = errors.New("action record: transition not allowed")
)

// ActionRecordStore is the durable action record (ADR-0154 D1).
type ActionRecordStore interface {
	// Record inserts rec under rec.Key when no record has that key; otherwise it
	// returns the existing record unchanged (created=false). Idempotent.
	Record(ctx context.Context, rec DurableAction) (stored DurableAction, created bool, err error)
	// Advance moves the record to `to` when the lifecycle allows it
	// (ErrActionTransition otherwise; ErrActionRecordNotFound for an unknown key)
	// and writes the update. Advancing to the current state rewrites the update.
	Advance(ctx context.Context, key string, to ActionState, u ActionUpdate) (DurableAction, error)
	// Get returns the record by key (ErrActionRecordNotFound when absent).
	Get(ctx context.Context, key string) (DurableAction, error)
}

// MemActionRecordStore is the in-memory ActionRecordStore: the twin the
// database-less edge profile and tests run (lost on restart, like every
// in-memory store).
type MemActionRecordStore struct {
	mu   sync.Mutex
	now  func() time.Time
	recs map[string]DurableAction
}

// NewMemActionRecordStore returns an empty in-memory store.
func NewMemActionRecordStore() *MemActionRecordStore {
	return &MemActionRecordStore{now: time.Now, recs: map[string]DurableAction{}}
}

var _ ActionRecordStore = (*MemActionRecordStore)(nil)

func (m *MemActionRecordStore) Record(_ context.Context, rec DurableAction) (DurableAction, bool, error) {
	if rec.Key == "" {
		return DurableAction{}, false, errors.New("action record: key is required")
	}
	if rec.State == "" {
		rec.State = ActionStateProposed
	}
	if !rec.State.Valid() {
		return DurableAction{}, false, fmt.Errorf("action record: unknown state %q", rec.State)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if got, ok := m.recs[rec.Key]; ok {
		return cloneActionRecord(got), false, nil
	}
	now := m.now().UTC()
	rec.CreatedAt, rec.UpdatedAt = now, now
	rec.Result = cloneResponse(rec.Result)
	m.recs[rec.Key] = rec
	return cloneActionRecord(rec), true, nil
}

func (m *MemActionRecordStore) Advance(_ context.Context, key string, to ActionState, u ActionUpdate) (DurableAction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.recs[key]
	if !ok {
		return DurableAction{}, ErrActionRecordNotFound
	}
	if !rec.State.CanAdvanceTo(to) {
		return cloneActionRecord(rec), fmt.Errorf("%w: %s → %s", ErrActionTransition, rec.State, to)
	}
	rec.State = to
	if u.Result != nil {
		rec.Result = cloneResponse(u.Result)
	}
	rec.Error = u.Error
	rec.UpdatedAt = m.now().UTC()
	m.recs[key] = rec
	return cloneActionRecord(rec), nil
}

func (m *MemActionRecordStore) Get(_ context.Context, key string) (DurableAction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.recs[key]
	if !ok {
		return DurableAction{}, ErrActionRecordNotFound
	}
	return cloneActionRecord(rec), nil
}

func cloneActionRecord(r DurableAction) DurableAction {
	r.Result = cloneResponse(r.Result)
	return r
}

func cloneResponse(r *ToolCallResponse) *ToolCallResponse {
	if r == nil {
		return nil
	}
	c := *r
	c.ResultJSON = append([]byte(nil), r.ResultJSON...)
	return &c
}
