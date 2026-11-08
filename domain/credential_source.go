package domain

import (
	"context"
	"errors"
	"time"
)

// The outbound-credential port (ADR-0141, which supersedes the storage and
// RPC model of ADR-0140 while keeping its per-call selection).
//
// An ACCOUNT is an outbound credential to an external system, owned by the
// organisation or by one principal. Accounts, their grants, their sign-in
// flows and their refresh live in PREMIUM (cambrian-premium/accounts); core
// only CONSUMES credential material through this port, so the OSS kernel stays
// complete without them:
//
//   - a server whose credential scope is "shared" (every server unless its
//     config says otherwise) is served by core's own config-stored credential
//     (mcp:<id>:token, mcp:<id>:env:<NAME>, internal/infrastructure/mcp
//     secrets.go) exactly as before — that resolver is the OSS default and
//     needs no CredentialSource at all;
//   - a server whose scope is "per_user" or "either" asks the installed
//     CredentialSource for the RUN principal's credential on every call
//     (RunPrincipalFromContext). With no source installed (an OSS build, or
//     premium's accounts plugin disabled) the answer is ErrCredentialNotConnected
//     and the call fails closed — never another principal's credential and
//     never the shared one unless the server's policy says so.
//
// The port is deliberately small: who may hold which account, how it was
// obtained and how it is refreshed are the provider's business. The tool
// adapter only needs "give me the token for (server, principal, purpose)".

// ErrCredentialNotConnected: the principal holds no account usable for this
// server and purpose. The caller turns it into an actionable "connect your
// account" error; it never falls back to another credential unless the
// server's policy explicitly allows the shared one (scope "either" +
// shared_fallback).
var ErrCredentialNotConnected = errors.New("account not connected for this principal")

// ErrCredentialReconnectNeeded: the principal connected once, but the account
// can no longer be refreshed (revoked upstream, refresh token expired, consent
// withdrawn). The person must sign in again. Never a reason to fall back.
var ErrCredentialReconnectNeeded = errors.New("account must be reconnected")

// ErrCredentialRevoked: the consumer is bound to an account an operator
// revoked. Every bound consumer fails closed on its next use (ADR-0141 D2:
// account_revoked); never a reason to fall back to another credential.
var ErrCredentialRevoked = errors.New("account was revoked")

// ErrCredentialPendingConfirmation: the principal signed in, but the account
// identity could not be matched to a known identity of theirs, so it waits for
// the person to confirm it (ADR-0141 owner position 4). Unusable until then.
var ErrCredentialPendingConfirmation = errors.New("account is waiting for its owner to confirm it")

// ErrCredentialNotPermitted: the consumer named an account it may not use —
// another principal's personal account, or an org account from a personal
// consumer the policy forbids. A refusal, never a fallback.
var ErrCredentialNotPermitted = errors.New("this consumer may not use that account")

// ErrCredentialInsufficientScope: the consumer is bound to a usable account,
// but the scopes the account was GRANTED do not cover the scopes the consumer
// declares (ADR-0141 grant scope enforcement). The account needs more
// permission (incremental consent: a re-sign-in or re-install asking for the
// missing scopes). A refusal, never a fallback to another credential; the
// wrapping error names the missing scopes (never credential material).
var ErrCredentialInsufficientScope = errors.New("the account needs more permission for this consumer")

// Credential purposes.
const (
	// CredentialPurposeMCP is a credential requested by the MCP tool adapter
	// (an agent acting with a person's or the organisation's account).
	CredentialPurposeMCP = "mcp"
	// CredentialPurposeIngress is an ingress's OUTBOUND credential (a poller,
	// websocket or backfill authenticating to the source it pulls from;
	// ADR-0141 phase 3). Inbound secrets (webhook signing, upload
	// capabilities) are never accounts and never asked for here.
	CredentialPurposeIngress = "ingress"
)

// Account references a consumer may name in its own definition
// (CredentialRequest.AccountID).
const (
	// AccountRefOwner prefixes "owner:<provider>": the consumer OWNER's
	// personal account at that provider (an ingress authored by or for a
	// person: personal ingest, ADR-0141 D10). The provider resolves it to
	// PrincipalID's own account and nobody else's, and refuses it when
	// PrincipalID is empty (an org consumer).
	AccountRefOwner = "owner"
)

// Slots of an MCP server's shared credential.
const (
	// SlotToken is an http/sse server's static bearer / header credential.
	SlotToken = "token"
	// SlotEnvPrefix prefixes a stdio server's secret_env variable name
	// ("env:POSTGRES_PASSWORD").
	SlotEnvPrefix = "env:"
)

// CredentialRequest names the credential a consumer needs.
type CredentialRequest struct {
	// ServerID is the consumer's id: an MCP server id (Purpose mcp) or an
	// ingress id (Purpose ingress).
	ServerID string
	// PrincipalID is the principal the work is FOR, never a value read from a
	// request payload or model output: the run principal of an MCP call, the
	// OWNER fixed on a personal ingress at authoring. "" asks for the
	// consumer's ORGANISATION credential (a shared MCP server, an org ingress).
	PrincipalID string
	// Purpose is what the credential will be used for (CredentialPurposeMCP,
	// CredentialPurposeIngress).
	Purpose string
	// Slot names which of the consumer's credentials (SlotToken,
	// SlotEnvPrefix+NAME, an ingress binding name). "" ⇒ SlotToken.
	Slot string
	// AccountID is the account the consumer's own definition names (an
	// ingress spec's `account:`), or AccountRefOwner. "" ⇒ whatever account
	// the provider has bound to (Purpose, ServerID, Slot).
	AccountID string
	// Scopes are the provider scopes this consumer DECLARES it needs (an
	// ingress binding's `scopes`). The provider refuses with
	// ErrCredentialInsufficientScope when the account's granted scopes do not
	// cover them. For Purpose mcp the provider also reads the server's own
	// declared scopes (ServerCredentialModel.Scopes); nil adds none.
	Scopes []string
}

// EffectiveSlot is Slot with its default applied.
func (r CredentialRequest) EffectiveSlot() string {
	if r.Slot == "" {
		return SlotToken
	}
	return r.Slot
}

// OrgCredentialWriter is implemented by a CredentialSource that also OWNS the
// organisation's credentials of consumers (ADR-0141 phase 2: premium's org
// accounts). When the installed source implements it, the kernel's existing
// write paths for a consumer's shared credential (SetMCPServerToken,
// SetIntegrationSecret, InstallIntegration's secrets, an ingress's outbound
// credential) write THROUGH it — to the bound org account, creating and
// binding one when none is bound — instead of to a consumer-named secret, so
// there is exactly one place an org credential lives. With no such source
// (OSS) those paths write the ADR-0101 store exactly as before.
type OrgCredentialWriter interface {
	// SetOrgCredential stores value as the org credential of (Purpose,
	// ServerID, Slot): it rotates the bound account, or creates and binds a
	// static-token account. actor is the authenticated principal, for audit.
	SetOrgCredential(ctx context.Context, req CredentialRequest, value, actor string) error
	// ClearOrgCredential unbinds the consumer slot from its account (the
	// account itself is kept: other consumers may share it). existed reports
	// whether a binding was removed.
	ClearOrgCredential(ctx context.Context, req CredentialRequest, actor string) (existed bool, err error)
	// ImportOrgCredential is the one-shot migration of a legacy
	// consumer-named secret (legacyName, e.g. mcp:<id>:token or
	// ingress:<id>:<name>) into an org account bound to the slot. Idempotent:
	// a slot already bound is left alone (created=false), so a re-run, or an
	// operator change made since, always wins. Audited without the value.
	ImportOrgCredential(ctx context.Context, req CredentialRequest, value, legacyName string) (created bool, err error)
	// OrgCredentialStatus reports whether a usable org credential is bound,
	// with its last four characters and last write time. Never the value.
	OrgCredentialStatus(req CredentialRequest) (configured bool, lastFour string, updatedAt time.Time)
}

// PrincipalIdentities answers "which external identities does this principal
// hold?" (emails, provider subjects) for the account-must-match-identity rule
// (ADR-0141 owner position 4). Implemented by the premium policy plane's
// principal directory (identity bindings, party identities). An empty answer
// means "none known", which is what sends a sign-in to confirmation.
type PrincipalIdentities interface {
	ExternalIdentities(ctx context.Context, principalID string) ([]string, error)
}

// Credential is usable credential material and when it stops being usable.
// The token is a secret: it must never be logged, returned on any read RPC,
// put in an error message, an audit entry or the operator feed.
type Credential struct {
	Token  string
	Expiry time.Time // zero ⇒ the provider declared none
}

// CredentialSource resolves credential material for one consumer call.
type CredentialSource interface {
	// Credential returns material valid for at least the provider's refresh
	// skew, refreshing it first when needed. Errors are the sentinels above,
	// or a transient error the caller reports without changing state.
	Credential(ctx context.Context, req CredentialRequest) (Credential, error)
	// ConnectedPrincipals lists principals holding a usable credential for the
	// server — used only to bring a per-user server up far enough to discover
	// its tool list, never to serve a call.
	ConnectedPrincipals(serverID string) []string
}

// Credential scopes of a consumer (ADR-0140 D1).
const (
	CredentialScopeShared  = "shared"
	CredentialScopePerUser = "per_user"
	CredentialScopeEither  = "either"
)

// ServerCredentialModel is what a consumer declares about the credential it
// needs (ADR-0140 D1, kept by ADR-0141): read by a CredentialSource provider
// from the LIVE server list so a console change is seen by the next sign-in.
// It carries no credential material.
type ServerCredentialModel struct {
	ServerID    string
	DisplayName string
	// Scope is CredentialScopeShared, CredentialScopePerUser or CredentialScopeEither.
	Scope string
	// Provider is the OAuth provider id an account for this server comes from.
	Provider string
	// Scopes are the provider scopes the server needs (incremental consent:
	// a consumer asks only for what it uses).
	Scopes []string
	// CatalogID is the integration-catalog entry the server was installed
	// from ("" for a hand-written server), so a provider can group servers
	// into external systems by data (ADR-0141 D5).
	CatalogID string
}

// SealedRecordStore is a namespaced, encrypted key-value store over the
// ADR-0101 config store: values are sealed with the store key on write and
// opened on read. A provider owns the record shape; the store knows nothing
// about it. Keys (never values) can be listed, because an account store must
// enumerate its own records — the namespace confines that to the provider's
// own records, unlike the named-secret seam, which never enumerates.
type SealedRecordStore interface {
	PutSealed(key string, plaintext []byte) error
	GetSealed(key string) (plaintext []byte, ok bool, err error)
	DeleteSealed(key string) (existed bool, err error)
	SealedKeys() ([]string, error)
}
