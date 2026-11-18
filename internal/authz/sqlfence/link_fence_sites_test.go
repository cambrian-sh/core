package sqlfence

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The link fence is rendered once, by LinkExists (ENG-2049, owner decision
// 2026-11-18): a link is visible only to a reader allowed to read EVERY evidence
// row it rests on — its cited evidence (as a typed row) and each id in
// links.supporting_evidence. A link fenced with TypedRowExists (or the
// evidence-only fence) alone would skip the supporting rows and show a derived
// link to a reader barred from one of them. Two checks keep every SQL link fence
// on LinkExists:
//
//  1. TestLinkFencesUseLinkExists — every function whose SQL reads the links
//     table AND renders an evidence-plane fence also renders the link fence
//     (LinkExists or its wrappers). A function that reads links with no SQL
//     fence at all is the store, or filters in Go through the query plane's
//     filterLinks (linkRefs, the walk mirror).
//  2. TestTypedRowFenceSites — the inventory of every typed-row fence call and
//     the column it fences: none is a link column. A new TypedRowExists call
//     fails this test until it is added here, so a link site cannot slip in.

// fenceCall is one production call of a fence renderer: the file, the function
// it sits in, the renderer's name and its column argument as source text.
type fenceCall struct {
	file, fn, name, col string
}

// The renderers, and which argument names the fenced column (or alias).
var (
	linkFences     = map[string]int{"LinkExists": 1, "viaLink": 1, "linkVisible": 0}
	typedRowFences = map[string]int{"TypedRowExists": 1, "viaTypedRow": 1, "typedRowVisible": 0}
	evidenceFences = map[string]int{"EvidenceExists": 1, "viaEvidence": 1, "evidenceVisible": 0}
)

// scanFenceSites walks the module's production Go files and returns every fence
// call, and per function whether its string literals read the links table.
func scanFenceSites(t *testing.T) (calls []fenceCall, readsLinks map[string]bool) {
	t.Helper()
	root := filepath.Join("..", "..", "..") // the cambrian-core module root
	readsLinks = map[string]bool{}
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
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := rel + ": " + fd.Name.Name
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BasicLit:
					if x.Kind == token.STRING {
						if s, err := strconv.Unquote(x.Value); err == nil && readsLinksTable(s) {
							readsLinks[fn] = true
						}
					}
				case *ast.CallExpr:
					var name string
					switch c := x.Fun.(type) {
					case *ast.SelectorExpr:
						name = c.Sel.Name
					case *ast.Ident:
						name = c.Name
					}
					for _, set := range []map[string]int{linkFences, typedRowFences, evidenceFences} {
						if i, ok := set[name]; ok && len(x.Args) > i {
							calls = append(calls, fenceCall{file: rel, fn: fn, name: name, col: types.ExprString(x.Args[i])})
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return calls, readsLinks
}

// readsLinksTable reports whether a SQL fragment reads the links table.
func readsLinksTable(s string) bool {
	f := strings.Join(strings.Fields(s), " ")
	return strings.Contains(f, "FROM links") || strings.Contains(f, "JOIN links")
}

func TestLinkFencesUseLinkExists(t *testing.T) {
	calls, readsLinks := scanFenceSites(t)
	fenced := map[string]bool{}   // function renders any evidence-plane fence
	linkSafe := map[string]bool{} // function renders the link fence
	for _, c := range calls {
		fenced[c.fn] = true
		if _, ok := linkFences[c.name]; ok {
			linkSafe[c.fn] = true
		}
	}
	var bad []string
	for fn := range readsLinks {
		if fenced[fn] && !linkSafe[fn] {
			bad = append(bad, fn)
		}
	}
	slices.Sort(bad)
	if len(bad) > 0 {
		t.Errorf("functions read the links table under an evidence-plane fence without the link fence "+
			"(ENG-2049: fence a link with sqlfence.LinkExists — or viaLink / linkVisible — so its supporting "+
			"evidence is checked too):\n%s", strings.Join(bad, "\n"))
	}
	// The check must have something to check: the known link fence sites.
	for _, want := range []string{
		"internal/infrastructure/postgres/identity_scan_source.go: ConfirmedIdentityLinks",
		"internal/infrastructure/postgres/identity_scan_source.go: CandidateLinksSince",
		"internal/objects/get.go: readLinks",
		"internal/objects/plan.go: link",
		"internal/ontology/violations.go: endpoint",
		"internal/ontology/violations.go: cardinality",
		"internal/ontology/violations.go: requiredLink",
	} {
		if !readsLinks[want] || !linkSafe[want] {
			t.Errorf("%s: expected to read links under LinkExists (reads=%v, link fence=%v)", want, readsLinks[want], linkSafe[want])
		}
	}
}

func TestTypedRowFenceSites(t *testing.T) {
	calls, _ := scanFenceSites(t)
	seen := map[string]bool{}
	var got []string
	for _, c := range calls {
		if _, ok := typedRowFences[c.name]; ok {
			k := c.file + ": " + c.name + "(" + c.col + ")"
			if !seen[k] {
				seen[k] = true
				got = append(got, k)
			}
		}
	}
	// Every typed-row fence call in production code, with the column it fences
	// (one entry per distinct file, renderer and column).
	// None is a link (links use LinkExists, which renders TypedRowExists over the
	// cited evidence itself). Adding a TypedRowExists call: if it fences a link,
	// use LinkExists; otherwise add it here.
	want := []string{
		// The renderers and wrappers (pass-through).
		"internal/authz/sqlfence/sqlfence.go: TypedRowExists(alias + \".evidence_id\")",
		"internal/infrastructure/postgres/query_plane_catalog.go: TypedRowExists(refCol)",
		"internal/objects/plan.go: TypedRowExists(refCol)",
	}
	want = append(want, typedRowSiteInventory...)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("typed-row fence call sites changed (ENG-2049: a link must be fenced with LinkExists).\n got: %q\nwant: %q", got, want)
	}
}

// typedRowSiteInventory lists the non-link typed rows fenced by TypedRowExists
// or a wrapper: events (events, ev, oe, e2, x), observations (observations, o)
// and entity mints (en.first_seen_evidence, ADR-0158 D4).
var typedRowSiteInventory = []string{
	`internal/infrastructure/postgres/identity_scan_source.go: viaTypedRow("en.first_seen_evidence")`,
	`internal/infrastructure/postgres/identity_scan_source.go: viaTypedRow("o.evidence_id")`,
	`internal/infrastructure/postgres/identity_scan_source.go: viaTypedRow("x.evidence_id")`,
	`internal/infrastructure/postgres/query_plane.go: TypedRowExists("ev.evidence_id")`,
	`internal/infrastructure/postgres/query_plane.go: TypedRowExists("observations.evidence_id")`,
	`internal/infrastructure/postgres/query_plane_catalog.go: viaTypedRow("ev.evidence_id")`,
	`internal/infrastructure/postgres/query_plane_catalog.go: viaTypedRow("events.evidence_id")`,
	`internal/infrastructure/postgres/query_plane_catalog.go: viaTypedRow("o.evidence_id")`,
	`internal/infrastructure/postgres/query_plane_catalog.go: viaTypedRow("observations.evidence_id")`,
	`internal/infrastructure/postgres/query_plane_listing.go: viaTypedRow("en.first_seen_evidence")`,
	`internal/infrastructure/postgres/query_plane_listing.go: viaTypedRow("x.evidence_id")`,
	`internal/infrastructure/postgres/query_plane_related.go: TypedRowExists("e2.evidence_id")`,
	`internal/infrastructure/postgres/query_plane_related.go: TypedRowExists("oe.evidence_id")`,
	`internal/objects/get.go: TypedRowExists("o.evidence_id")`,
	`internal/objects/get.go: typedRowVisible("en.first_seen_evidence")`,
	`internal/objects/get.go: typedRowVisible("ev.evidence_id")`,
	`internal/objects/get.go: typedRowVisible("o.evidence_id")`,
	`internal/ontology/violations.go: TypedRowExists("en.first_seen_evidence")`,
	`internal/ontology/violations.go: TypedRowExists("o.evidence_id")`,
}
