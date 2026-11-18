package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/cambrian-sh/core/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// Local accounts (ADR-0142 D1.4).
//
// The personal-install and bootstrap backend: a small table of password users
// in the registry, verified by the kernel itself. It is today's StaticIdentity
// grown up — the env bootstrap operator becomes a local account on every boot —
// and it stays the default when no identity provider is configured.
//
// Passwords are stored as argon2id PHC strings (OWASP 2024 floor: m=19 MiB,
// t=2, p=1). bcrypt was the alternative; argon2id was chosen because it is the
// password-hashing competition winner, memory-hard against GPU guessing, and
// already in golang.org/x/crypto (no new dependency).
//
// Sessions are OPAQUE random tokens held in memory with the operator plane's
// 12 h absolute TTL (ADR-0130 1f). They are the only tokens the kernel mints;
// they do not survive a restart, which is today's behaviour.
// ─────────────────────────────────────────────────────────────────────────────

// SessionTTL bounds a local-account session token (ADR-0130 1f): absolute, not
// sliding, so a token kept warm by a poller still expires.
const SessionTTL = 12 * time.Hour

// LocalIssuer is Identity.Issuer for a local-account session.
const LocalIssuer = "local"

// LocalCredential is one stored password credential.
type LocalCredential struct {
	PrincipalID  string
	Username     string
	PasswordHash string // argon2id PHC string
}

// ErrNoCredential is CredentialStore's "no such username".
var ErrNoCredential = errors.New("identity: no local credential")

// CredentialStore persists local credentials. Both registries implement it.
type CredentialStore interface {
	GetCredential(ctx context.Context, username string) (LocalCredential, error)
	PutCredential(ctx context.Context, c LocalCredential) error
}

// argon2id parameters (see the header).
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword returns the argon2id PHC string for password.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("identity: salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks password against an argon2id PHC string in constant time.
// A malformed hash verifies nothing.
func VerifyPassword(phc, password string) bool {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m == 0 || t == 0 || p == 0 {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is verified against when a username is unknown, so both failure
// paths cost one argon2 evaluation (no username-existence timing oracle — the
// same reason StaticIdentity compared against "" before).
var dummyHash = func() string {
	h, err := HashPassword("cambrian-dummy-password-for-timing")
	if err != nil {
		panic(err)
	}
	return h
}()

type session struct {
	principalID string
	expires     time.Time
}

// LocalAccounts is the local-account backend: domain.Authenticator,
// domain.PasswordLogin and domain.AuthMetadataSource.
type LocalAccounts struct {
	reg   domain.PrincipalRegistry
	creds CredentialStore

	mu       sync.Mutex
	sessions map[string]session
	now      func() time.Time
}

// NewLocalAccounts builds the backend over a registry and its credential store.
func NewLocalAccounts(reg domain.PrincipalRegistry, creds CredentialStore) *LocalAccounts {
	return &LocalAccounts{reg: reg, creds: creds, sessions: map[string]session{}, now: time.Now}
}

// SetPassword creates or replaces the local credential of an existing person.
func (l *LocalAccounts) SetPassword(ctx context.Context, principalID, username, password string) error {
	if strings.TrimSpace(username) == "" || password == "" {
		return fmt.Errorf("%w: username and password are required", domain.ErrInvalidPrincipal)
	}
	h, err := HashPassword(password)
	if err != nil {
		return err
	}
	return l.creds.PutCredential(ctx, LocalCredential{PrincipalID: principalID, Username: username, PasswordHash: h})
}

// EnsureAccount makes username a local account: the person (id = username) is
// created if absent with the given roles, and the password is (re)set when the
// stored hash does not already verify it. Idempotent, so the env bootstrap
// operator can be re-asserted on every boot (the migration from
// CAMBRIAN_OPERATOR_USER/PASSWORD — the env stays the source of truth for that
// one account, exactly as before).
func (l *LocalAccounts) EnsureAccount(ctx context.Context, username, password string, roles []string) (domain.Principal, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return domain.Principal{}, fmt.Errorf("%w: username and password are required", domain.ErrInvalidPrincipal)
	}
	p, _, err := l.reg.EnsurePrincipal(ctx, domain.Principal{
		ID: username, Kind: domain.PrincipalUser, DisplayName: username, Source: domain.SourceLocal, Roles: roles,
	})
	if err != nil {
		return domain.Principal{}, fmt.Errorf("identity: ensure local account %q: %w", username, err)
	}
	if !sameRoles(p.Roles, roles) {
		if err := l.reg.SetRoles(ctx, p.ID, roles); err != nil {
			return domain.Principal{}, err
		}
		p.Roles = normalizeRoles(roles)
	}
	if c, err := l.creds.GetCredential(ctx, username); err == nil && c.PrincipalID == p.ID && VerifyPassword(c.PasswordHash, password) {
		return p, nil
	} else if err != nil && !errors.Is(err, ErrNoCredential) {
		return domain.Principal{}, err
	}
	if err := l.SetPassword(ctx, p.ID, username, password); err != nil {
		return domain.Principal{}, err
	}
	return p, nil
}

func sameRoles(a, b []string) bool {
	a, b = normalizeRoles(a), normalizeRoles(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Login implements domain.PasswordLogin.
func (l *LocalAccounts) Login(ctx context.Context, username, password string) (string, domain.Identity, error) {
	c, err := l.creds.GetCredential(ctx, strings.TrimSpace(username))
	hash := dummyHash
	if err == nil {
		hash = c.PasswordHash
	} else if !errors.Is(err, ErrNoCredential) {
		slog.Warn("identity: local login: credential lookup failed", "err", err)
	}
	// Always evaluate argon2 before looking at err, so an unknown user costs the
	// same as a wrong password.
	ok := VerifyPassword(hash, password)
	if err != nil && !errors.Is(err, ErrNoCredential) {
		// The credential table did not answer: the password was never checked,
		// so this is not "invalid credentials" (ENG-2120).
		return "", domain.Identity{}, fmt.Errorf("%w: credential lookup: %w", domain.ErrAuthUnavailable, err)
	}
	if err != nil || !ok {
		return "", domain.Identity{}, fmt.Errorf("%w: invalid credentials", domain.ErrUnauthenticated)
	}
	id, err := l.identityOf(ctx, c.PrincipalID)
	if err != nil {
		return "", domain.Identity{}, err
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", domain.Identity{}, fmt.Errorf("identity: token generation: %w", err)
	}
	token := hex.EncodeToString(b[:])
	exp := l.now().Add(SessionTTL)
	l.mu.Lock()
	l.sessions[token] = session{principalID: c.PrincipalID, expires: exp}
	l.mu.Unlock()
	id.ExpiresAt = exp
	return token, id, nil
}

// identityOf re-reads the person on every use, so a suspension takes effect on
// the next call rather than at token expiry.
func (l *LocalAccounts) identityOf(ctx context.Context, principalID string) (domain.Identity, error) {
	p, err := l.reg.GetPrincipal(ctx, principalID)
	if errors.Is(err, domain.ErrPrincipalNotFound) {
		return domain.Identity{}, fmt.Errorf("%w: %v", domain.ErrUnauthenticated, err)
	}
	if err != nil {
		// The registry did not answer (Postgres down, a timeout): the token may be
		// perfectly good, so it is not reported as one that is not. Still refused.
		// T-20261118-operator-rpcs-answer-unauthenticated-invalid-or.
		return domain.Identity{}, fmt.Errorf("%w: principal lookup: %w", domain.ErrAuthUnavailable, err)
	}
	if !p.Active() {
		return domain.Identity{}, fmt.Errorf("%w: %v", domain.ErrUnauthenticated, domain.ErrPrincipalInactive)
	}
	return domain.Identity{Principal: p.Ref(), Roles: append([]string(nil), p.Roles...), Issuer: LocalIssuer}, nil
}

// Resolve implements domain.Authenticator. Expiry is lazy, as before.
func (l *LocalAccounts) Resolve(ctx context.Context, token string) (domain.Identity, error) {
	l.mu.Lock()
	s, ok := l.sessions[token]
	if ok && !l.now().Before(s.expires) {
		delete(l.sessions, token)
		ok = false
	}
	l.mu.Unlock()
	if !ok || token == "" {
		return domain.Identity{}, fmt.Errorf("%w: invalid or expired token", domain.ErrUnauthenticated)
	}
	id, err := l.identityOf(ctx, s.principalID)
	if err != nil {
		return domain.Identity{}, err
	}
	id.ExpiresAt = s.expires
	return id, nil
}

// Revoke implements domain.Authenticator.
func (l *LocalAccounts) Revoke(_ context.Context, token string) error {
	l.mu.Lock()
	delete(l.sessions, token)
	l.mu.Unlock()
	return nil
}

// AuthMetadata implements domain.AuthMetadataSource.
func (l *LocalAccounts) AuthMetadata() domain.AuthMetadata {
	return domain.AuthMetadata{PasswordLogin: true}
}

var (
	_ domain.Authenticator      = (*LocalAccounts)(nil)
	_ domain.PasswordLogin      = (*LocalAccounts)(nil)
	_ domain.AuthMetadataSource = (*LocalAccounts)(nil)
)
