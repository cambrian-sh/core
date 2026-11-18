package archlint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The rule engine. Pure functions over parsed source, so the same rules run on
// the real module (arch_test.go) and on the planted-violation tree in
// testdata/planted (planted_test.go), which proves each rule can fail.

// srcFile is one parsed non-test Go file of the tree under analysis.
type srcFile struct {
	rel     string            // slash path relative to the module root
	dir     string            // slash dir relative to the module root ("." for the root)
	ast     *ast.File         //
	fset    *token.FileSet    //
	imports map[string]string // local name -> import path
}

// finding is one rule violation. key is what the allowlist matches: it names
// the file (and the symbol where one applies) but never a line, so an entry
// survives unrelated edits above it.
type finding struct {
	rule string
	key  string
	pos  string
	msg  string
}

// plantedExt is the extra extension of the planted-violation sources.
const plantedExt = ".planted"

// loadTree parses every non-test .go (or .go.planted) file under root, skipping testdata,
// vendor and hidden directories (go build ignores them too).
func loadTree(t *testing.T, root string) []srcFile {
	t.Helper()
	var out []srcFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if p != root && (n == "testdata" || n == "vendor" || n == "node_modules" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		// The planted tree's sources end in ".go.planted": they import a fake
		// module, so they must not be Go files any tool would try to build.
		src := strings.TrimSuffix(p, plantedExt)
		if !strings.HasSuffix(src, ".go") || strings.HasSuffix(src, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution|parser.ParseComments)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, src)
		rel = filepath.ToSlash(rel)
		sf := srcFile{rel: rel, dir: path.Dir(rel), ast: f, fset: fset, imports: map[string]string{}}
		for _, imp := range f.Imports {
			ip, _ := strconv.Unquote(imp.Path.Value)
			name := path.Base(ip)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			sf.imports[name] = ip
		}
		out = append(out, sf)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func (f srcFile) at(n ast.Node) string {
	return f.rel + ":" + strconv.Itoa(f.fset.Position(n.Pos()).Line)
}

func under(dir, prefix string) bool { return dir == prefix || strings.HasPrefix(dir, prefix+"/") }

func pathUnder(ip, prefix string) bool { return ip == prefix || strings.HasPrefix(ip, prefix+"/") }

// isStdlib reports a standard-library import path: no dot in its first element.
func isStdlib(ip string) bool {
	first, _, _ := strings.Cut(ip, "/")
	return !strings.Contains(first, ".")
}

// ── Rule 1: import layering (AGENTS.md §1.2, §1.3) ──────────────────────────

const premiumModule = "github.com/cambrian-sh/cambrian-premium"

// domainThirdParty is what domain/ may import besides the standard library and
// the module's own pkg/ (the dependency-free public layer): domain holds ports
// and entities and imports nothing internal (§1.3).
var domainThirdParty = []string{"github.com/google/uuid"}

// infraForbidden: a driven adapter implements domain ports; it never reaches
// into the services that use it (§1.3, ENG-1104).
var infraForbidden = []string{"internal/memory", "internal/substrate", "internal/metabolism", "internal/kernel", "internal/supervision"}

func ruleImports(files []srcFile, mod string) []finding {
	var out []finding
	add := func(f srcFile, imp *ast.ImportSpec, ip, why string) {
		out = append(out, finding{rule: "imports", key: f.rel + " -> " + ip, pos: f.at(imp), msg: why})
	}
	for _, f := range files {
		for _, imp := range f.ast.Imports {
			ip, _ := strconv.Unquote(imp.Path.Value)
			if pathUnder(ip, premiumModule) {
				add(f, imp, ip, "core never imports premium (ADR-0057, AGENTS.md §1.2)")
			}
			if under(f.dir, "domain") && !isStdlib(ip) && !pathUnder(ip, mod+"/pkg") && !slices.Contains(domainThirdParty, ip) {
				add(f, imp, ip, "domain/ imports only the standard library, "+strings.Join(domainThirdParty, ", ")+" and pkg/ (AGENTS.md §1.3)")
			}
			if (under(f.dir, "domain") || under(f.dir, "internal")) && (pathUnder(ip, mod+"/app") || pathUnder(ip, mod+"/cmd")) {
				add(f, imp, ip, "domain/ and internal/ never import app/ or cmd/: app is the composition root (AGENTS.md §1.3)")
			}
			if pathUnder(ip, mod+"/internal/kernel") && !under(f.dir, "app") && !under(f.dir, "cmd") && !under(f.dir, "internal/kernel") {
				add(f, imp, ip, "only app/ (the one composition root) and cmd/ tools import internal/kernel (AGENTS.md §1.3)")
			}
			if under(f.dir, "internal/infrastructure") {
				for _, svc := range infraForbidden {
					if pathUnder(ip, mod+"/"+svc) {
						add(f, imp, ip, "a driven adapter under internal/infrastructure never imports the "+svc+" service; declare the port in domain/ (AGENTS.md §1.3, ENG-1104)")
					}
				}
			}
		}
	}
	return out
}

// ── Rule 2: restricted constructors (AGENTS.md §2.1) ────────────────────────

type restricted struct {
	pkg, name string
	allowed   func(f srcFile) bool
}

func restrictedCtors(mod string) []restricted {
	stack := func(f srcFile) bool { return f.rel == "internal/kernel/memory_stack.go" }
	return []restricted{
		// The raw adapter is opened once by the composition root; an operator-run
		// cmd/ maintenance tool may open its own (§2.2, checked for a citation below).
		{mod + "/internal/infrastructure/postgres", "NewPgVectorAdapter", func(f srcFile) bool {
			return f.rel == "app/app.go" || under(f.dir, "cmd")
		}},
		{mod + "/internal/authz", "NewEnforcingVectorStore", stack},
		{mod + "/internal/authz", "NewEnforcingStoreWriter", stack},
		{mod + "/internal/memory", "NewEnforcingDocumentStore", stack},
	}
}

// citation is what §2.2 asks a cmd/ tool that opens its own adapter to carry.
var citation = regexp.MustCompile(`ADR-\d{4}|ENG-\d+|T-20\d{6}-`)

func ruleRestrictedConstructors(files []srcFile, mod string) []finding {
	var out []finding
	ctors := restrictedCtors(mod)
	for _, f := range files {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var pkg, name string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				id, ok := fn.X.(*ast.Ident)
				if !ok {
					return true
				}
				pkg, name = f.imports[id.Name], fn.Sel.Name
			case *ast.Ident: // a call inside the defining package itself
				pkg, name = mod+"/"+f.dir, fn.Name
			default:
				return true
			}
			for _, c := range ctors {
				if c.pkg != pkg || c.name != name {
					continue
				}
				if !c.allowed(f) {
					out = append(out, finding{rule: "constructors", key: f.rel + " calls " + path.Base(c.pkg) + "." + c.name, pos: f.at(call),
						msg: path.Base(c.pkg) + "." + c.name + " is constructed only in the memory composition point (AGENTS.md §2.1); take the store as a constructor parameter"})
				} else if under(f.dir, "cmd") && !commentAbove(f, call, 15) {
					out = append(out, finding{rule: "constructors", key: f.rel + " calls " + path.Base(c.pkg) + "." + c.name + " without a citation", pos: f.at(call),
						msg: "an operator cmd/ tool that opens its own adapter carries a comment citing the ADR/ticket that justifies it (AGENTS.md §2.2)"})
				}
			}
			return true
		})
	}
	return out
}

// commentAbove reports a comment citing an ADR or ticket within lines above n.
func commentAbove(f srcFile, n ast.Node, lines int) bool {
	at := f.fset.Position(n.Pos()).Line
	for _, cg := range f.ast.Comments {
		l := f.fset.Position(cg.End()).Line
		if l <= at && at-l <= lines && citation.MatchString(cg.Text()) {
			return true
		}
	}
	return false
}

// ── Rule 3: ctx-first I/O methods on domain ports (AGENTS.md §2.7) ──────────

func ruleDomainPortContext(files []srcFile) []finding {
	var out []finding
	for _, f := range files {
		if !under(f.dir, "domain") {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				return true
			}
			for _, m := range it.Methods.List {
				ft, ok := m.Type.(*ast.FuncType)
				if !ok || len(m.Names) == 0 || !returnsError(ft) || takesContextFirst(f, ft) {
					continue
				}
				out = append(out, finding{rule: "domain-ctx", key: "domain." + ts.Name.Name + "." + m.Names[0].Name, pos: f.at(m),
					msg: "a domain port method that can fail does I/O: it takes ctx context.Context first (AGENTS.md §2.7, ENG-1466)"})
			}
			return true
		})
	}
	return out
}

func returnsError(ft *ast.FuncType) bool {
	if ft.Results == nil {
		return false
	}
	for _, r := range ft.Results.List {
		if id, ok := r.Type.(*ast.Ident); ok && id.Name == "error" {
			return true
		}
	}
	return false
}

func takesContextFirst(f srcFile, ft *ast.FuncType) bool {
	if ft.Params == nil || len(ft.Params.List) == 0 {
		return false
	}
	sel, ok := ft.Params.List[0].Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Context" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && f.imports[id.Name] == "context"
}

// ── Rule 4: context.Background / TODO outside an allowlist (AGENTS.md §2.7) ─

func ruleBackgroundContext(files []srcFile) []finding {
	var out []finding
	for _, f := range files {
		if !under(f.dir, "app") && !under(f.dir, "domain") && !under(f.dir, "internal") {
			continue // cmd/ mains and tools own their root contexts
		}
		ctxName := ""
		for name, ip := range f.imports {
			if ip == "context" {
				ctxName = name
			}
		}
		if ctxName == "" {
			continue
		}
		seen := map[string]int{}
		for _, decl := range f.ast.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			var body ast.Node = decl
			owner := "(package scope)"
			if ok {
				owner = funcName(fn)
				if fn.Body == nil {
					continue
				}
				body = fn.Body
			}
			ast.Inspect(body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Background" && sel.Sel.Name != "TODO") {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == ctxName {
					// The k-th call in its function: one allowlist entry per call, so a
					// new Background beside an allowed one is still reported.
					seen[owner]++
					key := f.rel + " " + owner + " #" + strconv.Itoa(seen[owner])
					out = append(out, finding{rule: "background", key: key, pos: f.at(call),
						msg: "context." + sel.Sel.Name + "() drops the caller's deadline, cancellation and scope: pass ctx, or context.WithoutCancel(ctx) for work that outlives the request (AGENTS.md §2.7)"})
				}
				return true
			})
		}
	}
	return out
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok {
		t = ix.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// ── Allowlist application ───────────────────────────────────────────────────

// reasonRef is what every allowlist reason must cite, so an entry points at the
// ticket that removes it or the ADR that permits it.
var reasonRef = regexp.MustCompile(`ENG-\d+|ADR-\d{4}|T-20\d{6}-`)

// reporter is the part of *testing.T that check uses, so the planted test can
// prove check itself fails.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

type recorder struct{ failed bool }

func (r *recorder) Helper()               {}
func (r *recorder) Errorf(string, ...any) { r.failed = true }

// check fails t for every finding not in allow, every allowlist entry that no
// longer matches (the list only shrinks), and every reason without a citation.
func check(t *testing.T, rule string, got []finding, allow map[string]string) {
	t.Helper()
	checkInto(t, rule, got, allow)
}

func checkInto(t reporter, rule string, got []finding, allow map[string]string) {
	t.Helper()
	used := map[string]bool{}
	var bad []string
	for _, f := range got {
		if _, ok := allow[f.key]; ok {
			used[f.key] = true
			continue
		}
		bad = append(bad, f.pos+": "+f.msg+"\n    allowlist key: "+strconv.Quote(f.key))
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("[%s] %s", rule, b)
	}
	var keys []string
	for k := range allow {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !reasonRef.MatchString(allow[k]) {
			t.Errorf("[%s] allowlist entry %q: its reason must cite a ticket (ENG-…/T-…) or an ADR", rule, k)
		}
		if !used[k] {
			t.Errorf("[%s] allowlist entry %q matches nothing any more: delete it (the list only shrinks)", rule, k)
		}
	}
}
