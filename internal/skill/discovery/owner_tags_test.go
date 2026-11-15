package discovery

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1046 (owner decision 2026-11-15, ADR-0146 D5): a SKILL.md scope may not
// carry an owner tag. The parser refuses it; discovery skips that skill and
// still loads the rest.
func TestParseSkillMD_RefusesOwnerScope(t *testing.T) {
	_, err := ParseSkillMD([]byte("---\nname: mine\ndescription: d\nscope: [ops, \"owner:alice\"]\n---\nbody"))
	if !errors.Is(err, domain.ErrOwnerTagInDescriptor) || !strings.Contains(err.Error(), "owner:alice") || !strings.Contains(err.Error(), "mine") {
		t.Fatalf("want a refusal naming the skill and tag, got %v", err)
	}
	sk, err := ParseSkillMD([]byte("---\nname: ok\ndescription: d\nscope: [ops]\n---\nbody"))
	if err != nil || len(sk.ScopeTags) != 1 {
		t.Fatalf("clean skill: %+v %v", sk, err)
	}
}

func TestDiscover_SkipsOwnerScopedSkillKeepsOthers(t *testing.T) {
	dir := t.TempDir()
	for name, md := range map[string]string{
		"mine": "---\nname: mine\ndescription: d\nscope: [\"owner:alice\"]\n---\nbody",
		"ok":   "---\nname: ok\ndescription: d\nscope: [ops]\n---\nbody",
	} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(md), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg := domain.NewInMemorySkillRegistry()
	skills, err := LoadRegistry(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "ok" {
		t.Fatalf("want only the clean skill, got %+v", skills)
	}
	if _, ok := reg.Get("mine"); ok {
		t.Fatal("an owner-scoped skill must not be served")
	}
}
