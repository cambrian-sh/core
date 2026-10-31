package memory

import (
	"context"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
)

// stripSpace drops every complete whitespace rune and keeps all other BYTES
// verbatim, so a multi-byte rune the hard cut split across two chunks compares
// equal once the chunks are joined back together.
func stripSpace(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !(r == utf8.RuneError && size == 1) && unicode.IsSpace(r) {
			i += size
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// FuzzRecursiveCharacterChunker: the ADR-0060 D2 recursive splitter must never
// panic on any body or tunables, and its output must be a faithful cover of
// the input — no chunk over the size ceiling, no empty chunk, every
// non-whitespace byte present exactly once and in order when overlap is off,
// contiguous metadata, and the same answer twice.
func FuzzRecursiveCharacterChunker(f *testing.F) {
	f.Add("", 200, 0)
	f.Add("   \n\n\t  ", 200, 0)
	f.Add("one paragraph that fits", 200, 0)
	f.Add("para one\n\npara two\n\npara three", 12, 0)
	f.Add("line one\nline two\nline three", 10, 3)
	f.Add("words words words words words words", 11, 0)
	f.Add(strings.Repeat("x", 1000), 64, 8)
	f.Add("ünïcödé — 日本語のテキスト nbsp emsp", 5, 2)
	f.Add("tabs\tand\r\nwindows\r\nline endings", 7, 0)
	f.Add("a", 1, 1)
	f.Add("abc", 0, -5)

	f.Fuzz(func(t *testing.T, body string, chunkSize, overlap int) {
		// Keep the tunables in a range a config would allow; the point is the
		// splitter's behaviour on the TEXT, not on absurd sizes.
		chunkSize %= 1024
		overlap %= 256
		c := NewRecursiveCharacterChunker(chunkSize, overlap)
		if chunkSize <= 0 {
			chunkSize = recursiveDefaultChunkSize
		}
		if overlap < 0 {
			overlap = 0
		}

		// No-panic holds for ANY bytes. The cover property below is checked on
		// valid UTF-8 only: the hard cut may split a multi-byte rune, and joining
		// chunks of an INVALID input can then form a whitespace rune the input
		// never had (the corpus entry ec41bad0ec440d91: "â " becomes
		// U+2008 once the space between the fragments is removed).
		if _, err := c.Chunk(context.Background(), &domain.ExternalDocument{Body: body}); err != nil {
			t.Fatalf("Chunk returned an error for raw bytes: %v", err)
		}
		body = strings.ToValidUTF8(body, "\uFFFD")

		doc := &domain.ExternalDocument{Body: body}
		chunks, err := c.Chunk(context.Background(), doc)
		if err != nil {
			t.Fatalf("Chunk returned an error for plain text: %v", err)
		}
		again, _ := c.Chunk(context.Background(), doc)
		if len(again) != len(chunks) {
			t.Fatalf("non-deterministic: %d chunks then %d", len(chunks), len(again))
		}

		trimmed := strings.TrimSpace(body)
		if trimmed == "" {
			if len(chunks) != 0 {
				t.Fatalf("blank body produced %d chunks", len(chunks))
			}
			return
		}
		if len(chunks) == 0 {
			t.Fatalf("non-blank body %q produced no chunks", body)
		}

		var joined strings.Builder
		for i, ch := range chunks {
			if ch.Body == "" {
				t.Fatalf("chunk %d is empty", i)
			}
			if len(ch.Body) > chunkSize+overlap {
				t.Fatalf("chunk %d is %d bytes, ceiling is %d+%d", i, len(ch.Body), chunkSize, overlap)
			}
			if ch.Metadata["chunk_index"] != i || ch.Metadata["total_chunks"] != len(chunks) {
				t.Fatalf("chunk %d metadata %v inconsistent with position/total %d", i, ch.Metadata, len(chunks))
			}
			if again[i].Body != ch.Body {
				t.Fatalf("non-deterministic body at chunk %d", i)
			}
			if overlap == 0 {
				if !strings.Contains(trimmed, ch.Body) {
					t.Fatalf("chunk %d %q is not a substring of the input", i, ch.Body)
				}
				joined.WriteString(ch.Body)
			}
		}
		if overlap == 0 {
			if got, want := stripSpace(joined.String()), stripSpace(trimmed); got != want {
				t.Fatalf("chunks do not cover the input: got %q want %q", got, want)
			}
		}
	})
}
