package memorynotes

import "testing"

func TestApplyOp(t *testing.T) {
	doc := "Intro line\n\n## Prefs\nterse\n\n## Facts\n```\n## not a heading\n```\nlikes tea\n"
	cases := []struct {
		name, in      string
		op            Op
		section, text string
		want          string
		ok            bool
	}{
		{"append to empty doc, no section", "", OpAppend, "", "hello", "hello\n", true},
		{"append at end, no section", doc, OpAppend, "", "tail", doc[:len(doc)-1] + "\n\ntail\n", true},
		{"append creates section", "Intro\n", OpAppend, "New", "body", "Intro\n\n## New\nbody\n", true},
		{"append into existing section keeps others", doc, OpAppend, "Prefs", "metric",
			"Intro line\n\n## Prefs\nterse\nmetric\n\n## Facts\n```\n## not a heading\n```\nlikes tea\n", true},
		{"append into last section", doc, OpAppend, "Facts", "coffee too",
			"Intro line\n\n## Prefs\nterse\n\n## Facts\n```\n## not a heading\n```\nlikes tea\ncoffee too\n", true},
		{"replace section", doc, OpReplaceSection, "Prefs", "verbose",
			"Intro line\n\n## Prefs\nverbose\n\n## Facts\n```\n## not a heading\n```\nlikes tea\n", true},
		{"fenced heading is not a section", doc, OpReplaceSection, "not a heading", "x", doc, false},
		{"remove section", doc, OpRemoveSection, "Prefs", "",
			"Intro line\n\n## Facts\n```\n## not a heading\n```\nlikes tea\n", true},
		{"remove last section", "## Only\nx\n", OpRemoveSection, "Only", "", "", true},
		{"remove missing", doc, OpRemoveSection, "Nope", "", doc, false},
	}
	for _, c := range cases {
		got, ok := applyOp(c.in, c.op, c.section, c.text)
		if ok != c.ok || got != c.want {
			t.Errorf("%s:\n got %q (%v)\nwant %q (%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("héllo", 2); got != "hé" {
		t.Fatalf("got %q", got)
	}
	if got := truncateRunes("hi", 5); got != "hi" {
		t.Fatalf("got %q", got)
	}
}
