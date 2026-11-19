package config

import (
	"strings"
	"testing"
)

// ENG-111: and_first is the default lexical match; or_only keeps the old OR match;
// anything else is refused; the mode is part of the retrieval fingerprint.
func TestLexicalMatchMode(t *testing.T) {
	d := DefaultConfig().Execution
	if d.Retrieval.LexicalMatchMode != LexicalMatchAndFirst {
		t.Fatalf("default = %q", d.Retrieval.LexicalMatchMode)
	}
	empty := d
	empty.Retrieval.LexicalMatchMode = ""
	if empty.Retrieval.EffectiveLexicalMatchMode() != LexicalMatchAndFirst || empty.RetrievalFingerprint("e") != d.RetrievalFingerprint("e") {
		t.Error(`"" must mean and_first`)
	}
	or := d
	or.Retrieval.LexicalMatchMode = LexicalMatchOrOnly
	if err := or.Validate(); err != nil {
		t.Fatalf("or_only refused: %v", err)
	}
	if or.RetrievalFingerprint("e") == d.RetrievalFingerprint("e") {
		t.Error("the match mode must change the fingerprint")
	}
	bad := d
	bad.Retrieval.LexicalMatchMode = "and_only"
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "lexical_match_mode") {
		t.Fatalf("validate: %v", err)
	}
}
