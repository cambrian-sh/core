// Package prompts is the kernel's prompt library (ADR-0139 C4 / D2, owner
// decision Q6): every model-facing prompt a kernel organ or system agent sends
// lives here as a versioned file, `<id>/v<N>.md`, embedded into the binary.
//
// The format is specified once, in SPEC.md next to this file; the SDK's
// Python loader (sdk/cambrian_agent_sdk/prompt_library.py) implements the same
// rules and is held to this one by testdata/hash_cases.json.
//
// It is a pure library: it imports nothing from the kernel, so any layer may
// use it without inverting the hexagonal dependency direction.
//
// Why a library rather than Go string literals: the prompt audit
// (investigations/2026-11-04-agent-context-interview-prompts.md §3) found the
// planner hashing, as its "prompt version", rule text it never sent. Here the
// hash is computed from the template that is actually rendered, so it cannot
// certify text the model did not see.
package prompts

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Files is the embedded prompt tree. Schema files are named *.schema.json so the
// pattern skips testdata/ (which holds the cross-language fixture, not prompts).
//
//go:embed */v*.md */*.schema.json
var Files embed.FS

// Tier is a prompt's place in the D2 three-tier assembly.
type Tier string

// The three tiers, in assembly order.
const (
	TierStable   Tier = "stable"
	TierContext  Tier = "context"
	TierVolatile Tier = "volatile"
)

// Placeholder replaces every slot when the static hash is computed. It is an
// empty slot name, so it can never collide with a real slot (SPEC.md §4).
const Placeholder = "{{}}"

// Sentinel errors callers can distinguish.
var (
	// ErrNotFound: no prompt with that id, or no such version.
	ErrNotFound = errors.New("prompts: not found")
	// ErrSlots: Render was given a missing or an extra slot value.
	ErrSlots = errors.New("prompts: slot mismatch")
	// ErrInvalid: a prompt file violates SPEC.md.
	ErrInvalid = errors.New("prompts: invalid prompt file")
)

var (
	idPattern       = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	versionFile     = regexp.MustCompile(`^v([1-9][0-9]*)\.md$`)
	slotPattern     = regexp.MustCompile(`\{\{([a-z][a-z0-9_]*)\}\}`)
	slotNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Prompt is one parsed version file.
type Prompt struct {
	ID          string
	Version     int
	Tier        Tier
	Inputs      []string
	Description string
	ModelNotes  string
	// ResponseSchema is the JSON Schema the reply must satisfy, or nil. A schema
	// file is returned as written; an inline one as compact JSON.
	ResponseSchema []byte
	// Body is the normalised template (SPEC.md §3).
	Body string
}

// Rendered is a prompt with its slots filled: the text to send, and the
// identity of the static template it came from.
type Rendered struct {
	Text           string
	StaticHash     string
	ID             string
	Version        int
	ResponseSchema []byte
}

// StaticHash is the sha256 of the body with every slot replaced by
// Placeholder: the static text actually sent, identical across renders.
func (p Prompt) StaticHash() string { return StaticHash(p.Body) }

// StaticHash computes SPEC.md §4 over a template body. The body is normalised
// first, so a raw file body and a parsed Prompt.Body hash the same.
func StaticHash(body string) string {
	static := slotPattern.ReplaceAllLiteralString(NormaliseBody(body), Placeholder)
	sum := sha256.Sum256([]byte(static))
	return hex.EncodeToString(sum[:])
}

// NormaliseBody applies SPEC.md §3: CRLF to LF, leading newlines and trailing
// spaces, tabs and newlines removed.
func NormaliseBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.TrimLeft(body, "\n")
	return strings.TrimRight(body, " \t\r\n")
}

// Slots returns the distinct slot names in a body, in first-use order.
func Slots(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range slotPattern.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// Render fills the slots. Every input needs a value (an empty string counts) and
// no value may name a slot the prompt does not declare: a missing value would
// send a literal "{{goal}}", and an extra one is a caller that believes it is
// sending data the model never sees. Substitution is a single pass, so a value
// containing "{{x}}" is inserted literally.
func (p Prompt) Render(values map[string]string) (Rendered, error) {
	declared := make(map[string]bool, len(p.Inputs))
	var missing []string
	for _, in := range p.Inputs {
		declared[in] = true
		if _, ok := values[in]; !ok {
			missing = append(missing, in)
		}
	}
	var extra []string
	for k := range values {
		if !declared[k] {
			extra = append(extra, k)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		sort.Strings(extra)
		return Rendered{}, fmt.Errorf("%w: %s v%d: missing %v, extra %v", ErrSlots, p.ID, p.Version, missing, extra)
	}
	text := slotPattern.ReplaceAllStringFunc(p.Body, func(m string) string {
		return values[m[2:len(m)-2]]
	})
	return Rendered{
		Text:           text,
		StaticHash:     p.StaticHash(),
		ID:             p.ID,
		Version:        p.Version,
		ResponseSchema: p.ResponseSchema,
	}, nil
}

// Library is a loaded prompt tree.
type Library struct {
	byID map[string][]Prompt // each slice sorted by version, contiguous from 1
}

// Load parses and validates every `<id>/v<N>.md` in fsys. It fails on the first
// file that violates the structural rules of SPEC.md (L1-L5), so a broken
// prompt is a load error rather than a runtime surprise.
func Load(fsys fs.FS) (*Library, error) {
	lib := &Library{byID: map[string][]Prompt{}}
	dirs, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("prompts: read root: %w", err)
	}
	for _, d := range dirs {
		if !d.IsDir() || d.Name() == "testdata" {
			continue
		}
		id := d.Name()
		entries, err := fs.ReadDir(fsys, id)
		if err != nil {
			return nil, fmt.Errorf("prompts: read %s: %w", id, err)
		}
		var versions []Prompt
		for _, e := range entries {
			m := versionFile.FindStringSubmatch(e.Name())
			if e.IsDir() || m == nil {
				continue
			}
			p, err := parseFile(fsys, id, e.Name())
			if err != nil {
				return nil, err
			}
			versions = append(versions, p)
		}
		if len(versions) == 0 {
			continue
		}
		if !idPattern.MatchString(id) {
			return nil, fmt.Errorf("%w: %s: id must match %s", ErrInvalid, id, idPattern)
		}
		sort.Slice(versions, func(i, j int) bool { return versions[i].Version < versions[j].Version })
		for i, p := range versions {
			if p.Version != i+1 {
				return nil, fmt.Errorf("%w: %s: versions must be contiguous from 1, found v%d at position %d", ErrInvalid, id, p.Version, i+1)
			}
		}
		lib.byID[id] = versions
	}
	return lib, nil
}

// Get returns the latest version of a prompt.
func (l *Library) Get(id string) (Prompt, error) {
	vs := l.byID[id]
	if len(vs) == 0 {
		return Prompt{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return vs[len(vs)-1], nil
}

// GetVersion returns one version of a prompt.
func (l *Library) GetVersion(id string, n int) (Prompt, error) {
	vs := l.byID[id]
	if n < 1 || n > len(vs) {
		return Prompt{}, fmt.Errorf("%w: %q v%d", ErrNotFound, id, n)
	}
	return vs[n-1], nil
}

// IDs lists the prompt ids, sorted.
func (l *Library) IDs() []string {
	out := make([]string, 0, len(l.byID))
	for id := range l.byID {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Versions returns every version of a prompt, oldest first.
func (l *Library) Versions(id string) []Prompt {
	return append([]Prompt(nil), l.byID[id]...)
}

var (
	defaultOnce sync.Once
	defaultLib  *Library
	defaultErr  error
)

// Default is the library over the embedded Files, loaded once.
func Default() (*Library, error) {
	defaultOnce.Do(func() { defaultLib, defaultErr = Load(Files) })
	return defaultLib, defaultErr
}

// Get returns the latest version of an embedded prompt.
func Get(id string) (Prompt, error) {
	lib, err := Default()
	if err != nil {
		return Prompt{}, err
	}
	return lib.Get(id)
}

// GetVersion returns one version of an embedded prompt.
func GetVersion(id string, n int) (Prompt, error) {
	lib, err := Default()
	if err != nil {
		return Prompt{}, err
	}
	return lib.GetVersion(id, n)
}

// frontmatter is the strict decode target. Pointers distinguish "absent" from
// a zero value, so a missing required field is reported rather than defaulted.
type frontmatter struct {
	ID             *string   `yaml:"id"`
	Version        *int      `yaml:"version"`
	Tier           *string   `yaml:"tier"`
	Description    *string   `yaml:"description"`
	Inputs         *[]string `yaml:"inputs"`
	ResponseSchema yaml.Node `yaml:"response_schema"`
	ModelNotes     string    `yaml:"model_notes"`
}

// SplitFrontmatter separates a version file into its frontmatter and body,
// after normalising line endings. It is exported so the lint can apply the
// body rules to exactly what the loader would render.
func SplitFrontmatter(data []byte) (front, body string, err error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimPrefix(text, string([]byte{0xEF, 0xBB, 0xBF})) // a UTF-8 byte order mark
	if !strings.HasPrefix(text, "---\n") {
		return "", "", errors.New("file must start with a --- frontmatter line")
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	switch {
	case strings.HasPrefix(rest, "---\n"):
		return "", rest[len("---\n"):], nil
	case end >= 0:
		return rest[:end+1], rest[end+len("\n---\n"):], nil
	case strings.HasSuffix(rest, "\n---"):
		return rest[:len(rest)-len("---")], "", nil
	}
	return "", "", errors.New("frontmatter has no closing --- line")
}

func parseFile(fsys fs.FS, id, name string) (Prompt, error) {
	where := path.Join(id, name)
	fail := func(format string, a ...any) (Prompt, error) {
		return Prompt{}, fmt.Errorf("%w: %s: %s", ErrInvalid, where, fmt.Sprintf(format, a...))
	}
	data, err := fs.ReadFile(fsys, where)
	if err != nil {
		return Prompt{}, fmt.Errorf("prompts: read %s: %w", where, err)
	}
	front, body, err := SplitFrontmatter(data)
	if err != nil {
		return fail("%v", err)
	}
	var fm frontmatter
	dec := yaml.NewDecoder(strings.NewReader(front))
	dec.KnownFields(true)
	if err := dec.Decode(&fm); err != nil {
		return fail("frontmatter: %v", err)
	}
	for field, present := range map[string]bool{
		"id": fm.ID != nil, "version": fm.Version != nil, "tier": fm.Tier != nil,
		"description": fm.Description != nil, "inputs": fm.Inputs != nil,
	} {
		if !present {
			return fail("frontmatter field %q is required", field)
		}
	}
	if *fm.ID != id {
		return fail("id %q does not match its directory %q", *fm.ID, id)
	}
	fileVersion, _ := strconv.Atoi(versionFile.FindStringSubmatch(name)[1])
	if *fm.Version != fileVersion {
		return fail("version %d does not match the file name", *fm.Version)
	}
	tier := Tier(*fm.Tier)
	if tier != TierStable && tier != TierContext && tier != TierVolatile {
		return fail("tier %q is not stable, context or volatile", tier)
	}
	if strings.TrimSpace(*fm.Description) == "" {
		return fail("description is empty")
	}
	body = NormaliseBody(body)
	if err := checkSlots(*fm.Inputs, body); err != nil {
		return fail("%v", err)
	}
	schema, err := loadSchema(fsys, id, &fm.ResponseSchema)
	if err != nil {
		return fail("response_schema: %v", err)
	}
	return Prompt{
		ID:             id,
		Version:        fileVersion,
		Tier:           tier,
		Inputs:         append([]string(nil), *fm.Inputs...),
		Description:    strings.TrimSpace(*fm.Description),
		ModelNotes:     strings.TrimSpace(fm.ModelNotes),
		ResponseSchema: schema,
		Body:           body,
	}, nil
}

// checkSlots enforces L4: valid unique inputs, body slots == inputs, and no
// malformed "{{" left over once the valid slots are removed.
func checkSlots(inputs []string, body string) error {
	declared := map[string]bool{}
	for _, in := range inputs {
		if !slotNamePattern.MatchString(in) {
			return fmt.Errorf("input %q is not a valid slot name", in)
		}
		if declared[in] {
			return fmt.Errorf("input %q is listed twice", in)
		}
		declared[in] = true
	}
	used := map[string]bool{}
	for _, s := range Slots(body) {
		used[s] = true
		if !declared[s] {
			return fmt.Errorf("body uses slot {{%s}} that inputs does not declare", s)
		}
	}
	for _, in := range inputs {
		if !used[in] {
			return fmt.Errorf("input %q is declared but the body never uses it", in)
		}
	}
	if stripped := slotPattern.ReplaceAllLiteralString(body, ""); strings.Contains(stripped, "{{") {
		return errors.New(`body has a malformed "{{" (slots are {{name}} with name [a-z][a-z0-9_]*)`)
	}
	return nil
}

func loadSchema(fsys fs.FS, id string, n *yaml.Node) ([]byte, error) {
	switch n.Kind {
	case 0:
		return nil, nil
	case yaml.ScalarNode:
		name := strings.TrimSpace(n.Value)
		if name == "" {
			return nil, nil
		}
		if strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, ".schema.json") {
			return nil, fmt.Errorf("%q must be a sibling file named *.schema.json", name)
		}
		data, err := fs.ReadFile(fsys, path.Join(id, name))
		if err != nil {
			return nil, err
		}
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			return nil, fmt.Errorf("%s is not a JSON object: %v", name, err)
		}
		return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), nil
	case yaml.MappingNode:
		var obj map[string]any
		if err := n.Decode(&obj); err != nil {
			return nil, err
		}
		return json.Marshal(obj)
	}
	return nil, errors.New("must be a file name or an inline JSON object")
}
