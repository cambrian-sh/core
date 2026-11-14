package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// configLeaf is one leaf field of Config: its flat koanf key and its Go field name.
type configLeaf struct {
	key, field string
}

// configLeaves walks Config by json tags, the same names koanf merges by, and returns
// every leaf (a field that is not itself a struct). Maps and slices are leaves.
func configLeaves() []configLeaf {
	var out []configLeaf
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if f.Anonymous && name == "" && ft.Kind() == reflect.Struct {
				walk(ft, prefix)
				continue
			}
			if name == "" {
				name = f.Name
			}
			key := name
			if prefix != "" {
				key = prefix + "." + name
			}
			if ft.Kind() == reflect.Struct && ft.PkgPath() != "time" {
				walk(ft, key)
				continue
			}
			out = append(out, configLeaf{key: key, field: f.Name})
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	return out
}

// goSourcesOutsideConfig returns the non-test Go sources of core outside
// internal/config, plus premium's when the workspace sibling is present: the places a
// config field can be read from.
func goSourcesOutsideConfig(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	roots := []string{filepath.Join("..", "..")}
	// Premium is scanned when it is checked out beside core (the go.work workspace);
	// core CI has no premium, so a key only premium reads would fail there — none
	// does today (checked 2026-11-13 with CONFIG_CONSUMERS_CORE_ONLY=1).
	if _, err := os.Stat(filepath.Join("..", "..", "..", "cambrian-premium", "go.mod")); err == nil && os.Getenv("CONFIG_CONSUMERS_CORE_ONLY") == "" {
		roots = append(roots, filepath.Join("..", "..", "..", "cambrian-premium"))
	}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "testdata", "vendor":
					return filepath.SkipDir
				}
				if filepath.ToSlash(p) == filepath.ToSlash(filepath.Join("..", "..", "internal", "config")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			src, rerr := os.ReadFile(p)
			if rerr == nil {
				b.Write(src)
				b.WriteByte('\n')
			}
			return nil
		})
	}
	return b.String()
}

func hasSelector(src, field string) bool {
	return regexp.MustCompile(`\.` + regexp.QuoteMeta(field) + `\b`).MatchString(src)
}

// Every inert key must still be declared (a Config leaf the defaults carry) and still
// unread. A key that gains a reader must leave inertConfigKeys in the same change, or
// the boot warning would tell the operator a working key does nothing.
func TestInertConfigKeys_AreDeclaredAndUnread(t *testing.T) {
	leaves := map[string]string{}
	for _, l := range configLeaves() {
		leaves[l.key] = l.field
	}
	src := goSourcesOutsideConfig(t)
	for key := range inertConfigKeys {
		field, ok := leaves[key]
		if !ok {
			t.Errorf("inert key %q is not a Config leaf any more; remove it from inertConfigKeys", key)
			continue
		}
		if hasSelector(src, field) {
			t.Errorf("inert key %q (field %s) now has a reader; remove it from inertConfigKeys", key, field)
		}
	}
}

func TestSetInertConfigKeys_OnlyAboveDefaults(t *testing.T) {
	t.Parallel()
	prov := Provenance{
		"execution.plan.max_recursion_depth":         SourceDefault,
		"execution.routing.proposal_timeout_ms":      SourceTuningLocal,
		"execution.llm.llm_gateway_retry_backoff_ms": EnvSource("CAMBRIAN_EXECUTION__LLM__LLM_GATEWAY_RETRY_BACKOFF_MS"),
	}
	got := setInertConfigKeys(prov)
	want := []string{
		"execution.llm.llm_gateway_retry_backoff_ms (env:CAMBRIAN_EXECUTION__LLM__LLM_GATEWAY_RETRY_BACKOFF_MS)",
		"execution.routing.proposal_timeout_ms (tuning.local.json)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// accessorFieldReads maps each config field name to true when it is read inside a
// method of a config type whose name some code outside internal/config calls — the
// accessor pattern this package uses for defaults (`func (c PlanConfig)
// CoverageMax() int { if c.CoverageMaxStages > 0 … }`). Validation methods do not count:
// a rule that checks a value nobody uses is exactly the defect being caught.
func accessorFieldReads(t *testing.T, outside string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// Selector names used in the body of each exported, non-validation method.
	bodies := map[string]map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || !fn.Name.IsExported() {
				continue
			}
			if strings.HasPrefix(fn.Name.Name, "Validate") {
				continue
			}
			sels := bodies[fn.Name.Name]
			if sels == nil {
				sels = map[string]bool{}
				bodies[fn.Name.Name] = sels
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					sels[sel.Sel.Name] = true
				}
				return true
			})
		}
	}
	// Live accessors: called from outside, or (transitively) from a live accessor
	// (DependencyBudgetBytes → DependencyBudgetTokens).
	live := map[string]bool{}
	for m := range bodies {
		if hasSelector(outside, m) {
			live[m] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for m := range live {
			for sel := range bodies[m] {
				if _, isMethod := bodies[sel]; isMethod && !live[sel] {
					live[sel], changed = true, true
				}
			}
		}
	}
	reads := map[string]bool{}
	for m := range live {
		for sel := range bodies[m] {
			reads[sel] = true
		}
	}
	return reads
}

// unreadConfigLeaves returns the Config leaves that nothing reads: no selector on the
// field outside internal/config, and no exported accessor reading it that is called
// from outside. Name-based, so it can MISS an unread field whose name another type
// shares; it cannot report a field that is read.
func unreadConfigLeaves(t *testing.T) []string {
	t.Helper()
	src := goSourcesOutsideConfig(t)
	viaAccessor := accessorFieldReads(t, src)
	var unread []string
	for _, l := range configLeaves() {
		if !hasSelector(src, l.field) && !viaAccessor[l.field] {
			unread = append(unread, l.key)
		}
	}
	sort.Strings(unread)
	return unread
}

// AGENTS.md §3.4: "A config field that is parsed and validated must be threaded to a
// consumer in the same change." This has been broken repeatedly
// (T-20261113-add-a-reflective-every-config-key-has-a). Every Config leaf must have a
// reader in core or premium; the only exceptions are the ticketed inertConfigKeys,
// which the kernel also names at load. A new unread key fails here: wire it in the
// same change, or (with the owner) declare it inert with its ticket.
func TestEveryConfigKeyHasAConsumer(t *testing.T) {
	for _, key := range unreadConfigLeaves(t) {
		if _, ok := inertConfigKeys[key]; !ok {
			t.Errorf("config key %q is declared but nothing reads it (AGENTS.md §3.4)", key)
		}
	}
}
