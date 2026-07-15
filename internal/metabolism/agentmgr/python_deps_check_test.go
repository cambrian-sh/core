package agentmgr

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// PLAT-01: the dependency self-check passes for installed modules and fails with
// the missing module named for absent ones. Needs a Python interpreter; skipped
// where none is on PATH.
func TestCheckPythonDeps(t *testing.T) {
	py, err := exec.LookPath("python")
	if err != nil {
		if py, err = exec.LookPath("python3"); err != nil {
			t.Skip("no python interpreter on PATH")
		}
	}
	ctx := context.Background()

	// Empty deps ⇒ always nil (no check).
	if err := checkPythonDeps(ctx, py, nil); err != nil {
		t.Errorf("empty deps should pass, got %v", err)
	}
	// Stdlib modules are always importable.
	if err := checkPythonDeps(ctx, py, []string{"sys", "json"}); err != nil {
		t.Errorf("stdlib deps should pass, got %v", err)
	}
	// A missing module must fail, and the error must name it.
	missing := "definitely_not_a_real_module_plat01_xyz"
	err = checkPythonDeps(ctx, py, []string{"sys", missing})
	if err == nil {
		t.Fatal("expected failure for a missing module")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error should name the missing module %q, got %v", missing, err)
	}
}

func TestPyStrList_StripsQuotesForSafety(t *testing.T) {
	got := pyStrList([]string{"docling", "to'rch\"", "a\\b"})
	if strings.ContainsAny(got, "\"\\") || strings.Count(got, "'")%2 != 0 {
		t.Errorf("pyStrList must produce a safe balanced literal, got %s", got)
	}
}
