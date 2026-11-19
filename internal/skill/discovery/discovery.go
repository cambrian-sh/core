// Package discovery auto-discovers system skills from skills/<name>/SKILL.md
// files into the kernel-owned SkillRegistry (ADR-0046 D2/D7). A SKILL.md carries YAML frontmatter (name,
// description, tools, scope) + a markdown body (the instructions). Agent skills
// are NOT discovered here — they are SDK-local.
package discovery

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cambrian-sh/core/domain"

	"gopkg.in/yaml.v3"
)

// skillFrontmatter is the YAML contract a SKILL.md declares. Scope maps onto the
// ADR-0034 domain.TagSet used to gate which agents may load the skill.
type skillFrontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tools       []string `yaml:"tools"`
	Scope       []string `yaml:"scope"` // ADR-0046 D9 (option A): flat classification tags
	// ADR-0139 C3 metadata. Lenient: a non-string version (e.g. `version: 1`) is
	// read as its YAML text.
	Version string `yaml:"version"`
	Trust   string `yaml:"trust"`
}

// ParseSkillMD parses one SKILL.md (frontmatter + body) into a domain.Skill.
// Errors on missing/malformed frontmatter or a missing name — Discover turns
// those into skips so one bad file never breaks discovery of the rest.
func ParseSkillMD(content []byte) (domain.Skill, error) {
	fm, body, ok := splitFrontmatter(content)
	if !ok {
		return domain.Skill{}, fmt.Errorf("skill: missing YAML frontmatter")
	}
	var f skillFrontmatter
	if err := yaml.Unmarshal([]byte(fm), &f); err != nil {
		return domain.Skill{}, fmt.Errorf("skill: malformed frontmatter: %w", err)
	}
	if strings.TrimSpace(f.Name) == "" {
		return domain.Skill{}, fmt.Errorf("skill: frontmatter missing name")
	}
	// ADR-0146 D5 / ENG-1046 (owner decision 2026-11-15): owner tags are
	// kernel-stamped; a skill's scope may not carry one. One parser serves disk
	// discovery, agent proposals and stored approved skills, so all three refuse.
	if err := domain.RefuseOwnerTags("skill "+strings.TrimSpace(f.Name)+": scope", f.Scope); err != nil {
		return domain.Skill{}, err
	}
	return domain.Skill{
		Name:         f.Name,
		Description:  f.Description,
		Instructions: body,
		ToolGrants:   f.Tools,
		ScopeTags:    f.Scope,
		Version:      strings.TrimSpace(f.Version),
		Trust:        strings.TrimSpace(f.Trust),
	}, nil
}

// splitFrontmatter separates leading `---`-delimited YAML from the markdown body.
// Returns ok=false when the content does not open with a frontmatter block.
func splitFrontmatter(content []byte) (frontmatter, body string, ok bool) {
	s := strings.TrimPrefix(string(content), "\ufeff") // strip UTF-8 BOM
	s = strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(s, "---") {
		return "", "", false
	}
	lines := strings.Split(s, "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return "", "", false
	}
	return strings.Join(lines[1:end], "\n"), strings.TrimSpace(strings.Join(lines[end+1:], "\n")), true
}

// Discover reads every skills/<name>/SKILL.md under dir and returns the parsed
// skills. A missing dir yields zero skills (not an error). A subdir without a
// SKILL.md, or one that fails to parse, is skipped with a warning.
func Discover(dir string) ([]domain.Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan skills: %w", err)
	}

	var skills []domain.Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			if !os.IsNotExist(rerr) {
				slog.Warn("skill discovery: read failed", "dir", e.Name(), "err", rerr)
			}
			continue
		}
		sk, perr := ParseSkillMD(content)
		if perr != nil {
			if errors.Is(perr, domain.ErrOwnerTagInDescriptor) {
				// Not a typo but a refused descriptor (ENG-1046): ERROR, so it
				// is not mistaken for an ignorable parse warning.
				slog.Error("skill discovery: skill refused", "dir", e.Name(), "file", path, "err", perr)
				continue
			}
			slog.Warn("skill discovery: skipping", "dir", e.Name(), "err", perr)
			continue
		}
		sk.SourcePath = path
		skills = append(skills, sk)
	}
	return skills, nil
}

// LoadRegistry scans dir and registers every discovered system skill into reg,
// returning the discovered skills (e.g. for indexing).
func LoadRegistry(dir string, reg domain.SkillRegistry) ([]domain.Skill, error) {
	skills, err := Discover(dir)
	if err != nil {
		return nil, err
	}
	for _, s := range skills {
		reg.Register(s)
	}
	return skills, nil
}
