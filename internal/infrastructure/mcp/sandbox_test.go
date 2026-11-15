package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/sandbox"
)

// testSandboxSpec launches a test fixture at first-party, rooted in a test
// directory: the fixtures are this package's own test binary.
func testSandboxSpec(t *testing.T, id string) sandbox.Spec {
	t.Helper()
	p := sandbox.DefaultPolicy()
	p.WorkRoot = t.TempDir()
	return p.Resolve(id, string(sandbox.TierFirstParty), "", false, sandbox.ServerSpec{})
}

// TestStdio_ContainmentIsReported: a launched stdio server's effective
// containment is readable from the connector (the console's status source).
func TestStdio_ContainmentIsReported(t *testing.T) {
	conn := NewConnector(CredentialSources{})
	defer conn.Close()
	tools := conn.ConnectAll(context.Background(), []ServerConfig{envServer(t, "contained")})
	if len(tools) == 0 {
		t.Fatal("the contained fixture server did not come up")
	}
	rep, ok := conn.Containment("contained")
	if !ok {
		t.Fatal("no containment recorded for a launched stdio server")
	}
	if rep.Tier != sandbox.TierFirstParty || rep.Refused || rep.ServerID != "contained" {
		t.Fatalf("report: %+v", rep)
	}
	for _, c := range []string{"env", "workdir", "memory", "kill_tree"} {
		if !strings.Contains(rep.Summary(), c) {
			t.Fatalf("summary lacks %s: %s", c, rep.Summary())
		}
	}
}

// TestStdio_UnresolvedSpecFailsClosed: a stdio server built with no resolved
// sandbox spec (a launch path that forgot the composition root) launches at
// the strict default tier. Where that tier's controls are not all available
// (no native egress control on Windows; Linux without bubblewrap) it is
// refused, the refusal is reported, and no tools register.
func TestStdio_UnresolvedSpecFailsClosed(t *testing.T) {
	conn := NewConnector(CredentialSources{})
	defer conn.Close()
	s := envServer(t, "unresolved")
	s.Sandbox = sandbox.Spec{}
	planned := sandbox.Plan(context.Background(), sandbox.Spec{ServerID: s.ID}, sandbox.Command{Path: s.Endpoint})
	tools := conn.ConnectAll(context.Background(), []ServerConfig{s})
	rep, ok := conn.Containment("unresolved")
	if !ok || rep.Tier != sandbox.TierThirdParty || rep.TierSource != "unresolved" {
		t.Fatalf("report: %+v (ok=%v)", rep, ok)
	}
	if len(planned.Missing) == 0 {
		t.Skip("this platform gives third-party every control natively")
	}
	if len(tools) != 0 || !rep.Refused {
		t.Fatalf("an unresolved third-party server must be refused here: tools=%d rep=%+v", len(tools), rep)
	}
	if h, ok := conn.Health("unresolved"); ok && !strings.Contains(h.LastError, "sandbox refused") {
		t.Fatalf("health must carry the refusal: %+v", h)
	}
}
