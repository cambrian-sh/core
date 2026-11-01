package app

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/authz"
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
