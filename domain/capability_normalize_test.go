package domain

import "testing"

func TestNormalizeCapability(t *testing.T) {
	cases := map[string]string{
		"web-navigation":   "web-navigation",
		"Web-Navigation":   "web-navigation",
		"web_navigation":   "web-navigation",
		"web navigation":   "web-navigation",
		"  Web  Navigation ": "web-navigation",
		"FILE_READ":        "file-read",
		"file-read":        "file-read",
		"-leading-":        "leading",
		"":                 "",
		"   ":              "",
	}
	for in, want := range cases {
		if got := NormalizeCapability(in); got != want {
			t.Errorf("NormalizeCapability(%q) = %q, want %q", in, got, want)
		}
	}
}

// Deterministic normalization folds format/typo variance but must NOT merge distinct
// words — file-read and file-write stay distinct (no wrong merges).
func TestNormalizeCapability_NoCrossWordMerge(t *testing.T) {
	if NormalizeCapability("file-read") == NormalizeCapability("file-write") {
		t.Fatal("file-read and file-write must not normalize to the same tag")
	}
	if NormalizeCapability("browser") == NormalizeCapability("web-navigation") {
		t.Fatal("cross-word synonyms must NOT be merged by deterministic normalization")
	}
}

func TestNormalizeCapabilities_Dedup(t *testing.T) {
	out := NormalizeCapabilities([]string{"Web-Navigation", "web_navigation", "", "file read", "file read"})
	if len(out) != 2 || out[0] != "web-navigation" || out[1] != "file-read" {
		t.Fatalf("expected [web-navigation file-read], got %v", out)
	}
}
