package memory

import "testing"

// TestEngagedEntities_WindowsPathEscaping pins the Windows hazard that sat behind the
// silent swallow in engagedEntities. A path with UNESCAPED backslashes is invalid JSON,
// so the args fail to parse and every entity the tool touched is lost -- previously with
// no signal at all. Properly escaped args, which a correct producer sends, extract fine.
func TestEngagedEntities_WindowsPathEscaping(t *testing.T) {
	got := engagedEntities("write_file", []byte(`{"path":"C:\\Users\\afsin\\Dev\\a.md"}`))
	if len(got) == 0 {
		t.Fatal("properly escaped Windows path must extract entities, got none")
	}
	t.Logf("escaped -> %d %+v", len(got), got)

	if n := len(engagedEntities("write_file", []byte(`{"path":"C:\Users\afsin\Dev\a.md"}`))); n != 0 {
		t.Errorf("unescaped path is invalid JSON; expected 0 entities, got %d", n)
	}
}

// Both Windows spellings must canonicalise alike, or one file becomes two entities and
// the world model fragments -- the #1 failure mode ADR-0049 D8 names.
func TestCanonicalPath_WindowsFormsAgree(t *testing.T) {
	back := canonicalPath(`C:\Users\afsin\Dev\a.md`)
	fwd := canonicalPath("C:/Users/afsin/Dev/a.md")
	if back != fwd {
		t.Errorf("windows path forms must canonicalise alike: %q vs %q", back, fwd)
	}
	if isDegeneratePath(back) {
		t.Errorf("%q must not be degenerate", back)
	}
}
