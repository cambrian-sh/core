package authz

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// ─────────────────────────────────────────────────────────────────────────────
// Per-instance agent tokens (ADR-0157 D4).
//
// Every agent process the kernel spawns gets a fresh random credential, bound
// to (agent id, instance id) and held ONLY in kernel memory — never in the
// ADR-0101 store, never on argv. One agent id can run several instances (the
// chat pool, dedicated subtask instances), so the token is per instance: a
// restart or eviction revokes that instance's token and nothing else, and a
// sibling instance keeps its own. Revocation is a map delete, so the next RPC
// carrying the old token is Unauthenticated at once.
//
// There is no TTL and no refresh: the token lives exactly as long as the
// process (ADR-0157 D4.4, owner's choice over JWT/SVID with a refresh protocol).
//
// Lookups are keyed by SHA-256 of the presented token, so finding a token never
// compares secret bytes with an early-exit compare: the map hashes a digest an
// attacker cannot steer toward a stored token without knowing it.
// ─────────────────────────────────────────────────────────────────────────────

// instanceTokenBytes is the credential's entropy (ADR-0157 D4.1: 32 bytes,
// crypto/rand), hex-encoded — the same size as the stored agent secrets.
const instanceTokenBytes = 32

// InstanceTokens is the kernel's in-memory registry of live per-instance agent
// tokens. The agent manager mints and revokes; the agent-plane verifier and the
// SignalStream gate look up. Safe for concurrent use; the zero value is not
// usable — build it with NewInstanceTokens.
type InstanceTokens struct {
	mu         sync.RWMutex
	byDigest   map[[sha256.Size]byte]InstanceIdentity
	byInstance map[string][sha256.Size]byte
}

// InstanceIdentity is who a live instance token was issued to.
type InstanceIdentity struct {
	AgentID    string
	InstanceID string
}

// NewInstanceTokens returns an empty registry.
func NewInstanceTokens() *InstanceTokens {
	return &InstanceTokens{
		byDigest:   make(map[[sha256.Size]byte]InstanceIdentity),
		byInstance: make(map[string][sha256.Size]byte),
	}
}

// Mint issues a fresh token for one instance and returns it. A token the same
// instance already held is revoked first, so an instance never has two.
func (t *InstanceTokens) Mint(agentID, instanceID string) (string, error) {
	if agentID == "" || instanceID == "" {
		return "", fmt.Errorf("instance token: agent id and instance id are required")
	}
	var b [instanceTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("instance token: entropy: %w", err)
	}
	tok := hex.EncodeToString(b[:])
	d := sha256.Sum256([]byte(tok))
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.byInstance[instanceID]; ok {
		delete(t.byDigest, old)
	}
	t.byDigest[d] = InstanceIdentity{AgentID: agentID, InstanceID: instanceID}
	t.byInstance[instanceID] = d
	return tok, nil
}

// Revoke deletes an instance's token. Revoking an instance with no token is a
// no-op, so every teardown path may call it without coordinating.
func (t *InstanceTokens) Revoke(instanceID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d, ok := t.byInstance[instanceID]; ok {
		delete(t.byDigest, d)
		delete(t.byInstance, instanceID)
	}
}

// Lookup returns the identity a live token was issued to. An empty, unknown or
// revoked token is ok=false — callers do not need to tell "never issued" from
// "revoked", and must not (that would tell a caller its stolen token once
// worked).
func (t *InstanceTokens) Lookup(token string) (InstanceIdentity, bool) {
	if t == nil || token == "" {
		return InstanceIdentity{}, false
	}
	d := sha256.Sum256([]byte(token))
	t.mu.RLock()
	defer t.mu.RUnlock()
	id, ok := t.byDigest[d]
	return id, ok
}

// Len reports how many live tokens exist (tests and boot diagnostics).
func (t *InstanceTokens) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.byDigest)
}
