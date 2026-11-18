package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
)

// Per-user credentials (ADR-0140 D4, kept by ADR-0141): a server whose credential scope is
// per_user (or either, for a principal who connected) is served by a session
// PER (server, principal), each carrying exactly one credential — that
// principal's. A shared session is never used for a per_user server, and a
// per-principal session is never reused for another principal.
//
//   - http/sse: one MCP session per principal whose HTTP client asks the
//     credential source for the CURRENT token on every request
//     (Authorization: Bearer, or the configured header), so a refreshed token
//     is used without reconnecting. A session per principal, not one shared
//     session with a per-request header, because MCP servers bind session state
//     (and, per the 2025-11-25 spec's session-hijacking guidance, the session
//     id) to the authenticated user: one session carrying several users'
//     tokens would be refused, or worse, honoured.
//   - stdio: one PROCESS per principal, launched with the principal's token in
//     its environment variable. Such a server cannot see a refresh, so a
//     process whose token has since been refreshed is retired and relaunched
//     on its next use. It gets the server's literal settings and passthrough,
//     but NOT the server's shared secret_env: a per-user process carries one
//     credential only.
//
// Sessions start lazily on a principal's first call, close after
// mcp.user_sessions.idle_seconds unused, and are capped at
// mcp.user_sessions.max (the least recently used idle one is evicted;
// with none idle the call is refused, never queued unboundedly).
//
// The credential itself comes from the installed domain.CredentialSource
// (ADR-0141: premium's accounts plugin). This file is credential DELIVERY into
// an MCP transport — which session or process carries which principal's
// token — and stays in core with the connector it is part of; it never stores,
// obtains or refreshes a credential.

// Credential scopes, mirrored from config (ADR-0140 D1).
const (
	ScopeShared  = "shared"
	ScopePerUser = "per_user"
	ScopeEither  = "either"
)

// HealthAwaitingUser: a per_user server has no shared session; it is up as
// far as the kernel can take it and waits for a person to connect an account
// (its tools are discovered on the first connection).
const HealthAwaitingUser = "awaiting_user"

// CallSessionSource resolves the session for one tool CALL, which may depend
// on the run principal on ctx. release must be called once the call is done;
// failed=true retires a per-principal session (its process or connection is
// suspect) so the next call starts a fresh one.
type CallSessionSource interface {
	SessionForCall(ctx context.Context, serverID string) (sess *mcpsdk.ClientSession, release func(failed bool), err error)
}

// UserPoolLimits bounds the per-principal sessions.
type UserPoolLimits struct {
	Max  int           // 0 ⇒ 32
	Idle time.Duration // 0 ⇒ 10 min
}

type userKey struct{ server, principal string }

type userInstance struct {
	key      userKey
	sess     *mcpsdk.ClientSession
	tokenFP  string // fingerprint of an env-injected token ("" for http)
	lastUsed time.Time
	inflight int
	retired  bool
	ready    chan struct{}
	err      error
}

type userPool struct {
	mu        sync.Mutex
	src       domain.CredentialSource
	limits    UserPoolLimits
	inst      map[userKey]*userInstance
	published map[string]bool          // per-user server id → tools published
	nudge     map[string]chan struct{} // per-user server id → "an account was connected"
	now       func() time.Time
}

// newUserPool builds the per-user pool over its credential source and limits
// (CredentialSources.User / UserLimits, fixed at construction). nil src ⇒
// per_user servers cannot be used (calls fail "not connected"): the OSS
// default, and a premium build whose accounts plugin is off.
func newUserPool(src domain.CredentialSource, limits UserPoolLimits) *userPool {
	if limits.Max <= 0 {
		limits.Max = 32
	}
	if limits.Idle <= 0 {
		limits.Idle = 10 * time.Minute
	}
	return &userPool{inst: map[userKey]*userInstance{}, published: map[string]bool{},
		nudge: map[string]chan struct{}{}, now: time.Now, src: src, limits: limits}
}

func (c *Connector) userSource() domain.CredentialSource {
	c.users.mu.Lock()
	defer c.users.mu.Unlock()
	return c.users.src
}

func (c *Connector) rememberConfig(s ServerConfig) {
	c.mu.Lock()
	c.configs[s.ID] = s
	c.mu.Unlock()
}

func (c *Connector) configFor(id string) (ServerConfig, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.configs[id]
	return s, ok
}

func (s ServerConfig) scope() string {
	if s.CredentialScope == "" {
		return ScopeShared
	}
	return s.CredentialScope
}

// PerUser reports whether every call must use the caller's own credential.
func (s ServerConfig) PerUser() bool { return s.scope() == ScopePerUser }

// NotConnectedError is the clean, actionable tool error for a principal who
// has not connected (or must reconnect) a per-user integration. It names no
// principal and carries no credential material.
type NotConnectedError struct {
	ServerID  string
	Reconnect bool
	NoPerson  bool
	// Pending: the person signed in, but the account waits for their
	// confirmation (ADR-0141 owner position 4). Never a reason to fall back.
	Pending bool
	// NeedsConsent: the person's account is connected but was not granted
	// every scope the server declares (ADR-0141 grant scope enforcement).
	// Detail names the missing scopes. Never a reason to fall back.
	NeedsConsent bool
	Detail       string
}

// notConnectedFor maps a credential-source refusal to the actionable tool
// error, or nil when err is not one of the refusals it knows.
func notConnectedFor(serverID string, err error) *NotConnectedError {
	switch {
	case errors.Is(err, domain.ErrCredentialReconnectNeeded):
		return &NotConnectedError{ServerID: serverID, Reconnect: true}
	case errors.Is(err, domain.ErrCredentialPendingConfirmation):
		return &NotConnectedError{ServerID: serverID, Pending: true}
	case errors.Is(err, domain.ErrCredentialInsufficientScope):
		return &NotConnectedError{ServerID: serverID, NeedsConsent: true, Detail: err.Error()}
	case errors.Is(err, domain.ErrCredentialNotConnected):
		return &NotConnectedError{ServerID: serverID}
	}
	return nil
}

func (e *NotConnectedError) Error() string {
	switch {
	case e.NoPerson:
		return fmt.Sprintf("integration %q acts with each person's own account, and this call is not acting for a person; it cannot run here", e.ServerID)
	case e.Reconnect:
		return fmt.Sprintf("your sign-in for integration %q has expired or was revoked. Reconnect it in Cambrian: Integrations > Connect my accounts > %s", e.ServerID, e.ServerID)
	case e.Pending:
		return fmt.Sprintf("the account you connected for integration %q is waiting for your confirmation. Confirm it in Cambrian: Integrations > Connect my accounts > %s", e.ServerID, e.ServerID)
	case e.NeedsConsent:
		return fmt.Sprintf("integration %q needs more permission than your connected account was granted (%s). Reconnect it in Cambrian and approve the requested access: Integrations > Connect my accounts > %s", e.ServerID, e.Detail, e.ServerID)
	}
	return fmt.Sprintf("integration %q uses your own account, and you have not connected it. Connect it in Cambrian: Integrations > Connect my accounts > %s", e.ServerID, e.ServerID)
}

// SessionForCall implements CallSessionSource.
func (c *Connector) SessionForCall(ctx context.Context, serverID string) (*mcpsdk.ClientSession, func(bool), error) {
	s, known := c.configFor(serverID)
	if !known {
		s = ServerConfig{ID: serverID}
	}
	return c.sessionFor(ctx, s)
}

// sessionFor selects the session a call to server s runs on: the shared
// session for a shared server, else the run principal's own (ADR-0140). Used
// by SessionForCall (the armed config) and TestServer (the config under test,
// which may not be armed yet).
func (c *Connector) sessionFor(ctx context.Context, s ServerConfig) (*mcpsdk.ClientSession, func(bool), error) {
	noop := func(bool) {}
	serverID := s.ID
	if s.scope() == ScopeShared {
		sess, ok := c.Session(serverID)
		if !ok {
			return nil, nil, fmt.Errorf("mcp server %q not connected", serverID)
		}
		return sess, noop, nil
	}
	shared := func(cause error) (*mcpsdk.ClientSession, func(bool), error) {
		if s.scope() == ScopeEither && s.SharedFallback {
			if sess, ok := c.Session(serverID); ok {
				return sess, noop, nil
			}
		}
		return nil, nil, cause
	}
	principal := domain.RunPrincipalFromContext(ctx)
	if principal == "" {
		return shared(&NotConnectedError{ServerID: serverID, NoPerson: true})
	}
	src := c.userSource()
	if src == nil {
		return shared(&NotConnectedError{ServerID: serverID})
	}
	tok, err := src.Credential(ctx, domain.CredentialRequest{ServerID: serverID, PrincipalID: principal, Purpose: domain.CredentialPurposeMCP})
	switch {
	case errors.Is(err, domain.ErrCredentialNotConnected):
		return shared(&NotConnectedError{ServerID: serverID})
	case errors.Is(err, domain.ErrCredentialReconnectNeeded):
		// A dead grant is NOT a reason to act as someone else: no fallback.
		return nil, nil, &NotConnectedError{ServerID: serverID, Reconnect: true}
	case errors.Is(err, domain.ErrCredentialPendingConfirmation):
		// Nor is an unconfirmed one (ADR-0141 position 4).
		return nil, nil, &NotConnectedError{ServerID: serverID, Pending: true}
	case errors.Is(err, domain.ErrCredentialInsufficientScope):
		// Nor an account short of the scopes the server declares: acting as
		// the shared account instead would hide the missing consent.
		return nil, nil, notConnectedFor(serverID, err)
	case err != nil:
		return nil, nil, fmt.Errorf("integration %q: could not obtain your credential: %w", serverID, err)
	}
	inst, err := c.acquireUser(ctx, s, principal, tok)
	if err != nil {
		return nil, nil, err
	}
	return inst.sess, func(failed bool) { c.releaseUser(inst, failed) }, nil
}

func fingerprint(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:8])
}

// acquireUser returns a live session for (server, principal), starting one
// when needed. The returned instance is held (inflight) until releaseUser.
func (c *Connector) acquireUser(ctx context.Context, s ServerConfig, principal string, tok domain.Credential) (*userInstance, error) {
	up := c.users
	fp := ""
	if s.UserInject == "env" {
		fp = fingerprint(tok.Token)
	}
	k := userKey{s.ID, principal}
	var toClose []*userInstance

	up.mu.Lock()
	if inst := up.inst[k]; inst != nil && !inst.retired && (fp == "" || inst.tokenFP == fp) {
		inst.inflight++
		up.mu.Unlock()
		select {
		case <-inst.ready:
		case <-ctx.Done():
			c.releaseUser(inst, false)
			return nil, ctx.Err()
		}
		if inst.err != nil {
			c.releaseUser(inst, true)
			return nil, inst.err
		}
		return inst, nil
	}
	if old := up.inst[k]; old != nil {
		// Launched with a token that has since been refreshed (stdio): retire
		// it; it closes when its in-flight calls finish.
		old.retired = true
		delete(up.inst, k)
		if old.inflight == 0 {
			toClose = append(toClose, old)
		}
	}
	if len(up.inst) >= up.limits.Max {
		victim := up.lruIdleLocked()
		if victim == nil {
			up.mu.Unlock()
			closeInstances(toClose)
			return nil, fmt.Errorf("integration %q: every per-user session (%d) is busy; try again shortly", s.ID, up.limits.Max)
		}
		victim.retired = true
		delete(up.inst, victim.key)
		toClose = append(toClose, victim)
	}
	inst := &userInstance{key: k, tokenFP: fp, inflight: 1, ready: make(chan struct{}), lastUsed: up.now()}
	up.inst[k] = inst
	up.mu.Unlock()
	closeInstances(toClose)

	sess, err := c.dialUser(ctx, s, principal, tok)
	up.mu.Lock()
	inst.sess, inst.err = sess, err
	if err != nil {
		inst.retired = true
		if up.inst[k] == inst {
			delete(up.inst, k)
		}
	}
	close(inst.ready)
	up.mu.Unlock()
	if err != nil {
		c.releaseUser(inst, true)
		return nil, err
	}
	return inst, nil
}

// lruIdleLocked returns the least recently used idle, ready instance.
func (up *userPool) lruIdleLocked() *userInstance {
	var best *userInstance
	for _, in := range up.inst {
		if in.inflight > 0 {
			continue
		}
		select {
		case <-in.ready:
		default:
			continue
		}
		if best == nil || in.lastUsed.Before(best.lastUsed) {
			best = in
		}
	}
	return best
}

func (c *Connector) releaseUser(inst *userInstance, failed bool) {
	up := c.users
	up.mu.Lock()
	inst.inflight--
	inst.lastUsed = up.now()
	if failed && !inst.retired {
		inst.retired = true
		if up.inst[inst.key] == inst {
			delete(up.inst, inst.key)
		}
	}
	closeNow := inst.retired && inst.inflight == 0
	up.mu.Unlock()
	if closeNow {
		closeInstances([]*userInstance{inst})
	}
}

func closeInstances(list []*userInstance) {
	for _, in := range list {
		if in.sess != nil {
			_ = in.sess.Close()
		}
	}
}

// ReapIdleUsers closes every per-principal session idle for longer than the
// configured idle time. Called by the Watch loop; exported for tests.
func (c *Connector) ReapIdleUsers() int {
	up := c.users
	up.mu.Lock()
	now := up.now()
	var victims []*userInstance
	for k, in := range up.inst {
		if in.inflight == 0 && now.Sub(in.lastUsed) >= up.limits.Idle {
			in.retired = true
			delete(up.inst, k)
			victims = append(victims, in)
		}
	}
	up.mu.Unlock()
	closeInstances(victims)
	return len(victims)
}

// UserInstances lists the live per-principal sessions as "server/principal"
// (tests and diagnostics; never tokens).
func (c *Connector) UserInstances() []string {
	up := c.users
	up.mu.Lock()
	defer up.mu.Unlock()
	out := make([]string, 0, len(up.inst))
	for k := range up.inst {
		out = append(out, k.server+"/"+k.principal)
	}
	sort.Strings(out)
	return out
}

// dropUsers closes every per-principal session of a server ("" principal ⇒
// all principals). In-flight sessions close when their call finishes.
func (c *Connector) dropUsers(serverID, principal string) {
	up := c.users
	up.mu.Lock()
	var victims []*userInstance
	for k, in := range up.inst {
		if k.server == serverID && (principal == "" || k.principal == principal) {
			in.retired = true
			delete(up.inst, k)
			if in.inflight == 0 {
				victims = append(victims, in)
			}
		}
	}
	up.mu.Unlock()
	closeInstances(victims)
}

// UserConnected is told when a principal connects (or reconnects) an
// account: their old session (old token) is retired, and a per-user server
// still waiting for its first account is nudged to discover its tools.
func (c *Connector) UserConnected(serverID, principal string) {
	c.dropUsers(serverID, principal)
	c.users.mu.Lock()
	ch := c.users.nudge[serverID]
	c.users.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// UserDisconnected retires a principal's session after a disconnect,
// revoke or dead grant.
func (c *Connector) UserDisconnected(serverID, principal string) { c.dropUsers(serverID, principal) }

// dialUser opens one principal's session. The dial context is detached from
// the call (another caller may be waiting on the same dial) and bounded.
func (c *Connector) dialUser(ctx context.Context, s ServerConfig, principal string, tok domain.Credential) (*mcpsdk.ClientSession, error) {
	var transport mcpsdk.Transport
	switch s.Transport {
	case "stdio":
		explicit := make(map[string]string, len(s.EnvValues)+1)
		for k, v := range s.EnvValues {
			explicit[k] = v
		}
		explicit[s.UserInjectName] = tok.Token
		// ADR-0139 D8: the same trust-tier containment as the shared session,
		// in a workdir of this principal's own (a shared one would let one
		// user's server instance read another's files).
		t, err := c.stdioTransportFor(ctx, s, explicit, principal)
		if err != nil {
			return nil, err
		}
		transport = t
	case "http", "sse":
		// Guarded egress (ADR-0153, ENG-1419), the same as the shared session.
		hc := s.guardedHTTPClient(func(base http.RoundTripper) http.RoundTripper {
			return &userAuthRoundTripper{
				base: base, src: c.userSource(), server: s.ID, principal: principal,
				inject: s.UserInject, header: s.UserInjectName,
			}
		})
		if s.Transport == "http" {
			transport = &mcpsdk.StreamableClientTransport{Endpoint: s.Endpoint, HTTPClient: hc, DisableStandaloneSSE: true}
		} else {
			transport = &mcpsdk.SSEClientTransport{Endpoint: s.Endpoint, HTTPClient: hc}
		}
	default:
		return nil, fmt.Errorf("unknown transport %q (want stdio|http|sse)", s.Transport)
	}
	// Detached from the call (the session is pooled past it); only the
	// handshake is bounded — a WithTimeout here cancelled the legacy-SSE
	// stream the moment this function returned (ENG-1528, handshake.go).
	sess, err := c.connectSession(context.WithoutCancel(ctx), transport)
	if err != nil {
		return nil, fmt.Errorf("integration %q: starting your session failed: %s", s.ID, redact(err.Error(), []string{tok.Token}))
	}
	return sess, nil
}

// userAuthRoundTripper puts the principal's CURRENT token on every request
// (refreshing it when due), so a long-lived session never sends a stale one.
type userAuthRoundTripper struct {
	base              http.RoundTripper
	src               domain.CredentialSource
	server, principal string
	inject, header    string
}

func (a *userAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if a.src == nil {
		return nil, &NotConnectedError{ServerID: a.server}
	}
	tok, err := a.src.Credential(req.Context(), domain.CredentialRequest{ServerID: a.server, PrincipalID: a.principal, Purpose: domain.CredentialPurposeMCP})
	if err != nil {
		if nc := notConnectedFor(a.server, err); nc != nil {
			return nil, nc
		}
		return nil, err
	}
	req = req.Clone(req.Context())
	if a.inject == "header" && a.header != "" {
		req.Header.Set(a.header, tok.Token)
	} else {
		req.Header.Set("Authorization", "Bearer "+tok.Token)
	}
	return a.base.RoundTrip(req)
}

// ── discovery for per_user servers ─────────────────────────────────────────

// watchUserServer brings a per_user server up as far as it can go without a
// person: it has no shared session, so its tool list is discovered through
// the first connected principal's session, once, and published for everyone
// (a server's tool list does not depend on whose account it acts as; each
// CALL still uses the caller's own credential). It retries on its interval
// and immediately when an account is connected.
func (c *Connector) watchUserServer(ctx context.Context, s ServerConfig, sink ToolSink, interval time.Duration, gen uint64) {
	c.users.mu.Lock()
	nudge := c.users.nudge[s.ID]
	c.users.mu.Unlock()
	// ENG-743: the tools last discovered for this server (by anyone) go on the
	// menu NOW, so before the first person connects (and after every restart)
	// an agent can call one and get "connect your account" instead of finding
	// no tool at all. A live discovery below replaces them.
	known := c.publishKnownTools(ctx, s, sink)
	for {
		if ctx.Err() != nil {
			return
		}
		src := c.userSource()
		switch {
		case src == nil:
			c.setHealth(s.ID, gen, func(h *ServerHealth) {
				h.State, h.ToolCount = HealthAwaitingUser, 0
				h.LastError = "per-user accounts are not available on this kernel (they are provided by the premium accounts plugin, ADR-0141)"
			})
		default:
			principals := src.ConnectedPrincipals(ctx, s.ID)
			if len(principals) == 0 {
				c.setHealth(s.ID, gen, func(h *ServerHealth) {
					h.State, h.ToolCount, h.LastError = HealthAwaitingUser, known, ""
				})
			}
			for _, p := range principals {
				n, err := c.discoverAs(ctx, s, sink, p)
				if ctx.Err() != nil {
					return
				}
				if err == nil {
					c.setHealth(s.ID, gen, func(h *ServerHealth) {
						h.State, h.LastError, h.ToolCount, h.LastHandshake = HealthReady, "", n, time.Now()
					})
					return // published; per-call sessions take it from here
				}
				msg := err.Error()
				c.setHealth(s.ID, gen, func(h *ServerHealth) { h.State, h.LastError = HealthError, msg })
			}
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-nudge:
			t.Stop()
		case <-t.C:
		}
	}
}

// discoverAs lists a per_user server's tools through one principal's session
// and publishes them.
func (c *Connector) discoverAs(ctx context.Context, s ServerConfig, sink ToolSink, principal string) (int, error) {
	src := c.userSource()
	tok, err := src.Credential(ctx, domain.CredentialRequest{ServerID: s.ID, PrincipalID: principal, Purpose: domain.CredentialPurposeMCP})
	if err != nil {
		return 0, fmt.Errorf("discovery: %w", err)
	}
	inst, err := c.acquireUser(ctx, s, principal, tok)
	if err != nil {
		return 0, err
	}
	lctx, cancel := context.WithTimeout(ctx, c.handshakeTimeout()) // a silent tools/list must not stall the loop (ENG-1528)
	adv, derr := listAdvertised(lctx, inst.sess)
	cancel()
	c.releaseUser(inst, derr != nil)
	if derr != nil {
		return 0, fmt.Errorf("connected, but listing tools failed: %s", redact(derr.Error(), []string{tok.Token}))
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	tools := toolsFor(s, adv)
	c.saveToolList(ctx, s.ID, adv)
	if sink != nil {
		sink.SetServerTools(ctx, s.ID, tools)
	}
	c.users.mu.Lock()
	c.users.published[s.ID] = true
	c.users.mu.Unlock()
	slog.Info("ADR-0140: per-user MCP server tools discovered", "server", s.ID, "tools", len(tools))
	return len(tools), nil
}

// closeAllUsers closes every per-principal session (shutdown).
func (c *Connector) closeAllUsers() {
	c.users.mu.Lock()
	var all []*userInstance
	for k, in := range c.users.inst {
		in.retired = true
		delete(c.users.inst, k)
		if in.inflight == 0 {
			all = append(all, in)
		}
	}
	c.users.mu.Unlock()
	closeInstances(all)
}
