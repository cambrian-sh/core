package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// list + read work over real files, root-jailed.
func TestReferenceFsMCP_ListAndRead(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "intro.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	lo, err := doList(root, ".")
	if err != nil {
		t.Fatalf("doList: %v", err)
	}
	names := map[string]bool{}
	for _, e := range lo.Entries {
		names[e.Name] = e.IsDir
	}
	if _, ok := names["intro.md"]; !ok || names["intro.md"] {
		t.Errorf("expected intro.md as a file; got %+v", lo.Entries)
	}
	if isDir, ok := names["sub"]; !ok || !isDir {
		t.Errorf("expected sub as a directory; got %+v", lo.Entries)
	}

	ro, err := doRead(root, "intro.md")
	if err != nil {
		t.Fatalf("doRead: %v", err)
	}
	if ro.Content != "hello" || ro.Bytes != 5 {
		t.Errorf("read mismatch: %+v", ro)
	}
}

// The jail contains every path: any traversal is neutralized (re-based into root) or
// rejected — never resolved to a location outside root. This is the security property.
func TestReferenceFsMCP_JailContainment(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		"../secret", "../../etc/passwd", "/etc/passwd", "a/../../b", "./../../x", `..\..\win`,
	} {
		full, err := resolveJailed(root, p)
		if err != nil {
			continue // an outright rejection is also safe
		}
		rel, err := filepath.Rel(root, full)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("path %q escaped the jail → %q (rel %q)", p, full, rel)
		}
	}
	// An in-root path resolves fine.
	if _, err := resolveJailed(root, "docs/a.md"); err != nil {
		t.Errorf("an in-root path must resolve: %v", err)
	}
}

// read_file on a directory is an error (it is not a write op, just a misuse).
func TestReferenceFsMCP_ReadDirectoryErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := doRead(root, "d"); err == nil {
		t.Error("reading a directory must error (use list_directory)")
	}
}
