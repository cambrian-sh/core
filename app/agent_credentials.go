package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// Stored agent credentials (ADR-0130 D1, narrowed by ADR-0157 D4.5).
//
// Two credential kinds authenticate on the agent plane:
//
//   - every agent the KERNEL spawns presents a per-instance token minted at its
//     spawn and held only in kernel memory (authz.InstanceTokens, owned by the
//     agent manager). It has NO stored secret: the verifier refuses the stored
//     path for it, nothing mints one for it, and the boot retires any that an
//     older kernel or `setup` left behind (retireKernelSpawnedAgentCredentials,
//     the ADR-0157 Q4 migration);
//   - an agent the kernel does NOT spawn — a registered remote (A2A) agent, or
//     an external SDK agent started by hand — authenticates with the named
//     secret `agent:<id>:token` (authz.AgentTokenSecretName). Boot mints it for
//     registered agents of that kind that have none; an operator stores one for
//     an unregistered external agent through the named-secret seam.
//
// No value ever leaves the store except onto the wire. Nothing logs it; the boot
// lines name agents, not secrets.

// agentTokenBytes is the credential's entropy: 32 bytes, hex-encoded, the same
// size the MCP endpoint's client tokens are issued at.
const agentTokenBytes = 32

// mintAgentToken returns a fresh random credential.
func mintAgentToken() (string, error) {
	var b [agentTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("agent credential: entropy: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// agentCredentialStore is what minting and retiring need from the ADR-0101
// store: presence (never a read-back), an encrypted write and a delete.
// *storage.BoltConfigStore satisfies it through config.SecretStore.
type agentCredentialStore interface {
	Configured(name string) bool
	SetSecret(name, value string) error
	ClearSecret(name string) error
}

// mintMissingAgentCredentials stores a credential for every agent id that has
// none, returning the ids it minted for. Idempotent: an existing credential is
// never rotated here — rotation is an operator act, and silently re-minting at
// boot would invalidate every external copy of the token.
func mintMissingAgentCredentials(store agentCredentialStore, agentIDs []string) ([]string, error) {
	if store == nil {
		return nil, errConfigStoreOff
	}
	var minted []string
	for _, id := range agentIDs {
		if id == "" {
			continue
		}
		name := authz.AgentTokenSecretName(id)
		if store.Configured(name) {
			continue
		}
		tok, err := mintAgentToken()
		if err != nil {
			return minted, err
		}
		if err := store.SetSecret(name, tok); err != nil {
			return minted, fmt.Errorf("agent credential for %s: %w", id, err)
		}
		minted = append(minted, id)
	}
	sort.Strings(minted)
	return minted, nil
}

// retireKernelSpawnedAgentCredentials is the ADR-0157 Q4 migration (owner
// answer: delete, not keep behind a flag) on the ADR-0101 store,
// configs/config.db.
//
// cambrian:destructive — it DELETES the stored `agent:<id>:token` of every
// agent id the kernel spawns. Those secrets authenticate nothing any more (the
// verifier refuses the stored path for a kernel-spawned agent), and keeping
// them would leave usable-looking credentials for a sandbox escape to find.
// It is idempotent: an absent secret is skipped, so it runs on every boot and
// also removes a secret a later `setup` of an older release writes back. The
// store is not snapshotted here: operators take `orchestrator backup run`
// (which copies config.db) before upgrading, as for any destructive migration.
//
// Returns the ids whose secret it deleted.
func retireKernelSpawnedAgentCredentials(store agentCredentialStore, agentIDs []string) ([]string, error) {
	if store == nil {
		return nil, errConfigStoreOff
	}
	var retired []string
	for _, id := range agentIDs {
		if id == "" {
			continue
		}
		name := authz.AgentTokenSecretName(id)
		if !store.Configured(name) {
			continue
		}
		if err := store.ClearSecret(name); err != nil {
			return retired, fmt.Errorf("retire agent credential for %s: %w", id, err)
		}
		retired = append(retired, id)
	}
	sort.Strings(retired)
	return retired, nil
}

// ensureAgentCredentials is the boot-time half: registered agents the kernel
// does not spawn get a stored credential if they lack one; kernel-spawned
// agents have theirs retired. Logged, never fatal — a kernel whose store is
// off still boots.
func ensureAgentCredentials(ctx context.Context, store agentCredentialStore, registry domain.AgentRegistry) {
	if store == nil || registry == nil {
		return
	}
	defs, err := registry.GetAllAgents(ctx)
	if err != nil {
		slog.Warn("ADR-0130: could not list agents to provision credentials", "err", err)
		return
	}
	var external, spawned []string
	for _, d := range defs {
		if d.SpawnedByKernel() {
			spawned = append(spawned, d.ID)
		} else {
			external = append(external, d.ID)
		}
	}
	retired, err := retireKernelSpawnedAgentCredentials(store, spawned)
	if err != nil {
		slog.Warn("ADR-0157: retiring kernel-spawned agents' stored credentials stopped early",
			"retired", retired, "err", err)
	}
	if len(retired) > 0 {
		slog.Warn("ADR-0157 Q4: deleted the stored credentials of kernel-spawned agents (destructive, one-way)",
			"agents", retired, "secret", "agent:<id>:token",
			"why", "kernel-spawned agents authenticate only with per-instance tokens minted at each spawn")
	}
	minted, err := mintMissingAgentCredentials(store, external)
	if err != nil {
		slog.Warn("ADR-0130: agent credential provisioning stopped early", "minted", minted, "err", err)
	}
	if len(minted) > 0 {
		slog.Info("ADR-0130: minted credentials for agents the kernel does not spawn",
			"agents", minted, "secret", "agent:<id>:token")
	}
}

// kernelSpawnedAgent is the verifier's ADR-0157 D4.5 predicate: does the kernel
// spawn agentID itself? Read from the registry on every call, so an agent
// registered while the kernel runs is covered at once. It fails CLOSED: when
// the registry cannot be read it answers true with an ErrAuthUnavailable error,
// which refuses the stored-secret path as retryable Unavailable (ENG-2120) — an
// external agent is refused for that request rather than a kernel-spawned one
// admitted on a stored secret. Only the stored-secret path
// asks; a live instance token never reaches it.
func kernelSpawnedAgent(reg domain.AgentLister) func(ctx context.Context, agentID string) (bool, error) {
	return func(ctx context.Context, agentID string) (bool, error) {
		if reg == nil {
			return true, nil
		}
		defs, err := reg.GetAllAgents(ctx)
		if err != nil {
			// Cannot tell: the verifier refuses the stored credential as retryable
			// Unavailable, not as a wrong token (ENG-2120). Never accepted.
			slog.WarnContext(ctx, "ADR-0157: agent registry unreadable; refusing the stored agent credential",
				"agent_id", agentID, "err", err)
			return true, fmt.Errorf("%w: agent registry: %w", domain.ErrAuthUnavailable, err)
		}
		for _, d := range defs {
			if d.ID == agentID {
				return d.SpawnedByKernel(), nil
			}
		}
		return false, nil
	}
}
