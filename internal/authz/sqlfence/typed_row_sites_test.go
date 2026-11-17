package sqlfence

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestEvidenceExistsNeverFencesATypedRow lists every production use of the
// evidence-only fence — EvidenceExists and the two wrappers that pass a column
// through to it (postgres sqlArgs.viaEvidence, objects sqlb.evidenceVisible) —
// with the reference column it is given (ADR-0158 D2, stage 3).
//
// A typed row (events, observations, links) carries its own row classification,
// and EvidenceExists ignores it: fencing such a row with it shows the row to a
// reader its mapping restricted. Typed rows use TypedRowExists (or the wrappers
// viaTypedRow / the objects equivalent), which can only narrow. Every entry left
// here is a ref that is NOT a typed row — an entity's minting evidence
// (first_seen_evidence: ADR-0158 stage 5 classifies the mint), the evidence
// outbox — or one of the objects sites ADR-0158 stage 4 moves.
//
// If this test fails because you added an EvidenceExists call: if the column
// belongs to events, observations or links, use TypedRowExists instead; if it
// truly is not a typed row, add the line with a comment saying what it fences.
func TestEvidenceExistsNeverFencesATypedRow(t *testing.T) {
	want := []string{
		// The wrappers themselves (pass-through of their refCol parameter).
		"internal/infrastructure/postgres/query_plane_catalog.go: EvidenceExists(refCol)",
		"internal/objects/plan.go: EvidenceExists(refCol)",

		// evidence_outbox (aliased o): an outbox row has no row classification.
		`internal/infrastructure/postgres/query_plane_catalog.go: viaEvidence("o.evidence_id")`,
		`internal/infrastructure/postgres/query_plane_catalog.go: viaEvidence("o.evidence_id")`,

		// Entity first-seen (ADR-0158 D4, stage 5 moves these to
		// entities.first_seen_classification).
		`internal/infrastructure/postgres/identity_scan_source.go: viaEvidence("en.first_seen_evidence")`,
		`internal/infrastructure/postgres/identity_scan_source.go: viaEvidence("en.first_seen_evidence")`,
		`internal/infrastructure/postgres/identity_scan_source.go: viaEvidence("en.first_seen_evidence")`,
		`internal/infrastructure/postgres/identity_scan_source.go: viaEvidence("en.first_seen_evidence")`,
		`internal/infrastructure/postgres/query_plane_listing.go: viaEvidence("en.first_seen_evidence")`,
		`internal/objects/get.go: evidenceVisible("en.first_seen_evidence")`,
		`internal/ontology/violations.go: EvidenceExists("en.first_seen_evidence")`,

		// Objects typed-row sites ADR-0158 stage 4 moves (D3 sites 38, 41-44):
		// link filters, GetObject links, alternatives, event and observation
		// timelines. Stage 4 must delete these lines.
		`internal/objects/get.go: evidenceVisible("l.evidence_id")`,
		`internal/objects/get.go: EvidenceExists("o.evidence_id")`,
		`internal/objects/get.go: evidenceVisible("ev.evidence_id")`,
		`internal/objects/get.go: evidenceVisible("o.evidence_id")`,
		`internal/objects/plan.go: evidenceVisible(l + ".evidence_id")`,
	}

	root := filepath.Join("..", "..", "..") // the cambrian-core module root
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
			var name string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			}
			var ref ast.Expr
			switch name {
			case "EvidenceExists", "viaEvidence":
				if len(call.Args) >= 2 {
					ref = call.Args[1]
				}
			case "evidenceVisible":
				if len(call.Args) >= 1 {
					ref = call.Args[0]
				}
			default:
				return true
			}
			if ref == nil {
				return true
			}
			got = append(got, rel+": "+name+"("+types.ExprString(ref)+")")
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
		t.Errorf("evidence-only fence call sites changed (ADR-0158 D2: a typed row — events, observations, links — must use TypedRowExists).\n got: %q\nwant: %q", got, want)
	}
}
