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

// Agent credential provisioning (ADR-0130 D1, the "first-boot flow must mint
// them" consequence).
//
// Every agent the kernel spawns authenticates to the agent plane with the
// named secret `agent:<id>:token` (authz.AgentTokenSecretName). Three parties
// touch that secret and all three go through this file or the seam it uses:
//
//   - `cambrian-orchestrator setup` (ADR-0122) mints a secret for every agent
//     it materialised, so a fresh install's agents can authenticate on the
//     first boot (setupAgentCredentials, app/setup_agent_credentials.go);
//   - the kernel mints any that are still missing at boot (ensureAgentCredentials
//     from Run) — an agent dropped into agents_dir after setup, or a store
//     that was reset, must not leave that agent permanently refused;
//   - the agent manager reads the value at spawn and places it in the agent's
//     process environment as CAMBRIAN_AGENT_TOKEN (agentCredentialEnv), the
//     one credential SEC-01's deny-by-default environment lets through.
//
// The value never leaves the store except into the spawned process's
// environment and the wire. Nothing logs it; the boot line names the agents,
// not the secrets.

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

// agentCredentialStore is what minting needs from the ADR-0101 store: presence
// (never a read-back) and an encrypted write. *storage.BoltConfigStore
// satisfies it through config.SecretStore.
type agentCredentialStore interface {
	Configured(name string) bool
	SetSecret(name, value string) error
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

// ensureAgentCredentials is the boot-time half: every registered agent gets a
// credential if the store is on. Logged, never fatal — a kernel whose store is
// off still boots, and the ADR-0130 boot line already said no agent can
// authenticate in that configuration.
func ensureAgentCredentials(ctx context.Context, store agentCredentialStore, registry domain.AgentRegistry) {
	if store == nil || registry == nil {
		return
	}
	defs, err := registry.GetAllAgents(ctx)
	if err != nil {
		slog.Warn("ADR-0130: could not list agents to provision credentials", "err", err)
		return
	}
	ids := make([]string, 0, len(defs))
	for _, d := range defs {
		ids = append(ids, d.ID)
	}
	minted, err := mintMissingAgentCredentials(store, ids)
	if err != nil {
		slog.Warn("ADR-0130: agent credential provisioning stopped early", "minted", minted, "err", err)
	}
	if len(minted) > 0 {
		slog.Info("ADR-0130: minted agent credentials", "agents", minted, "secret", "agent:<id>:token")
	}
}

// agentCredentialEnv is what the agent manager asks at spawn: the agent's
// credential through the late-bound named-secret seam, so a rotation made
// while the kernel runs reaches the next spawn without a restart. ok=false
// (store off, never minted) spawns the agent WITHOUT a credential, which on a
// strict plane means its first RPC is refused with a message that names the
// fix — better than a kernel that will not start its agents at all.
func agentCredentialEnv(agentID string) (string, bool) {
	return resolveNamedSecret(authz.AgentTokenSecretName(agentID))
}
