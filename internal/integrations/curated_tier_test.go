package integrations

import "testing"

// Owner decision 2026-11-06: code-sandbox is a curated (reviewed + pinned) server
// so it runs at the vetted containment tier (T-20261106-mark-code-sandbox-vetted-add-a-curated-catalog).
func TestBuiltinCatalog_CodeSandboxIsCurated(t *testing.T) {
	c, err := Load("") // builtin catalog only
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	e, ok := c.Get("code-sandbox")
	if !ok {
		t.Fatal("code-sandbox missing from the builtin catalog")
	}
	if e.TrustTier != TrustCurated {
		t.Fatalf("code-sandbox trust_tier = %q, want %q", e.TrustTier, TrustCurated)
	}
}
