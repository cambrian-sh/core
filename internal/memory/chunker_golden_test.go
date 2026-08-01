package memory

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// updateGolden regenerates testdata/chunker_golden.json instead of verifying it:
//
//	go test ./internal/memory/ -run TestChunkerGolden -update-golden
//
// Then re-run `make proto-sync` (which also copies this fixture) and commit the
// updated file in cambrian-benchmarks.
var updateGolden = flag.Bool("update-golden", false, "rewrite the chunker golden fixture")

// goldenFixture is the cross-language contract for chunking behaviour.
//
// # Why this file exists
//
// cambrian-benchmarks re-implements all five Go chunkers in Python
// (suites/chunking/chunkers.py — "Python ports of the 5 chunking strategies").
// That is a deliberate consequence of the black-box invariant: the harness may not
// import kernel internals, so the logic was ported by hand instead. The cost is
// that the benchmark measures a COPY, and nothing compared the copy to the
// original — the port's own tests assert Python behaviour against hand-written
// expectations, so the two could diverge silently and the benchmark would keep
// reporting numbers that describe neither implementation.
//
// This fixture closes that hole from the Go side: it pins what each chunker
// actually produces, and the Python suite asserts its ports reproduce it exactly.
// A behaviour change in Go now fails HERE (the golden no longer matches) and, once
// the golden is regenerated and synced, fails on the Python side until the port is
// updated to match.
//
// # What is compared, and what is not
//
// BODIES only, not metadata. The Go chunkers receive a whole ExternalDocument and
// stamp doc-derived metadata (source_uri, author, timestamp, chunk_index); the
// Python ports receive a bare string and cannot reproduce any of that, by design.
// The segmentation — where the boundaries fall — is the shared behaviour, and it
// is the thing the chunking benchmark actually measures.
//
// The `late` chunker is deliberately absent: its output depends on an embedder, so
// a cross-language fixture would pin the mock rather than the chunker.
type goldenFixture struct {
	Name string `json:"name"`
	// Ext is the file extension the input is presented as. It decides which
	// chunkers claim the input via Supports(), so the Python side must apply the
	// same gating rather than running every port on every fixture.
	Ext  string `json:"ext"`
	Text string `json:"text"`
	// Chunkers maps chunker name → the ordered chunk bodies it produces. A chunker
	// absent from this map either declined the input (Supports == false) or
	// refused it (see Refused).
	Chunkers map[string][]string `json:"chunkers"`
	// Refused lists chunkers that claimed the input via Supports but returned an
	// error on it — e.g. ast_go on a file that does not parse as Go. Sorted for a
	// stable diff.
	Refused []string `json:"refused,omitempty"`
}

type goldenFile struct {
	// Comment travels with the data so a reader of the JSON in the other repo
	// learns what it is without finding this file.
	Comment  string          `json:"_comment"`
	Fixtures []goldenFixture `json:"fixtures"`
}

// goldenInputs are chosen to exercise each chunker's decision points rather than
// to look realistic: paragraph breaks, the 1000-char sentence-split threshold,
// markdown heading levels, Go declarations with doc comments, and the degenerate
// empty/whitespace cases where implementations most often disagree.
func goldenInputs() []struct{ name, ext, text string } {
	long := ""
	for range 40 {
		long += "This sentence is here to push the paragraph past the split threshold. "
	}
	return []struct{ name, ext, text string }{
		{"empty", ".txt", ""},
		{"whitespace_only", ".txt", "   \n\n  \t "},
		{"single_paragraph", ".txt", "Just one paragraph with no breaks at all."},
		{"two_paragraphs", ".txt", "First paragraph.\n\nSecond paragraph."},
		{"trailing_blank_lines", ".txt", "Alpha.\n\nBeta.\n\n\n"},
		{"long_paragraph_sentence_split", ".txt", long},
		{"markdown_headings", ".md", "# Title\n\nIntro text.\n\n## Section A\n\nBody A.\n\n### Sub A1\n\nBody A1.\n\n## Section B\n\nBody B."},
		{"markdown_no_heading", ".md", "No heading here.\n\nJust prose."},
		{"go_source", ".go", "package memory\n\n// Doc comment for Alpha.\nfunc Alpha() int {\n\treturn 1\n}\n\ntype Beta struct {\n\tX int\n}\n\nconst Gamma = 3\n"},
		{"go_source_no_package", ".go", "func Orphan() {}\n"},
		{"unicode", ".txt", "Héllo wörld.\n\n日本語のテキストです。\n\nЕщё один абзац."},
	}
}

// goldenChunkers is the set compared across languages, keyed by the registry name
// so the JSON is readable in the Python repo.
func goldenChunkers() map[string]domain.Chunker {
	return map[string]domain.Chunker{
		"option_c":            OptionCChunker{},
		"ast_go":              ASTGoChunker{},
		"markdown_header":     MarkdownHeaderChunker{},
		"recursive_character": NewRecursiveCharacterChunker(0, 0),
	}
}

func goldenPath() string { return filepath.Join("testdata", "chunker_golden.json") }

func buildGolden(t *testing.T) goldenFile {
	t.Helper()
	ctx := context.Background()
	out := goldenFile{
		Comment: "Generated by cambrian-core: go test ./internal/memory/ -run TestChunkerGolden -update-golden. " +
			"Chunk BODIES produced by each Go chunker. cambrian-benchmarks asserts its Python ports reproduce these exactly. " +
			"Do not hand-edit.",
	}
	for _, in := range goldenInputs() {
		fx := goldenFixture{Name: in.name, Ext: in.ext, Text: in.text, Chunkers: map[string][]string{}}
		doc := &domain.ExternalDocument{
			SourceURI:  "golden://" + in.name + in.ext,
			SourceType: "file_drop",
			Title:      in.name,
			Body:       in.text,
		}
		for name, c := range goldenChunkers() {
			// Only record a chunker that would actually be routed this input.
			// ast_go rejects anything but .go, and forcing it would pin an error
			// path rather than a chunking behaviour.
			if !c.Supports(doc.SourceType, in.ext) {
				continue
			}
			chunks, err := c.Chunk(ctx, doc)
			if err != nil {
				// A chunker that REFUSES an input is a real, intended behaviour
				// (ast_go rejects a file that will not parse). Record it as an
				// outcome instead of failing the fixture, so the contract says
				// "this input is refused" rather than pretending it never arose.
				// The bodies comparison stays focused on successful chunking,
				// where the Python port can meaningfully be held to the same
				// output; matching Go error STRINGS across languages would pin
				// the message rather than the behaviour.
				fx.Refused = append(fx.Refused, name)
				continue
			}
			bodies := make([]string, 0, len(chunks))
			for _, ch := range chunks {
				bodies = append(bodies, ch.Body)
			}
			fx.Chunkers[name] = bodies
		}
		sort.Strings(fx.Refused)
		out.Fixtures = append(out.Fixtures, fx)
	}
	return out
}

// TestChunkerGolden pins Go chunking behaviour. A diff here means either a real
// behaviour change (regenerate with -update-golden and update the Python port) or
// an accidental one (fix the code).
func TestChunkerGolden(t *testing.T) {
	got := buildGolden(t)
	// Indented + newline-terminated so the file reviews cleanly in a diff.
	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	encoded = append(encoded, '\n')

	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath(), encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden fixture rewritten: %s", goldenPath())
		return
	}

	want, err := os.ReadFile(goldenPath())
	if err != nil {
		t.Fatalf("read golden (%s): %v\nGenerate it with: go test ./internal/memory/ -run TestChunkerGolden -update-golden", goldenPath(), err)
	}
	// Compare parsed structures, not bytes: a CRLF checkout must not fail this.
	var wantFile goldenFile
	if err := json.Unmarshal(want, &wantFile); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if len(wantFile.Fixtures) != len(got.Fixtures) {
		t.Fatalf("fixture count: golden has %d, code produces %d — regenerate with -update-golden",
			len(wantFile.Fixtures), len(got.Fixtures))
	}
	for i, wf := range wantFile.Fixtures {
		gf := got.Fixtures[i]
		if wf.Name != gf.Name {
			t.Fatalf("fixture %d: golden %q, code %q — regenerate with -update-golden", i, wf.Name, gf.Name)
		}
		for name, wantBodies := range wf.Chunkers {
			gotBodies, ok := gf.Chunkers[name]
			if !ok {
				t.Errorf("fixture %q: chunker %q missing from current code", wf.Name, name)
				continue
			}
			if len(wantBodies) != len(gotBodies) {
				t.Errorf("fixture %q chunker %q: chunk count golden=%d got=%d",
					wf.Name, name, len(wantBodies), len(gotBodies))
				continue
			}
			for j := range wantBodies {
				if wantBodies[j] != gotBodies[j] {
					t.Errorf("fixture %q chunker %q chunk %d:\n golden: %q\n    got: %q",
						wf.Name, name, j, wantBodies[j], gotBodies[j])
				}
			}
		}
	}
}
