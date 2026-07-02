package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validSkill = `---
name: deploy
description: Deploy the application to production.
tools:
  - execute_command
  - read_file
scope:
  - ops
---
# Deploy

Run the migration, then the deploy script, then verify health.
`

// A well-formed SKILL.md parses into all Skill fields; the body becomes the
// instructions and the frontmatter does not leak into them.
func TestParseSkillMD_Valid(t *testing.T) {
	s, err := ParseSkillMD([]byte(validSkill))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "deploy" {
		t.Errorf("name = %q", s.Name)
	}
	if s.Description != "Deploy the application to production." {
		t.Errorf("description = %q", s.Description)
	}
	if len(s.ToolGrants) != 2 || s.ToolGrants[0] != "execute_command" {
		t.Errorf("tool grants = %v", s.ToolGrants)
	}
	if len(s.ScopeTags) != 1 || s.ScopeTags[0] != "ops" {
		t.Errorf("scope tags = %v", s.ScopeTags)
	}
	if !strings.Contains(s.Instructions, "migration") {
		t.Errorf("instructions should be the body, got %q", s.Instructions)
	}
	if strings.Contains(s.Instructions, "name: deploy") {
		t.Errorf("frontmatter leaked into instructions: %q", s.Instructions)
	}
}

// Missing/malformed frontmatter and a missing name are errors (Discover skips them).
func TestParseSkillMD_Errors(t *testing.T) {
	cases := map[string]string{
		"no frontmatter": "# Just a body\nno frontmatter here",
		"missing name":   "---\ndescription: x\n---\nbody",
		"malformed yaml": "---\nname: [unclosed\n---\nbody",
	}
	for name, content := range cases {
		if _, err := ParseSkillMD([]byte(content)); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

// Discover walks skills/<name>/SKILL.md, parses valid ones, and skips bad ones.
func TestDiscover(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "deploy", "SKILL.md"), validSkill)
	mustWrite(t, filepath.Join(dir, "broken", "SKILL.md"), "no frontmatter at all")

	skills, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "deploy" {
		t.Errorf("expected exactly the one valid skill, got %v", skills)
	}
}

// A missing skills dir yields zero skills, not an error.
func TestDiscover_MissingDir(t *testing.T) {
	skills, err := Discover(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil || skills != nil {
		t.Errorf("missing dir should be (nil, nil), got %v, %v", skills, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
