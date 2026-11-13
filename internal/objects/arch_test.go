package objects

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The object tables have ONE writer (ADR-0149 D3, "Not a second source of
// truth"): the maintainer in state.go. This test fails the build when any other
// production file writes object table rows, or when another file of this
// package starts issuing row writes at all.

var (
	// A row write naming an objects_ table, in any Go source.
	objectTableWrite = regexp.MustCompile(`(?is)\b(insert\s+into|update|delete\s+from|truncate(\s+table)?|copy)\s+(only\s+)?(\\?")?(public\.)?(\\?")?objects_`)
	// A row write naming the fence class dictionary (migration 0043): the
	// maintainer interns pairs; nothing else may add, change or remove one (an
	// id must name one pair forever, or a row's vis_classes would change meaning).
	classDictWrite = regexp.MustCompile(`(?is)\b(insert\s+into|update|delete\s+from|truncate(\s+table)?|copy)\s+(only\s+)?(\\?")?(public\.)?(\\?")?object_fence_classes\b`)
	// Any row-writing statement keyword (this package builds table names
	// dynamically, so inside it any such keyword outside state.go is suspect).
	rowWriteKeyword = regexp.MustCompile(`(?i)\b(INSERT\s+INTO|DELETE\s+FROM|UPDATE\s+\S+\s+SET|TRUNCATE)\b`)
)

func goSources(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestArchitecture_OnlyTheMaintainerWritesObjectTables(t *testing.T) {
	core, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	roots := []string{core}
	// Premium plugs into core and must not write the tables either (go.work sibling).
	if premium := filepath.Join(core, "..", "cambrian-premium"); dirExists(premium) {
		roots = append(roots, premium)
	}
	pkg, _ := filepath.Abs(".")
	maintainer := filepath.Join(pkg, "state.go")
	checked := 0
	for _, root := range roots {
		for _, f := range goSources(t, root) {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			checked++
			if f == maintainer {
				continue
			}
			if m := objectTableWrite.Find(src); m != nil {
				t.Errorf("%s writes an object table (%q): only internal/objects/state.go may (ADR-0149 D3)", f, m)
			}
			if m := classDictWrite.Find(src); m != nil {
				t.Errorf("%s writes the fence class dictionary (%q): only internal/objects/state.go may (ADR-0149 D6)", f, m)
			}
			if filepath.Dir(f) == pkg {
				if m := rowWriteKeyword.Find(src); m != nil {
					t.Errorf("%s issues a row write (%q): in internal/objects only the maintainer (state.go) writes rows", f, m)
				}
			}
		}
	}
	if checked < 100 {
		t.Fatalf("scanned only %d files under %v: the walk is broken", checked, roots)
	}
}

func TestArchitecture_DetectorCatchesAWriter(t *testing.T) {
	for _, bad := range []string{
		"INSERT INTO objects_order (a) VALUES (1)",
		`tx.Exec(ctx, "UPDATE \"objects_order\" SET x = 1")`,
		"delete from objects_customer where true",
		"TRUNCATE TABLE objects_order",
	} {
		if !objectTableWrite.MatchString(bad) {
			t.Errorf("detector misses %q", bad)
		}
	}
	if objectTableWrite.MatchString("SELECT * FROM objects_order") {
		t.Error("a read is not a write")
	}
	for _, bad := range []string{
		"INSERT INTO object_fence_classes (tags, parties) VALUES ('{}','{}')",
		"update object_fence_classes set tags = '{}'",
		"DELETE FROM object_fence_classes",
		"TRUNCATE object_fence_classes",
	} {
		if !classDictWrite.MatchString(bad) {
			t.Errorf("detector misses %q", bad)
		}
	}
	if classDictWrite.MatchString("SELECT c.id FROM object_fence_classes c") {
		t.Error("a read of the dictionary is not a write")
	}
	// The maintainer itself does write it: the check is not vacuous.
	src, err := os.ReadFile("state.go")
	if err != nil {
		t.Fatal(err)
	}
	if !classDictWrite.Match(src) {
		t.Error("state.go no longer interns classes: the detector would pass vacuously")
	}
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
