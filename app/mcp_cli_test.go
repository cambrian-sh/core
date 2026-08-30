package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
	"github.com/cambrian-sh/core/internal/storage"
)

// The token lifecycle is OFFLINE against the ADR-0101 store (phase 1): create
// once, refuse a silent overwrite, rotate explicitly, revoke. The store path is
// per-test via the env override, which is exactly the relocation seam it exists
// to provide.

func mcpCLIStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.db")
	t.Setenv(ConfigStoreEnv, path)
	return path
}

func TestRunMCP_TokenCreateIssuesADurableCredential(t *testing.T) {
	path := mcpCLIStore(t)

	if code := RunMCP(context.Background(), []string{"token", "create", "ci-bot"}); code != 0 {
		t.Fatalf("token create = exit %d", code)
	}

	// Durable: a fresh open (the next kernel boot) sees the credential.
	store, err := storage.OpenConfigStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	if !store.Configured(mcpserve.ClientSecretName("ci-bot")) {
		t.Fatal("created token is not in the store")
	}
	if got := store.Resolve(mcpserve.ClientSecretName("ci-bot"), ""); len(got) < 30 {
		t.Fatalf("stored token is implausibly short: %d chars", len(got))
	}
}

func TestRunMCP_TokenCreateRefusesASilentOverwrite(t *testing.T) {
	mcpCLIStore(t)

	if code := RunMCP(context.Background(), []string{"token", "create", "ci-bot"}); code != 0 {
		t.Fatalf("first create = exit %d", code)
	}
	// A second create must NOT silently rotate: every configured client's MCP
	// config would go stale with nothing to say why.
	if code := RunMCP(context.Background(), []string{"token", "create", "ci-bot"}); code == 0 {
		t.Fatal("second create succeeded without --rotate")
	}
	if code := RunMCP(context.Background(), []string{"token", "create", "ci-bot", "--rotate"}); code != 0 {
		t.Fatal("--rotate refused")
	}
}

func TestRunMCP_TokenRevokeClearsTheCredential(t *testing.T) {
	path := mcpCLIStore(t)

	if code := RunMCP(context.Background(), []string{"token", "create", "ci-bot"}); code != 0 {
		t.Fatal("create failed")
	}
	if code := RunMCP(context.Background(), []string{"token", "revoke", "ci-bot"}); code != 0 {
		t.Fatal("revoke failed")
	}
	store, err := storage.OpenConfigStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	if store.Configured(mcpserve.ClientSecretName("ci-bot")) {
		t.Fatal("revoked credential still present")
	}
}

// ADR-0127 D1: `--owner` binds a machine credential to its owner principal at
// issuance — durably (a fresh store open sees it, like the token itself),
// preserved across a plain rotate, re-bound by a rotate WITH --owner, and
// revoked with the credential (central revocation covers both facts).
func TestRunMCP_TokenCreateWithOwnerBindsAWorkerDurably(t *testing.T) {
	path := mcpCLIStore(t)

	if code := RunMCP(context.Background(), []string{"token", "create", "afsin-laptop", "--owner", "owner-afsin"}); code != 0 {
		t.Fatalf("worker create = exit %d", code)
	}
	store, err := storage.OpenConfigStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if got := store.Resolve(mcpserve.WorkerOwnerSecretName("afsin-laptop"), ""); got != "owner-afsin" {
		t.Fatalf("owner binding = %q, want owner-afsin", got)
	}
	store.Close()

	// A plain rotate re-issues the token and KEEPS the fleet membership.
	if code := RunMCP(context.Background(), []string{"token", "create", "afsin-laptop", "--rotate"}); code != 0 {
		t.Fatal("rotate failed")
	}
	// A rotate WITH --owner re-binds: issuance is where ownership lives.
	if code := RunMCP(context.Background(), []string{"token", "create", "afsin-laptop", "--rotate", "--owner", "owner-b"}); code != 0 {
		t.Fatal("re-binding rotate failed")
	}
	store, err = storage.OpenConfigStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if got := store.Resolve(mcpserve.WorkerOwnerSecretName("afsin-laptop"), ""); got != "owner-b" {
		t.Fatalf("owner binding after re-bind = %q, want owner-b", got)
	}
	store.Close()

	if code := RunMCP(context.Background(), []string{"token", "revoke", "afsin-laptop"}); code != 0 {
		t.Fatal("revoke failed")
	}
	store, err = storage.OpenConfigStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	if store.Configured(mcpserve.WorkerOwnerSecretName("afsin-laptop")) {
		t.Fatal("revoke left the owner binding behind — a revoked worker must vanish from fleet resolution")
	}
}

func TestRunMCP_TokenCreateRejectsAMalformedOwner(t *testing.T) {
	mcpCLIStore(t)
	if code := RunMCP(context.Background(), []string{"token", "create", "afsin-laptop", "--owner", "two words"}); code == 0 {
		t.Fatal("an owner principal with whitespace was accepted")
	}
}

func TestRunMCP_RejectsBadInput(t *testing.T) {
	mcpCLIStore(t)
	for name, args := range map[string][]string{
		"no subcommand":     {},
		"unknown":           {"frobnicate"},
		"bad client name":   {"token", "create", "CI_Bot"},
		"leading dash name": {"token", "create", "-bot"},
		"bridge sans creds": {"bridge"},
	} {
		if code := RunMCP(context.Background(), args); code == 0 {
			t.Errorf("%s: exit 0, want failure", name)
		}
	}
}

func TestValidMCPClientName(t *testing.T) {
	for name, want := range map[string]bool{
		"ci-bot": true, "afsin-laptop": true, "a": true,
		"": false, "CI": false, "9bot": false, "-x": false, "a.b": false,
		"waaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaay-too-long-for-a-client": false,
	} {
		if got := validMCPClientName(name); got != want {
			t.Errorf("validMCPClientName(%q) = %v, want %v", name, got, want)
		}
	}
}
