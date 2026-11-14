package app

import (
	"context"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/storage"
)

// Where an MCP server's SHARED credential is written, cleared and reported
// (ADR-0141 phase 2 + phase 5).
//
// One place per deployment, never two:
//   - OSS (no account provider): the ADR-0101 store, under the consumer-named
//     secrets mcp:<id>:token and mcp:<id>:env:<NAME> — exactly as before.
//   - premium with the accounts plugin: the provider's ORG ACCOUNTS
//     (domain.OrgCredentialWriter). SetMCPServerToken, SetIntegrationSecret
//     and InstallIntegration's secrets rotate the account bound to the server
//     slot (creating and binding one when none is), and the consumer-named
//     store entries are neither written nor read (mcp.SetOrgCredentials).
//
// The operator RPCs and their outcomes are unchanged, so no console change is
// needed for either deployment shape.

// sharedCredActor attributes provider-side audit entries written through the
// legacy RPCs; the operator RPC itself is audited with the caller.
const sharedCredActor = "operator-console"

// orgCredentialWriter is the installed provider's org-credential writer, or nil.
func orgCredentialWriter() domain.OrgCredentialWriter {
	if w, ok := mcp.OrgCredentials().(domain.OrgCredentialWriter); ok {
		return w
	}
	return nil
}

// mcpSlotOf derives the credential slot from a consumer-named secret
// (mcp:<id>:token → "token", mcp:<id>:env:<NAME> → "env:<NAME>").
func mcpSlotOf(serverID, secretName string) string {
	return strings.TrimPrefix(secretName, "mcp:"+serverID+":")
}

func mcpOrgRequest(serverID, secretName string) domain.CredentialRequest {
	return domain.CredentialRequest{ServerID: serverID, Purpose: domain.CredentialPurposeMCP, Slot: mcpSlotOf(serverID, secretName)}
}

// writeSharedMCPCredential stores one shared credential of one server. ctx is
// the operator RPC's (AGENTS.md §2.7): the provider write inherits its
// deadline, cancellation, principal and trace.
func writeSharedMCPCredential(ctx context.Context, store *storage.BoltConfigStore, serverID, secretName, value string) error {
	if w := orgCredentialWriter(); w != nil {
		return w.SetOrgCredential(ctx, mcpOrgRequest(serverID, secretName), value, sharedCredActor)
	}
	return store.SetSecret(secretName, value)
}

// clearSharedMCPCredential removes one shared credential of one server (with
// accounts: unbinds the slot; the account is kept for its other consumers).
func clearSharedMCPCredential(ctx context.Context, store *storage.BoltConfigStore, serverID, secretName string) error {
	if w := orgCredentialWriter(); w != nil {
		if _, err := w.ClearOrgCredential(ctx, mcpOrgRequest(serverID, secretName), sharedCredActor); err != nil {
			return err
		}
		// The pre-migration consumer-named copy goes too (best effort): nothing
		// reads it any more, but a cleared credential should not linger as a
		// stale secret in the store.
		if store != nil {
			_ = store.ClearSecret(secretName) // best effort: the binding is already gone
		}
		return nil
	}
	return store.ClearSecret(secretName)
}

// sharedMCPCredentialStatus reports whether a shared credential is stored, and
// when it was last written. Never the value.
func sharedMCPCredentialStatus(store *storage.BoltConfigStore, serverID, secretName string) (bool, time.Time) {
	if w := orgCredentialWriter(); w != nil {
		ok, _, at := w.OrgCredentialStatus(mcpOrgRequest(serverID, secretName))
		return ok, at
	}
	if store == nil || !store.Configured(secretName) {
		return false, time.Time{}
	}
	return true, store.SecretUpdatedAt(secretName)
}
