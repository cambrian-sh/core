package migrate

import (
	"strings"
	"testing"
)

func TestParseName(t *testing.T) {
	cases := []struct {
		in      string
		version int64
		name    string
		wantErr bool
	}{
		{"0002_add_widget.sql", 2, "add_widget", false},
		{"0010_fts_index.sql", 10, "fts_index", false},
		{"0002_a.sql", 2, "a", false},
		{"noversion.sql", 0, "", true},
		{"_leading.sql", 0, "", true},
		{"abc_x.sql", 0, "", true},
	}
	for _, c := range cases {
		v, n, err := parseName(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.in, err)
			continue
		}
		if v != c.version || n != c.name {
			t.Errorf("%s: got (%d,%q), want (%d,%q)", c.in, v, n, c.version, c.name)
		}
	}
}

func TestUnknownApplied_DetectsDBAhead(t *testing.T) {
	known := map[int64]bool{1: true, 2: true}
	// DB only at known versions → not ahead.
	if _, ahead := unknownApplied(map[int64]bool{1: true, 2: true}, known); ahead {
		t.Fatal("should not be ahead when all applied versions are known")
	}
	// DB has version 3 the binary doesn't know → ahead.
	v, ahead := unknownApplied(map[int64]bool{1: true, 2: true, 3: true}, known)
	if !ahead || v != 3 {
		t.Fatalf("expected ahead at version 3, got (%d,%v)", v, ahead)
	}
}

func TestKnownVersions(t *testing.T) {
	known, max := knownVersions([]migration{{version: 5}, {version: 3}})
	if !known[3] || !known[5] || max != 5 {
		t.Fatalf("expected versions 3,5 known and max 5, got known=%v max=%d", known, max)
	}
}

// loadMigrations over the embedded FS must include the baseline (0001) and substitute
// ${EMBEDDING_DIM}, proving the embed + glob + templating wiring.
func TestLoadMigrations_IncludesBaselineWithDimSubstituted(t *testing.T) {
	migs, err := loadMigrations(1024)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("expected at least the baseline migration")
	}
	if migs[0].version != BaselineVersion || migs[0].name != "baseline" {
		t.Fatalf("first migration should be the baseline, got %d_%s", migs[0].version, migs[0].name)
	}
	if !strings.Contains(migs[0].sql, "VECTOR(1024)") {
		t.Fatal("expected ${EMBEDDING_DIM} substituted to 1024 in the baseline SQL")
	}
	if strings.Contains(migs[0].sql, "${EMBEDDING_DIM}") {
		t.Fatal("baseline SQL still contains an un-substituted ${EMBEDDING_DIM}")
	}
}
