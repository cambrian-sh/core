package llm

import (
	"os"
	"testing"
)

// fakeResolver applies the same env-then-store precedence as the real store.
type fakeResolver struct{ stored map[string]string }

func (f fakeResolver) Resolve(name, envVar string) string {
	if envVar != "" {
		if v := os.Getenv(envVar); v != "" {
			return v
		}
	}
	return f.stored[name]
}

func TestAPIKeyFor_UsesTheStoredCredential(t *testing.T) {
	// The bug this exists for: the console stored a key, reported it installed,
	// showed its last four — and every request went out unauthenticated, because
	// nothing ever read the store. The endpoint answered 401 beside a panel
	// saying a key was configured.
	t.Cleanup(func() { SetSecretResolver(nil) })
	SetSecretResolver(fakeResolver{stored: map[string]string{
		GeneratorKeySecretName("deepseek"): "sk-from-store",
	}})

	if got := APIKeyFor("deepseek", "SOME_UNSET_VAR_XYZ"); got != "sk-from-store" {
		t.Fatalf("a stored credential must reach the request, got %q", got)
	}
}

func TestAPIKeyFor_EnvironmentOutranksTheStore(t *testing.T) {
	// ADR-0101 D5, and the same precedence the console reports in key_source. If
	// the two disagreed, the console would name a source the kernel does not use.
	t.Setenv("CAMBRIAN_TEST_KEY", "sk-from-env")
	t.Cleanup(func() { SetSecretResolver(nil) })
	SetSecretResolver(fakeResolver{stored: map[string]string{
		GeneratorKeySecretName("deepseek"): "sk-from-store",
	}})

	if got := APIKeyFor("deepseek", "CAMBRIAN_TEST_KEY"); got != "sk-from-env" {
		t.Fatalf("a deployment's variable must win, got %q", got)
	}
}

func TestAPIKeyFor_FallsBackToTheEnvironmentWithNoStore(t *testing.T) {
	// An OSS kernel with no config store must keep working exactly as before.
	t.Setenv("CAMBRIAN_TEST_KEY", "sk-from-env")
	SetSecretResolver(nil)
	if got := APIKeyFor("deepseek", "CAMBRIAN_TEST_KEY"); got != "sk-from-env" {
		t.Fatalf("the environment path must survive without a store, got %q", got)
	}
}

func TestAPIKeyFor_NoGeneratorIDStillReadsTheEnvironment(t *testing.T) {
	// A model-agent client is configured straight from config and has no store
	// entry of its own.
	t.Setenv("CAMBRIAN_TEST_KEY", "sk-from-env")
	t.Cleanup(func() { SetSecretResolver(nil) })
	SetSecretResolver(fakeResolver{stored: map[string]string{}})
	if got := APIKeyFor("", "CAMBRIAN_TEST_KEY"); got != "sk-from-env" {
		t.Fatalf("got %q", got)
	}
}

func TestAPIKeyFor_NothingConfiguredIsEmptyNotAPanic(t *testing.T) {
	t.Cleanup(func() { SetSecretResolver(nil) })
	SetSecretResolver(fakeResolver{stored: map[string]string{}})
	if got := APIKeyFor("nobody", ""); got != "" {
		t.Fatalf("got %q", got)
	}
}
