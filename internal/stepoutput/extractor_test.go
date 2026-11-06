package stepoutput

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// feedAll feeds pieces in order and concatenates what comes out.
func feedAll(key string, pieces ...string) (string, bool) {
	e := NewExtractor(key)
	var b strings.Builder
	for _, p := range pieces {
		out := e.Feed(p)
		if !utf8.ValidString(out) {
			panic("extractor returned invalid UTF-8: " + out)
		}
		b.WriteString(out)
	}
	return b.String(), e.Done()
}

// splitEvery cuts s into pieces of n bytes — deliberately splitting escapes,
// \u sequences and multibyte characters.
func splitEvery(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

func TestExtractor_SimpleAndWhole(t *testing.T) {
	got, done := feedAll("answer", `{"answer": "hello world", "type": "text"}`)
	if got != "hello world" || !done {
		t.Fatalf("got %q done=%v", got, done)
	}
}

func TestExtractor_ByteAtATimeMatchesEncodingJSON(t *testing.T) {
	answers := []string{
		"plain",
		"quote \" backslash \\ slash / tab \t newline \n cr \r bs \b ff \f",
		"unicode é ü 中文 and emoji 😀🎉 and combining é",
		"control \u0001 and nul-ish \u001f",
		"",
		strings.Repeat("long answer ", 200),
	}
	for _, a := range answers {
		raw, _ := json.Marshal(map[string]any{"type": "text", "answer": a, "nested": map[string]any{"answer": "decoy"}})
		// encoding/json escapes <, >, & and non-ASCII stays raw; also try an
		// ASCII-only form with \u escapes everywhere.
		ascii := asciiEscape(string(raw))
		for _, doc := range []string{string(raw), ascii} {
			for _, n := range []int{1, 2, 3, 5, 7, 64} {
				got, done := feedAll("answer", splitEvery(doc, n)...)
				if got != a {
					t.Fatalf("n=%d doc=%s\n got %q\nwant %q", n, doc, got, a)
				}
				if !done {
					t.Fatalf("n=%d: not done", n)
				}
			}
		}
	}
}

// asciiEscape rewrites every non-ASCII rune as \uXXXX (surrogate pairs above the
// BMP), the form some providers stream.
func asciiEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			hi, lo := 0xD800+(r>>10), 0xDC00+(r&0x3FF)
			b.WriteString(`\u` + hex4(hi) + `\u` + hex4(lo))
		default:
			b.WriteString(`\u` + hex4(r))
		}
	}
	return b.String()
}

func hex4(r rune) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[r>>12&0xF], digits[r>>8&0xF], digits[r>>4&0xF], digits[r&0xF]})
}

func TestExtractor_RandomSplitsMatchEncodingJSON(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	alphabet := []rune("ab \"\\/\n\té中😀 {}[]:,")
	for i := 0; i < 300; i++ {
		var a strings.Builder
		for j := rng.Intn(40); j > 0; j-- {
			a.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		want := a.String()
		raw, _ := json.Marshal(map[string]any{"x": []any{1, "answer", map[string]any{"answer": 2}}, "answer": want})
		doc := string(raw)
		if rng.Intn(2) == 0 {
			doc = asciiEscape(doc)
		}
		var pieces []string
		for len(doc) > 0 {
			n := 1 + rng.Intn(6)
			if n > len(doc) {
				n = len(doc)
			}
			pieces = append(pieces, doc[:n])
			doc = doc[n:]
		}
		if got, _ := feedAll("answer", pieces...); got != want {
			t.Fatalf("case %d: got %q want %q", i, got, want)
		}
	}
}

func TestExtractor_NestedKeyIsNotTheArgument(t *testing.T) {
	got, done := feedAll("answer", `{"meta": {"answer": "decoy"}, "list": ["answer", {"answer": "no"}], "answer": "real"}`)
	if got != "real" || !done {
		t.Fatalf("got %q done=%v", got, done)
	}
}

func TestExtractor_KeyWithEscapesMatches(t *testing.T) {
	// "answer" is "answer".
	got, _ := feedAll("answer", `{"answer": "yes"}`)
	if got != "yes" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractor_NonStringAnswerYieldsNothing(t *testing.T) {
	for _, doc := range []string{
		`{"answer": 42, "type": "text"}`,
		`{"answer": {"text": "nested"}}`,
		`{"answer": ["a", "b"]}`,
		`{"answer": null}`,
		`{"answer": true}`,
	} {
		got, done := feedAll("answer", splitEvery(doc, 3)...)
		if got != "" {
			t.Errorf("%s: got %q, want nothing", doc, got)
		}
		if !done {
			t.Errorf("%s: a non-string value should end the extraction", doc)
		}
	}
}

func TestExtractor_SplitEscapeAndSurrogates(t *testing.T) {
	cases := []struct {
		pieces []string
		want   string
	}{
		{[]string{`{"answer":"a\`, `nb"}`}, "a\nb"},
		{[]string{`{"answer":"\u00`, `e9"}`}, "é"},
		{[]string{`{"answer":"\ud83d`, `\ude00!"}`}, "😀!"},
		{[]string{`{"answer":"\ud83d\`, `ude00"}`}, "😀"},
		// Raw UTF-8 split mid-character: the extractor waits for the whole rune.
		{[]string{"{\"answer\":\"\xe4\xb8", "\xad\"}"}, "中"},
		// An unpaired surrogate becomes U+FFFD, like encoding/json.
		{[]string{`{"answer":"x\ud83dy"}`}, "x�y"},
		{[]string{`{"answer":"\ude00"}`}, "�"},
	}
	for _, c := range cases {
		got, _ := feedAll("answer", c.pieces...)
		if got != c.want {
			t.Errorf("%q: got %q want %q", c.pieces, got, c.want)
		}
	}
}

func TestExtractor_StreamsIncrementally(t *testing.T) {
	e := NewExtractor("answer")
	if out := e.Feed(`{"type":"text","answer":"Hel`); out != "Hel" {
		t.Fatalf("first piece: %q", out)
	}
	if out := e.Feed(`lo`); out != "lo" {
		t.Fatalf("second piece: %q", out)
	}
	if e.Done() {
		t.Fatal("done before the closing quote")
	}
	if out := e.Feed(`", "more": "ignored"}`); out != "" || !e.Done() {
		t.Fatalf("closing piece: %q done=%v", out, e.Done())
	}
	if out := e.Feed(`garbage "answer": "again"`); out != "" {
		t.Fatalf("after done: %q", out)
	}
}

func TestExtractor_AbsentKeyAndTopLevelArray(t *testing.T) {
	if got, done := feedAll("answer", `{"type":"text"}`); got != "" || done {
		t.Errorf("absent key: %q done=%v", got, done)
	}
	if got, _ := feedAll("answer", `["answer", "x"]`); got != "" {
		t.Errorf("top-level array: %q", got)
	}
}
