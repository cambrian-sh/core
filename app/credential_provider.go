package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// The outbound-credential provider seam (ADR-0141).
//
// Accounts — outbound credentials owned by the organisation or by a person,
// their sign-in flows, storage and refresh — are a PREMIUM capability
// (cambrian-premium/accounts, plugins/accounts). Core consumes them through
// one port, domain.CredentialSource, which the MCP connector asks for a
// per_user / either server's credential on every call. With no provider
// registered (OSS, or the premium plugin off) the connector has no source and
// per-user servers fail closed ("not connected"); shared servers are served
// by core's own config-stored credentials (mcp:<id>:token,
// mcp:<id>:env:<NAME>) exactly as before.
//
// The provider is built ONCE, by its factory, after the connector and the
// ADR-0101 store exist and before any MCP health loop or plugin lifecycle
// starts, and installed on the connector before Watch — so a per_user
// server's first discovery already sees it.

// CredentialRecordsNamespace is the sealed-record namespace handed to the
// credential provider. ADR-0140's records were migrated into it by the store
// (storage.LegacyAccountsNamespace), so the two must agree.
const CredentialRecordsNamespace = storage.LegacyAccountsNamespace

// CredentialServices is what the kernel gives a credential provider. Every
// field is set when the factory runs; none of them carries a credential the
// provider did not put there itself.
type CredentialServices struct {
	// Approvals is the kernel's addressed approval hub (ADR-0142 D11): a pending
	// account confirmation is raised there, addressed to its person. nil in OSS
	// tests.
	Approvals *domain.AddressedHub

	// ServerCredentialModel answers a server's credential model from the LIVE
	// server list (a console change is seen by the next sign-in). ok=false
	// for an unknown server; a server with no user_credential block is
	// CredentialScopeShared.
	ServerCredentialModel func(serverID string) (domain.ServerCredentialModel, bool)
	// PersonalServers lists the installed servers whose scope is per_user or
	// either, sorted by id — what a console offers "Connect" for.
	PersonalServers func() []domain.ServerCredentialModel
	// Servers lists EVERY installed server's credential model (any scope),
	// sorted by id — what a provider groups into external systems and reports
	// as an account's MCP consumers (ADR-0141 D5, ListSystems).
	Servers func() []domain.ServerCredentialModel
	// Records is the provider's sealed-record namespace of the ADR-0101 store.
	Records domain.SealedRecordStore
	// ResolveNamedSecret reads one named ADR-0101 secret (e.g. an OAuth
	// client id / secret), the same seam as KernelServices.ResolveNamedSecret.
	ResolveNamedSecret func(name string) (string, bool)
	// CredentialChanged tells the tool adapter a principal's account for a
	// server was connected (connected=true: retire the session holding the
	// old token; discover a never-connected server's tools) or removed.
	// Must not block.
	CredentialChanged func(serverID, principalID string, connected bool)
	// RecordAudit writes one entry to the operator audit store + feed, for
	// changes made outside an RPC (an OAuth callback, a dead refresh grant,
	// the boot migration and legacy cleanup). Before the operator plane
	// exists it queues the entry (bounded) and writes it when the plane is
	// bound; it never blocks boot.
	RecordAudit func(ctx context.Context, e domain.AuditEntry) error
	// RunMutation runs one operator-plane command with the OperatorConsole
	// mutation contract (command_id + reason, audited with the caller,
	// deduped). Unavailable before the operator plane exists.
	RunMutation func(ctx context.Context, commandID, reason, action, targetType, targetID string, apply func() error) (deduped bool, err error)
	// Caller is the authenticated operator-plane caller of a premium plane
	// RPC: principal id and whether it holds the Operator role.
	Caller func(ctx context.Context) (principal string, isOperator bool, ok bool)

	// StoreNamedSecret / ClearNamedSecret / NamedSecretStatus are the write
	// half of the named-secret seam (KernelServices'), for the provider's own
	// named secrets: the OAuth client registrations
	// oauth:<provider>:client_id|client_secret (ADR-0141 phase 2: their only
	// writer). NamedSecretUpdatedAt is the last write time (zero = unknown).
	StoreNamedSecret     func(name, value string) error
	ClearNamedSecret     func(name string) error
	NamedSecretStatus    func(name string) (configured bool, lastFour string)
	NamedSecretUpdatedAt func(name string) time.Time

	// SharedCredentialChanged tells the kernel the org account bound to a
	// SHARED server changed (rotated, revoked, rebound): the server is
	// reconnected so it presents the new credential. Must not block long.
	SharedCredentialChanged func(serverID string)

	// LegacySharedCredentials lists every consumer-named shared MCP credential
	// slot the kernel could hold (mcp:<id>:token, mcp:<id>:env:<NAME>) for the
	// installed servers, for the provider's one-shot boot migration into org
	// accounts (ADR-0141 owner position 1). Values are read one name at a time
	// through ResolveNamedSecret; this lists names, never values.
	LegacySharedCredentials func() []LegacySharedCredential

	// PrincipalIdentities answers a principal's known external identities
	// (emails, provider subjects) for the account-must-match-identity rule
	// (ADR-0141 owner position 4), from the premium policy plane's principal
	// directory when it implements domain.PrincipalIdentities. known=false
	// when no directory answers (the sign-in then needs confirmation).
	PrincipalIdentities func(ctx context.Context, principalID string) (ids []string, known bool, err error)
}

// LegacySharedCredential is one consumer-named shared credential slot.
type LegacySharedCredential struct {
	ServerID   string
	Slot       string // domain.SlotToken or domain.SlotEnvPrefix+NAME
	SecretName string // mcp:<id>:token or mcp:<id>:env:<NAME>
}

// NewCredentialSourceFunc builds the credential provider. A nil source with a
// nil error means "nothing to install" (the provider is configured off).
type NewCredentialSourceFunc func(CredentialServices) (domain.CredentialSource, error)

// SetCredentialSource installs the outbound-credential provider factory
// (ADR-0141). Tier-1 replace-one: two providers would be two answers to
// "whose token does this call carry", with no way to say which held.
func (r *Registry) SetCredentialSource(owner string, f NewCredentialSourceFunc) error {
	if f == nil {
		return nil
	}
	if r.credentialSource != nil {
		return fmt.Errorf("credential source already registered by plugin %q; %q cannot also own it", r.credentialOwner, owner)
	}
	r.credentialSource = f
	r.credentialOwner = owner
	return nil
}

// errOperatorPlaneNotReady: a plugin-plane mutation arrived before the
// operator service was wired (cannot happen through gRPC, which serves only
// after wiring; guards a direct call).
var errOperatorPlaneNotReady = errors.New("the operator plane is not ready")

// credentialSeams holds the late-bound operator hooks: the operator service
// is built after the connector, and the provider is built before it.
type credentialSeams struct {
	mu       sync.RWMutex
	audit    func(ctx context.Context, e domain.AuditEntry) error
	mutation func(ctx context.Context, commandID, reason, action, targetType, targetID string, apply func() error) (bool, error)
	// src is the built provider (nil until startKernelServices builds it, and
	// in OSS). Plugins reach it only through resolveCredential / orgWriter,
	// late-bound because plugins Build before the provider exists.
	src domain.CredentialSource
	// directory answers PrincipalIdentities (the premium principal
	// directory, registered by a plugin at Build).
	directory func() domain.PrincipalDirectory
	// bounce reconnects one shared MCP server (the operator wiring's
	// bounceMCPServer), late-bound like audit.
	bounce func(id string) bool
	// pending holds audit entries recorded before the operator plane was
	// bound, flushed by bind. The provider is built (and runs its boot
	// migration and legacy cleanup, ADR-0141 D11) BEFORE startKernelServices
	// wires the operator service, so without this every boot-time account
	// audit entry was logged and dropped. Bounded: past maxPendingAudits an
	// entry is logged and dropped as before.
	pending []pendingAudit
	// approvals is the ADR-0142 D11 addressed hub, handed to the provider.
	approvals *domain.AddressedHub
}

type pendingAudit struct {
	ctx context.Context
	e   domain.AuditEntry
}

// maxPendingAudits bounds the pre-bind queue (one entry per migrated or
// cleaned-up credential; a deployment has tens, not thousands).
const maxPendingAudits = 1024

func (c *credentialSeams) setBounce(f func(id string) bool) {
	c.mu.Lock()
	c.bounce = f
	c.mu.Unlock()
}

// sharedCredentialChanged reconnects a shared server whose org account
// changed, so the new credential is presented (credentials are injected at
// connect time). A no-op before the operator plane is wired.
func (c *credentialSeams) sharedCredentialChanged(serverID string) {
	c.mu.RLock()
	f := c.bounce
	c.mu.RUnlock()
	if f != nil {
		f(serverID)
	}
}

func (c *credentialSeams) setSource(src domain.CredentialSource) {
	c.mu.Lock()
	c.src = src
	c.mu.Unlock()
}

// resolveCredential is KernelServices.ResolveCredential: the installed
// provider's answer, or ErrCredentialNotConnected when there is none (OSS).
func (c *credentialSeams) resolveCredential(ctx context.Context, req domain.CredentialRequest) (domain.Credential, error) {
	c.mu.RLock()
	src := c.src
	c.mu.RUnlock()
	if src == nil {
		return domain.Credential{}, domain.ErrCredentialNotConnected
	}
	return src.Credential(ctx, req)
}

// orgWriter is KernelServices.OrgCredentials: the provider's org-credential
// writer, or nil when none is installed.
func (c *credentialSeams) orgWriter() domain.OrgCredentialWriter {
	c.mu.RLock()
	src := c.src
	c.mu.RUnlock()
	if w, ok := src.(domain.OrgCredentialWriter); ok {
		return w
	}
	return nil
}

// principalIdentities asks the registered principal directory, when it
// implements domain.PrincipalIdentities.
func (c *credentialSeams) principalIdentities(ctx context.Context, principalID string) ([]string, bool, error) {
	if c.directory == nil {
		return nil, false, nil
	}
	pi, ok := c.directory().(domain.PrincipalIdentities)
	if !ok {
		return nil, false, nil
	}
	ids, err := pi.ExternalIdentities(ctx, principalID)
	return ids, err == nil, err
}

func (c *credentialSeams) bind(svc *operator.Service) {
	c.bindAudit(svc.RecordAudit, svc.RunPluginMutation)
}

// bindAudit installs the operator sinks and flushes the entries recorded
// before they existed, in order.
func (c *credentialSeams) bindAudit(audit func(context.Context, domain.AuditEntry) error,
	mutation func(ctx context.Context, commandID, reason, action, targetType, targetID string, apply func() error) (bool, error)) {
	c.mu.Lock()
	c.audit, c.mutation = audit, mutation
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	for _, p := range pending {
		if err := audit(p.ctx, p.e); err != nil {
			slog.Warn("ADR-0141: a boot-time account audit entry could not be written", "action", p.e.ActionType, "target", p.e.TargetID, "err", err)
		}
	}
}

func (c *credentialSeams) recordAudit(ctx context.Context, e domain.AuditEntry) error {
	c.mu.Lock()
	fn := c.audit
	if fn == nil {
		// Before the operator plane is bound (boot): queue, flushed by bind.
		queued := len(c.pending) < maxPendingAudits
		if queued {
			c.pending = append(c.pending, pendingAudit{ctx: context.WithoutCancel(ctx), e: e})
		}
		c.mu.Unlock()
		if !queued {
			slog.Warn("ADR-0141: audit sink not wired yet and the boot queue is full; account change not audited", "action", e.ActionType, "target", e.TargetID)
		}
		return nil
	}
	c.mu.Unlock()
	return fn(context.WithoutCancel(ctx), e)
}

func (c *credentialSeams) runMutation(ctx context.Context, commandID, reason, action, targetType, targetID string, apply func() error) (bool, error) {
	c.mu.RLock()
	fn := c.mutation
	c.mu.RUnlock()
	if fn == nil {
		return false, errOperatorPlaneNotReady
	}
	return fn(ctx, commandID, reason, action, targetType, targetID, apply)
}

// serverCredentialModel converts a server's config into the port's model.
func serverCredentialModel(c config.MCPServerConfig) domain.ServerCredentialModel {
	m := domain.ServerCredentialModel{ServerID: c.ID, DisplayName: c.DisplayName, Scope: c.CredentialScope(), CatalogID: c.CatalogID}
	if m.DisplayName == "" {
		m.DisplayName = c.ID
	}
	if uc := c.UserCredential; uc != nil {
		m.Provider = uc.Provider
		m.Scopes = append([]string(nil), uc.Scopes...)
	}
	return m
}

// buildCredentialSource runs the registered factory and installs the source
// on the connector. (nil, nil) when no factory is registered or it built
// nothing. An error leaves the connector without a source: per-user servers
// then fail closed, shared ones are unaffected.
func buildCredentialSource(f NewCredentialSourceFunc, cfg *config.Config, store *storage.BoltConfigStore,
	rt *mcpRuntime, conn *mcp.Connector, seams *credentialSeams) (domain.CredentialSource, error) {
	if f == nil {
		return nil, nil
	}
	if store == nil {
		return nil, fmt.Errorf("the credential provider needs the ADR-0101 config store (accounts are sealed in it)")
	}
	if rt == nil || conn == nil {
		return nil, fmt.Errorf("the credential provider needs the MCP connector")
	}
	records, err := store.SealedRecords(CredentialRecordsNamespace)
	if err != nil {
		return nil, err
	}
	svc := CredentialServices{
		Approvals: seams.approvals,
		ServerCredentialModel: func(id string) (domain.ServerCredentialModel, bool) {
			c, _, ok := rt.config(id)
			if !ok {
				return domain.ServerCredentialModel{}, false
			}
			return serverCredentialModel(c), true
		},
		PersonalServers: func() []domain.ServerCredentialModel {
			var out []domain.ServerCredentialModel
			for _, s := range rt.list() {
				c, _, ok := rt.config(s.ID)
				if !ok {
					continue
				}
				if m := serverCredentialModel(c); m.Scope == domain.CredentialScopePerUser || m.Scope == domain.CredentialScopeEither {
					out = append(out, m)
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })
			return out
		},
		Servers: func() []domain.ServerCredentialModel {
			var out []domain.ServerCredentialModel
			for _, s := range rt.list() {
				if c, _, ok := rt.config(s.ID); ok {
					out = append(out, serverCredentialModel(c))
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })
			return out
		},
		Records:            records,
		ResolveNamedSecret: resolveNamedSecret,
		CredentialChanged: func(serverID, principalID string, connected bool) {
			if connected {
				conn.UserConnected(serverID, principalID)
			} else {
				conn.UserDisconnected(serverID, principalID)
			}
		},
		RecordAudit:          seams.recordAudit,
		RunMutation:          seams.runMutation,
		Caller:               operator.CallerFromContext,
		StoreNamedSecret:     storeNamedSecret,
		ClearNamedSecret:     clearNamedSecret,
		NamedSecretStatus:    namedSecretStatus,
		NamedSecretUpdatedAt: store.SecretUpdatedAt,
		LegacySharedCredentials: func() []LegacySharedCredential {
			return legacySharedCredentials(rt)
		},
		PrincipalIdentities:     seams.principalIdentities,
		SharedCredentialChanged: seams.sharedCredentialChanged,
	}
	src, err := f(svc)
	if err != nil || src == nil {
		return nil, err
	}
	us := cfg.MCP.UserSessions
	conn.SetUserCredentials(src, mcp.UserPoolLimits{Max: us.Max, Idle: time.Duration(us.IdleSeconds) * time.Second})
	// ADR-0141 phase 2/5: a provider that owns ORG credentials serves every
	// shared server's credential too; the consumer-named store entries are no
	// longer read (its factory migrated them). A provider without org
	// accounts leaves shared servers on the store, as in OSS.
	if _, ok := src.(domain.OrgCredentialWriter); ok {
		mcp.SetOrgCredentials(src)
		slog.Info("ADR-0141: shared MCP credentials are served by org accounts")
	}
	seams.setSource(src)
	slog.Info("ADR-0141: outbound credential provider installed")
	return src, nil
}

// legacySharedCredentials enumerates the consumer-named credential slots of
// the installed servers: a token slot for every server (one may have been
// saved whatever the server's auth type) and one env slot per secret_env name.
func legacySharedCredentials(rt *mcpRuntime) []LegacySharedCredential {
	var out []LegacySharedCredential
	for _, s := range rt.list() {
		c, _, ok := rt.config(s.ID)
		if !ok {
			continue
		}
		out = append(out, LegacySharedCredential{ServerID: c.ID, Slot: domain.SlotToken, SecretName: mcp.TokenSecretName(c.ID)})
		for _, n := range c.SecretEnv {
			out = append(out, LegacySharedCredential{ServerID: c.ID, Slot: domain.SlotEnvPrefix + n, SecretName: mcp.EnvSecretName(c.ID, n)})
		}
	}
	return out
}
