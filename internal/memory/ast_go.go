// Package memory — AST Go Chunker.
//
// ASTGoChunker splits Go source files on top-level declarations
// (functions, types, constants, variables, imports) using go/ast
// (ADR-0060 D2, T-1.6). For non-Go source (or files whose Supports
// check returns false) the registry's fall-through routes the doc
// to the default chunker — this chunker's Chunk returns an
// ErrUnsupportedExtension in that case, matching the spec.
//
// Each top-level decl becomes one chunk. The chunk body is the
// decl's source span including the immediately-preceding doc
// comment (if any) — the doc-comment-inclusion behaviour of
// parser.ParseComments. The first chunk additionally carries the
// package declaration (so chunk 0 is a self-contained "file header
// + first decl" unit that downstream readers can render standalone).
//
// No tree-sitter, no cgo, no external Go parser — just go/ast from
// the Go stdlib.
package memory

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// ErrUnsupportedExtension is returned by ASTGoChunker.Chunk when
// Supports is false. The registry reads this and falls back to the
// configured default.
var ErrUnsupportedExtension = errors.New("ast_go: unsupported extension (only .go)")

// ASTGoChunker is a zero-value-friendly domain.Chunker. Construct
// with the literal `ASTGoChunker{}`; there are no per-call tunables.
type ASTGoChunker struct{}

// Name is the registry key. Stable.
func (ASTGoChunker) Name() string { return "ast_go" }

// Supports returns true only for ".go". The registry's
// Resolve (or this chunker's Chunk) falls back to the default for
// any other ext.
func (ASTGoChunker) Supports(sourceType, ext string) bool {
	_ = sourceType
	return ext == ".go"
}

// Chunk parses the doc body as a Go source file and emits one
// chunk per top-level declaration. Doc comments immediately
// preceding a decl are included in the chunk body. The first chunk
// additionally carries the package declaration so the chunk is
// self-contained.
//
// Returns ErrUnsupportedExtension for non-.go ext. Returns
// []Chunk{} for a Go file with no top-level decls (e.g. a
// package-only file).
func (a ASTGoChunker) Chunk(ctx context.Context, doc *domain.ExternalDocument) ([]domain.Chunk, error) {
	_ = ctx
	if doc == nil {
		return nil, nil
	}
	ext := docExtFromURI(doc.SourceURI)
	if ext != ".go" {
		return nil, ErrUnsupportedExtension
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "doc.go", doc.Body, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	if len(file.Decls) == 0 {
		return []domain.Chunk{}, nil
	}

	lines := splitLinesKeepEmpty(doc.Body)

	pkgStartLine := 0
	if file.Name != nil {
		pkgStartLine = fset.Position(file.Name.Pos()).Line - 1
	}

	type declSpan struct{ startLine, endLine int }
	spans := make([]declSpan, 0, len(file.Decls))
	for _, d := range file.Decls {
		startLine := fset.Position(d.Pos()).Line - 1
		endLine := fset.Position(d.End()).Line
		if endLine > startLine {
			endLine = endLine - 1
		} else {
			endLine = startLine
		}
		docStart := walkBackDocComment(lines, startLine)
		spans = append(spans, declSpan{startLine: docStart, endLine: endLine})
	}

	total := len(spans)
	out := make([]domain.Chunk, total)
	for i, sp := range spans {
		start := sp.startLine
		if start < 0 {
			start = 0
		}
		end := sp.endLine + 1
		if end > len(lines) {
			end = len(lines)
		}
		if end <= start {
			end = start + 1
		}
		body := joinTrimmedLines(lines[start:end])
		if i == 0 && pkgStartLine >= 0 && pkgStartLine < len(lines) {
			pkgLine := strings.TrimRight(lines[pkgStartLine], " \t")
			if pkgLine != "" {
				body = pkgLine + "\n\n" + body
			}
		}
		out[i] = domain.Chunk{
			Body: body,
			Metadata: map[string]any{
				"chunk_index":  i,
				"total_chunks": total,
			},
		}
	}
	return out, nil
}

// walkBackDocComment returns the 0-based line index of the first
// consecutive `//`-prefixed line immediately before startLine (no
// blank lines in between). If no doc comment group is found,
// returns startLine.
func walkBackDocComment(lines []string, startLine int) int {
	for i := startLine - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			return startLine
		}
		if !strings.HasPrefix(t, "//") {
			return startLine
		}
		startLine = i
	}
	return startLine
}

// docExtFromURI is a local copy of the IngestionManager helper
// (the IngestionManager's helper is unexported). Keeping a local
// copy makes the chunker independently testable.
func docExtFromURI(uri string) string {
	dot := strings.LastIndex(uri, ".")
	if dot < 0 || dot >= len(uri)-1 {
		return ""
	}
	return strings.ToLower(uri[dot:])
}

// splitLinesKeepEmpty splits body on '\n' and keeps empty lines.
func splitLinesKeepEmpty(body string) []string {
	var out []string
	start := 0
	for i := 0; i < len(body); i++ {
		if body[i] == '\n' {
			out = append(out, body[start:i])
			start = i + 1
		}
	}
	out = append(out, body[start:])
	return out
}

// joinTrimmedLines joins lines with '\n' and trims trailing
// whitespace from the last line.
func joinTrimmedLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	for i, ln := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ln)
	}
	return strings.TrimRight(b.String(), " \t")
}
