package domain

import (
	"testing"
)

func TestHasUsableText(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"":                    false,
		"   \n\t  ":           false,
		"  ":                  false, // no-break and em spaces
		"��":                  false, // what a decoder leaves for unreadable bytes
		"\x01\x02":            false, // control characters
		"a":                   true,
		"  42 ":               true,
		"—":                   true,
		"İstanbul belediyesi": true,
	} {
		if got := HasUsableText(in); got != want {
			t.Errorf("HasUsableText(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStorableText(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"clean text":       "clean text",
		"a\x00b":           "a b",
		"\x00":             " ",
		"bad \xff\xfe utf": "bad    utf",
		"ünïcødé":          "ünïcødé",
	}
	for in, want := range cases {
		got := StorableText(in)
		if got != want {
			t.Errorf("StorableText(%q) = %q, want %q", in, got, want)
		}
		if len(got) != len(in) {
			t.Errorf("StorableText(%q) changed the byte length %d -> %d (part offsets would shift)", in, len(in), len(got))
		}
	}
}

func TestNormalizeForStorage_DoesNotRewriteCallerSlices(t *testing.T) {
	t.Parallel()
	tags := []string{"ok", "bad\x00tag"}
	d := ExternalDocument{Title: "t\x00", Body: "x\x00y", Tags: tags,
		Parts: []DocumentPart{{Offset: 0, Length: 3, Speaker: "al\x00ice"}}}
	d.NormalizeForStorage()
	if d.Title != "t " || d.Body != "x y" || d.Tags[1] != "bad tag" || d.Parts[0].Speaker != "al ice" {
		t.Errorf("not normalised: %+v", d)
	}
	if tags[1] != "bad\x00tag" {
		t.Errorf("the caller's tag slice was rewritten: %q", tags[1])
	}
}
