package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/storage"
)

// fakeCredentialStore is the ADR-0101 store's presence+write surface.
type fakeCredentialStore struct {
	secrets map[string]string
	writes  int
}

func (f *fakeCredentialStore) Configured(name string) bool { _, ok := f.secrets[name]; return ok }
func (f *fakeCredentialStore) SetSecret(name, value string) error {
	f.secrets[name] = value
	f.writes++
	return nil
}
func (f *fakeCredentialStore) ClearSecret(name string) error {
	delete(f.secrets, name)
	return nil
}

// Minting is idempotent and never rotates: a second run writes nothing, and
// an agent that already has a credential keeps the one external copies hold.
func TestMintMissingAgentCredentials_IdempotentAndNeverRotates(t *testing.T) {
	t.Parallel()
	store := &fakeCredentialStore{secrets: map[string]string{
		authz.AgentTokenSecretName("summariser"): "keep-me",
	}}
	minted, err := mintMissingAgentCredentials(store, []string{"analyst_agent", "summariser", "", "pulse_agent"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(minted, ",") != "analyst_agent,pulse_agent" {
		t.Fatalf("minted = %v, want analyst_agent and pulse_agent only", minted)
	}
	if store.secrets[authz.AgentTokenSecretName("summariser")] != "keep-me" {
		t.Fatal("an existing credential was rotated at mint time")
	}
	tok := store.secrets[authz.AgentTokenSecretName("analyst_agent")]
	if len(tok) != agentTokenBytes*2 {
		t.Fatalf("token length %d, want %d hex chars", len(tok), agentTokenBytes*2)
	}
	again, err := mintMissingAgentCredentials(store, []string{"analyst_agent", "summariser", "pulse_agent"})
	if err != nil || len(again) != 0 {
		t.Fatalf("second run minted %v (err %v), want nothing", again, err)
	}
	if store.writes != 2 {
		t.Fatalf("writes = %d, want 2", store.writes)
	}
}

func TestMintMissingAgentCredentials_NoStoreIsAnError(t *testing.T) {
	t.Parallel()
	if _, err := mintMissingAgentCredentials(nil, []string{"a"}); err == nil {
		t.Fatal("nil store accepted")
	}
}

// ADR-0157 Q4 migration, on the store the secrets live in (configs/config.db,
// a real BoltConfigStore): ensureAgentCredentials deletes the stored secrets of
// kernel-spawned agents, keeps an external agent's, mints for a registered
// agent the kernel does not spawn — and is idempotent across boots.
func TestEnsureAgentCredentials_RetiresKernelSpawnedSecrets_RealStore(t *testing.T) {
	store, err := storage.OpenConfigStore(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reg := metabolism.NewInMemoryRegistry()
	reg.SetAgent(domain.AgentDefinition{ID: "chat_agent", Runtime: domain.RuntimePython, ExecPath: "chat_agent.py"})
	reg.SetAgent(domain.AgentDefinition{ID: "js_agent", Runtime: domain.RuntimeBun, ExecPath: "agent.ts"})
	reg.SetAgent(domain.AgentDefinition{ID: "remote_agent", Runtime: domain.RuntimeA2A, A2AEndpoint: "https://remote.example"})
	// Left by an earlier release's setup/boot minting.
	for _, id := range []string{"chat_agent", "js_agent"} {
		if err := store.SetSecret(authz.AgentTokenSecretName(id), "old-"+id); err != nil {
			t.Fatal(err)
		}
	}
	// An unregistered external SDK agent's secret, set by an operator.
	if err := store.SetSecret(authz.AgentTokenSecretName("external_sdk"), "ext"); err != nil {
		t.Fatal(err)
	}

	for boot := 1; boot <= 2; boot++ { // idempotent: the second boot changes nothing
		ensureAgentCredentials(context.Background(), store, reg)
		for _, id := range []string{"chat_agent", "js_agent"} {
			if store.Configured(authz.AgentTokenSecretName(id)) {
				t.Fatalf("boot %d: kernel-spawned %s still has a stored secret", boot, id)
			}
		}
		if store.Resolve(authz.AgentTokenSecretName("external_sdk"), "") != "ext" {
			t.Fatalf("boot %d: the external agent's secret was touched", boot)
		}
		if !store.Configured(authz.AgentTokenSecretName("remote_agent")) {
			t.Fatalf("boot %d: no credential minted for the A2A agent", boot)
		}
	}
}

// The verifier's predicate: process runtimes are kernel-spawned, a registered
// remote agent and an unknown id are not, an unreadable registry fails closed.
func TestKernelSpawnedAgent(t *testing.T) {
	t.Parallel()
	reg := metabolism.NewInMemoryRegistry()
	reg.SetAgent(domain.AgentDefinition{ID: "chat_agent", Runtime: domain.RuntimePython})
	reg.SetAgent(domain.AgentDefinition{ID: "remote_agent", Runtime: domain.RuntimeA2A})
	reg.SetAgent(domain.AgentDefinition{ID: "llm:gpt", Trait: domain.TraitModel})
	pred := kernelSpawnedAgent(reg)
	ctx := context.Background()
	is := func(id string) bool {
		ok, err := pred(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return ok
	}
	if !is("chat_agent") || is("remote_agent") || is("llm:gpt") || is("external_sdk") {
		t.Fatal("kernel-spawned classification wrong")
	}
	// ENG-2120: an unreadable registry still fails closed (treated as
	// kernel-spawned) AND says why, so the verifier answers Unavailable rather
	// than "bad credential".
	spawned, err := kernelSpawnedAgent(failingLister{})(ctx, "external_sdk")
	if !spawned {
		t.Fatal("an unreadable registry must fail closed (treated as kernel-spawned)")
	}
	if !errors.Is(err, domain.ErrAuthUnavailable) {
		t.Fatalf("unreadable registry: err = %v, want ErrAuthUnavailable", err)
	}
}

type failingLister struct{}

func (failingLister) GetAllAgents(context.Context) ([]domain.AgentDefinition, error) {
	return nil, errors.New("registry down")
}
