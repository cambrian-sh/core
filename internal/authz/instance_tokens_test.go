package authz_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ADR-0157 D4: per-instance tokens and the verifier's two credential paths.

func TestInstanceTokens_MintLookupRevoke(t *testing.T) {
	t.Parallel()
	reg := authz.NewInstanceTokens()
	a, err := reg.Mint("chat_agent", "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := reg.Mint("chat_agent", "inst-b") // a sibling of the same agent
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 || a == b {
		t.Fatal("tokens must be 32 random bytes, distinct per instance")
	}
	if id, ok := reg.Lookup(a); !ok || id != (authz.InstanceIdentity{AgentID: "chat_agent", InstanceID: "inst-a"}) {
		t.Fatalf("lookup a = %+v %v", id, ok)
	}

	// Re-minting one instance (restart under the same id) replaces only its token.
	a2, err := reg.Mint("chat_agent", "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Lookup(a); ok {
		t.Fatal("the instance's previous token survived a re-mint")
	}
	if _, ok := reg.Lookup(a2); !ok {
		t.Fatal("re-minted token not live")
	}
	if _, ok := reg.Lookup(b); !ok {
		t.Fatal("the sibling's token was revoked by another instance's re-mint")
	}

	reg.Revoke("inst-a")
	reg.Revoke("inst-a") // idempotent
	if _, ok := reg.Lookup(a2); ok {
		t.Fatal("revoked token still live")
	}
	if _, ok := reg.Lookup(b); !ok || reg.Len() != 1 {
		t.Fatal("revoking one instance touched another")
	}
	if _, ok := reg.Lookup(""); ok {
		t.Fatal("empty token matched")
	}
	if _, err := reg.Mint("", "x"); err == nil {
		t.Fatal("mint without an agent id accepted")
	}
}

// -race: concurrent mint, lookup and revoke.
func TestInstanceTokens_Concurrent(t *testing.T) {
	t.Parallel()
	reg := authz.NewInstanceTokens()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inst := string(rune('a' + i))
			for j := 0; j < 50; j++ {
				tok, err := reg.Mint("agent", inst)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = reg.Lookup(tok)
				reg.Revoke(inst)
			}
		}(i)
	}
	wg.Wait()
	if reg.Len() != 0 {
		t.Fatalf("live tokens = %d, want 0", reg.Len())
	}
}

func TestAgentCredentialVerifier_RequiresRegistryAndPredicate(t *testing.T) {
	t.Parallel()
	never := func(context.Context, string) (bool, error) { return false, nil }
	if _, err := authz.NewAgentCredentialVerifier(nil, nil, never, false); err == nil {
		t.Fatal("verifier built without an instance-token registry")
	}
	if _, err := authz.NewAgentCredentialVerifier(nil, authz.NewInstanceTokens(), nil, false); err == nil {
		t.Fatal("verifier built without the kernel-spawned predicate")
	}
}

func TestAgentCredentialVerifier_InstanceAndStoredPaths(t *testing.T) {
	t.Parallel()
	const method = "/cambrian.Orchestrator/ListTools"
	reg := authz.NewInstanceTokens()
	spawned := map[string]bool{"chat_agent": true, "analyst_agent": true}
	stored := secretStore{
		authz.AgentTokenSecretName("analyst_agent"):  "stale-stored-secret", // kernel-spawned: must be refused
		authz.AgentTokenSecretName("external_agent"): "external-secret",
	}
	v, err := authz.NewAgentCredentialVerifier(stored.resolve, reg,
		func(_ context.Context, id string) (bool, error) { return spawned[id], nil }, false)
	if err != nil {
		t.Fatal(err)
	}
	call := func(agent, token string) (domain.PrincipalRef, error) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("x-agent-id", agent, "authorization", "Bearer "+token))
		out, err := v.Authenticate(ctx, method, nil)
		return domain.PrincipalFromContext(out), err
	}
	chatTok, err := reg.Mint("chat_agent", "inst-1")
	if err != nil {
		t.Fatal(err)
	}

	// (a) a live instance token for the asserted agent.
	if p, err := call("chat_agent", chatTok); err != nil || p.ID != "chat_agent" {
		t.Fatalf("live instance token refused: p=%+v err=%v", p, err)
	}
	// ...but not when the header names another agent, even one whose stored
	// secret would match nothing.
	if _, err := call("external_agent", chatTok); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("instance token accepted for another agent: %v", err)
	}
	// (b) refused for a kernel-spawned agent even with its (stale) stored secret.
	if _, err := call("analyst_agent", "stale-stored-secret"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("kernel-spawned agent authenticated with a stored secret: %v", err)
	}
	// (b) an external agent keeps authenticating with its stored secret.
	if p, err := call("external_agent", "external-secret"); err != nil || p.ID != "external_agent" {
		t.Fatalf("external agent's stored secret refused: p=%+v err=%v", p, err)
	}
	// Revocation is instant.
	reg.Revoke("inst-1")
	if _, err := call("chat_agent", chatTok); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("revoked instance token accepted: %v", err)
	}
}

// ADR-0157 D5.2: the verifier stamps the instance a live per-instance token was
// minted for on the context (the provenance of what the call writes); the
// stored-secret path of an external agent stamps none.
func TestAgentCredentialVerifier_StampsTheCallingInstance(t *testing.T) {
	t.Parallel()
	reg := authz.NewInstanceTokens()
	stored := secretStore{authz.AgentTokenSecretName("external_agent"): "external-secret"}
	v, err := authz.NewAgentCredentialVerifier(stored.resolve, reg,
		func(_ context.Context, id string) (bool, error) { return id == "analyst_agent", nil }, false)
	if err != nil {
		t.Fatal(err)
	}
	instanceOf := func(agent, token string) string {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("x-agent-id", agent, "authorization", "Bearer "+token))
		out, err := v.Authenticate(ctx, "/cambrian.Orchestrator/UploadArtifact", nil)
		if err != nil {
			t.Fatalf("%s: %v", agent, err)
		}
		return domain.AgentInstanceFromContext(out)
	}
	tok, err := reg.Mint("analyst_agent", "inst-77")
	if err != nil {
		t.Fatal(err)
	}
	if got := instanceOf("analyst_agent", tok); got != "inst-77" {
		t.Fatalf("instance on ctx = %q, want inst-77", got)
	}
	if got := instanceOf("external_agent", "external-secret"); got != "" {
		t.Fatalf("a stored-secret caller was given an instance %q", got)
	}
}

// ENG-2120: when the agent registry cannot be read the stored-secret path is
// refused as retryable Unavailable — not accepted (fail closed), and not
// Unauthenticated (the stored token may be perfectly good). A live instance
// token never consults the registry and keeps working through the outage.
func TestAgentCredentialVerifier_RegistryDownIsUnavailable(t *testing.T) {
	t.Parallel()
	const method = "/cambrian.Orchestrator/ListTools"
	reg := authz.NewInstanceTokens()
	stored := secretStore{authz.AgentTokenSecretName("external_agent"): "external-secret"}
	var down atomic.Bool
	v, err := authz.NewAgentCredentialVerifier(stored.resolve, reg,
		func(context.Context, string) (bool, error) {
			if down.Load() {
				return true, fmt.Errorf("%w: agent registry: connection refused", domain.ErrAuthUnavailable)
			}
			return false, nil
		}, false)
	if err != nil {
		t.Fatal(err)
	}
	call := func(agent, token string) error {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("x-agent-id", agent, "authorization", "Bearer "+token))
		_, err := v.Authenticate(ctx, method, nil)
		return err
	}
	chatTok, err := reg.Mint("chat_agent", "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	if err := call("external_agent", "external-secret"); status.Code(err) != codes.Unavailable {
		t.Fatalf("stored secret while the registry is down: %v, want Unavailable", err)
	}
	if err := call("external_agent", "wrong"); status.Code(err) != codes.Unavailable {
		// Undecidable either way: the registry decides whether this path is open.
		t.Fatalf("wrong secret while the registry is down: %v, want Unavailable", err)
	}
	if err := call("chat_agent", chatTok); err != nil {
		t.Fatalf("instance token during a registry outage: %v", err)
	}
	down.Store(false)
	if err := call("external_agent", "external-secret"); err != nil {
		t.Fatalf("stored secret after recovery: %v", err)
	}
	if err := call("external_agent", "wrong"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong secret after recovery: %v, want Unauthenticated", err)
	}
}
