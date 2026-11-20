package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"sync"
)

// TurnCalls is the record of the side-effecting calls a chat turn has begun,
// and the source of their turn-scoped idempotency keys (ENG-1634, owner
// decision 2026-11-20: a chat turn whose worker is killed is re-driven ONCE,
// and only when the killed attempt began no side-effecting call; otherwise it
// fails loudly).
//
// The chat lane Tracks the turn's lease before it dispatches and Forgets it when
// the turn ends, so only turn leases are recorded and nothing outlives a turn.
// The tool executor calls Begin for every side-effecting call made on a tracked
// lease that is neither a subtask's (ADR-0150 keys those) nor a plan step's
// (ADR-0145 D9 keys those): the call is counted, and its key — the lease, the
// tool, the canonical arguments and the occurrence of that pair, the subtask
// recipe — is the action's identity on the wire (ADR-0154 D4) and its entry in
// the turn's idempotency store. A re-driven turn starts its occurrences again
// (Redrive), so an identical call it repeats is answered from the stored result
// instead of running twice.
type TurnCalls struct {
	mu    sync.Mutex
	turns map[string]*turnCallState
	idem  *IdempotencyCache
}

type turnCallState struct {
	// begun counts the side-effecting calls begun on the lease since it was
	// tracked (Redrive does not reset it: a re-drive's own calls add to it).
	begun int
	tools map[string]bool
	// occ counts each (tool, canonical args) pair in the current attempt.
	occ map[string]int
}

// NewTurnCalls returns an empty record whose turn calls are kept in idem
// (NewDurableIdempotencyCache over the action record, ADR-0154; nil ⇒ an
// in-memory cache with the default retention).
func NewTurnCalls(idem *IdempotencyCache) *TurnCalls {
	if idem == nil {
		idem = NewIdempotencyCache(0)
	}
	return &TurnCalls{turns: map[string]*turnCallState{}, idem: idem}
}

// Track starts recording the calls made on lease. Tracking an already tracked
// lease keeps what it recorded.
func (t *TurnCalls) Track(lease LeaseID) {
	if t == nil || lease == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.turns[string(lease)]; !ok {
		t.turns[string(lease)] = &turnCallState{tools: map[string]bool{}, occ: map[string]int{}}
	}
}

// Forget stops recording lease and drops what was recorded (the stored results
// stay in the idempotency store until its retention passes).
func (t *TurnCalls) Forget(lease LeaseID) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.turns, string(lease))
}

// Begin records a side-effecting call on lease and returns its turn-scoped key;
// ok=false when lease is not tracked (the call is not a chat turn's).
func (t *TurnCalls) Begin(lease LeaseID, tool string, args []byte) (key string, ok bool) {
	if t == nil || lease == "" {
		return "", false
	}
	canon := string(CanonicalJSON(args))
	t.mu.Lock()
	s, tracked := t.turns[string(lease)]
	if !tracked {
		t.mu.Unlock()
		return "", false
	}
	s.begun++
	s.tools[tool] = true
	sig := tool + "\x00" + canon
	s.occ[sig]++
	n := s.occ[sig]
	t.mu.Unlock()
	h := sha256.New()
	for _, p := range []string{"turn", string(lease), tool, canon, strconv.Itoa(n)} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "turn:" + hex.EncodeToString(h.Sum(nil)), true
}

// Begun reports how many side-effecting calls were begun on lease since it was
// tracked, and the tools they named (sorted).
func (t *TurnCalls) Begun(lease LeaseID) (int, []string) {
	if t == nil {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.turns[string(lease)]
	if !ok {
		return 0, nil
	}
	tools := make([]string, 0, len(s.tools))
	for name := range s.tools {
		tools = append(tools, name)
	}
	sort.Strings(tools)
	return s.begun, tools
}

// Redrive starts a new attempt of the turn on lease: occurrences count from one
// again, so the attempt's calls map onto the same keys as the killed attempt's.
func (t *TurnCalls) Redrive(lease LeaseID) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.turns[string(lease)]; ok {
		s.occ = map[string]int{}
	}
}

// Idempotency is the store of the turns' side-effecting results.
func (t *TurnCalls) Idempotency() *IdempotencyCache {
	if t == nil {
		return nil
	}
	return t.idem
}
