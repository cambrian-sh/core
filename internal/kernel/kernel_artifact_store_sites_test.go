package kernel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestKernelArtifactStoreCallSites lists every production call of
// MemoryStack.KernelArtifactStore — the one way to reach the raw, unenforced vector
// store (tier 0, AGENTS.md §2.2) — with the ADR it names (ENG-1476).
//
// A new raw-store use therefore shows up as a change to the list below, where a
// reviewer sees it, instead of as a comment in a file nobody re-reads. If this test
// fails because you added a call: make sure the use is a kernel-authored artifact
// that serves no principal and that the ADR you name permits it, then add the line.
// A principal-serving read or write must use Reader() or Writer() instead.
func TestKernelArtifactStoreCallSites(t *testing.T) {
	want := []string{
		"app/app.go: ADR-0044 D3", // tool index: writer (ToolIndexer) and ranker (VectorToolRetriever)
		"app/app.go: ADR-0046 D2", // skill index writer (SkillIndexer)
	}

	root := filepath.Join("..", "..") // the cambrian-core module root
	var got []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "KernelArtifactStore" {
				return true
			}
			got = append(got, rel+": "+reasonOf(t, fset, call))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("KernelArtifactStore call sites changed (tier-0 raw store uses, AGENTS.md §2.2).\n got: %q\nwant: %q", got, want)
	}
}

// reasonOf returns the literal ADR reference passed to KernelArtifactStore. A reason
// that is not a string literal (a variable, a computed value) is refused: the point
// is that the justification is readable at the call site and in this test.
func reasonOf(t *testing.T, fset *token.FileSet, call *ast.CallExpr) string {
	t.Helper()
	if len(call.Args) != 1 {
		t.Errorf("%s: KernelArtifactStore takes exactly one reason", fset.Position(call.Pos()))
		return "?"
	}
	arg := call.Args[0]
	// Accept domain.ADRRef("…") as well as an untyped "…".
	if conv, ok := arg.(*ast.CallExpr); ok && len(conv.Args) == 1 {
		arg = conv.Args[0]
	}
	lit, ok := arg.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		t.Errorf("%s: KernelArtifactStore reason must be a string literal naming an ADR", fset.Position(call.Pos()))
		return "?"
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Errorf("%s: unquote reason: %v", fset.Position(call.Pos()), err)
		return "?"
	}
	return s
}
