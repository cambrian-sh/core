package archlint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The architecture rules of AGENTS.md §1.3 / §2.1 / §2.2 / §2.7 that used to be
// review-only, enforced on the real module (ENG-1484). Each rule extends the
// go/parser pattern of the existing guards (TestKernelArtifactStoreCallSites,
// internal/infrastructure/postgres/layering_test.go, internal/objects/arch_test.go)
// instead of adding a second mechanism; `make archlint` runs this package and
// is part of `make per-pr` and the CI verify job.
//
// A failure names the file, the rule and the allowlist key. Fix the code; an
// exception is justified only by an ADR or tracked by a ticket, and then goes
// in allowlist_test.go with that citation. Entries that stop matching fail the
// test too, so the lists only shrink.

func moduleRoot(t *testing.T) (root, mod string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return root, strings.TrimSpace(m)
		}
	}
	t.Fatal("go.mod has no module line")
	return "", ""
}

func TestArch_ImportLayering(t *testing.T) {
	root, mod := moduleRoot(t)
	check(t, "imports", ruleImports(loadTree(t, root), mod), allowImports)
}

func TestArch_RestrictedConstructors(t *testing.T) {
	root, mod := moduleRoot(t)
	check(t, "constructors", ruleRestrictedConstructors(loadTree(t, root), mod), allowConstructors)
}

func TestArch_DomainPortsTakeContext(t *testing.T) {
	root, _ := moduleRoot(t)
	check(t, "domain-ctx", ruleDomainPortContext(loadTree(t, root)), allowDomainCtx)
}

func TestArch_NoBackgroundContextOnRequestPaths(t *testing.T) {
	root, _ := moduleRoot(t)
	check(t, "background", ruleBackgroundContext(loadTree(t, root)), allowBackground)
}
