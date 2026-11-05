package prompts

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

func file(front, body string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("---\n" + front + "---\n" + body)}
}

const okFront = "id: greet\nversion: %d\ntier: stable\ndescription: Say hello.\ninputs: [name]\n"

func front(v int) string { return strings.Replace(okFront, "%d", string(rune('0'+v)), 1) }

func TestRenderFillsSlotsAndHashesTheStaticText(t *testing.T) {
	lib, err := Load(fstest.MapFS{"greet/v1.md": file(front(1), "Hello {{name}}, bye {{name}}.\n")})
	if err != nil {
		t.Fatal(err)
	}
	p, err := lib.Get("greet")
	if err != nil {
		t.Fatal(err)
	}
	a, err := p.Render(map[string]string{"name": "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Render(map[string]string{"name": "a much longer {{name}} value"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Text != "Hello Ada, bye Ada." {
		t.Fatalf("text = %q", a.Text)
	}
	// Single pass: a value containing slot syntax is inserted literally.
	if !strings.Contains(b.Text, "a much longer {{name}} value") {
		t.Fatalf("value was re-expanded: %q", b.Text)
	}
	if a.StaticHash != b.StaticHash {
		t.Fatal("the static hash must not depend on slot values")
	}
	if a.StaticHash != StaticHash("Hello {{}}, bye {{}}.") {
		t.Fatal("the static hash must be over the body with slots replaced by the placeholder")
	}
	if a.ID != "greet" || a.Version != 1 {
		t.Fatalf("identity = %s v%d", a.ID, a.Version)
	}
}

func TestStaticHashChangesWithAnyStaticByte(t *testing.T) {
	if StaticHash("Hello {{name}}.") == StaticHash("Hello  {{name}}.") {
		t.Fatal("a whitespace change in the static text must change the hash")
	}
	if StaticHash("Hello {{name}}.") != StaticHash("Hello {{other}}.") {
		t.Fatal("renaming a slot does not change the sent static text")
	}
	if StaticHash("a\r\nb\n\n") != StaticHash("\na\nb") {
		t.Fatal("line endings and edge newlines are normalised before hashing")
	}
}

func TestRenderRejectsMissingAndExtraSlots(t *testing.T) {
	p := Prompt{ID: "x", Version: 1, Inputs: []string{"a", "b"}, Body: "{{a}} {{b}}"}
	if _, err := p.Render(map[string]string{"a": "1"}); !errors.Is(err, ErrSlots) {
		t.Fatalf("missing slot: err = %v", err)
	}
	if _, err := p.Render(map[string]string{"a": "1", "b": "2", "c": "3"}); !errors.Is(err, ErrSlots) {
		t.Fatalf("extra slot: err = %v", err)
	}
	r, err := p.Render(map[string]string{"a": "", "b": ""})
	if err != nil || r.Text != " " {
		t.Fatalf("an empty string is a value: %q, %v", r.Text, err)
	}
}

func TestGetLatestAndGetVersion(t *testing.T) {
	lib, err := Load(fstest.MapFS{
		"greet/v1.md": file(front(1), "one {{name}}"),
		"greet/v2.md": file(front(2), "two {{name}}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := lib.Get("greet"); p.Version != 2 {
		t.Fatalf("Get returned v%d, want the latest", p.Version)
	}
	if p, err := lib.GetVersion("greet", 1); err != nil || p.Body != "one {{name}}" {
		t.Fatalf("GetVersion(1) = %+v, %v", p, err)
	}
	if _, err := lib.GetVersion("greet", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version: %v", err)
	}
	if _, err := lib.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id: %v", err)
	}
}

func TestLoadRejectsInvalidFiles(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"version gap": {
			"greet/v1.md": file(front(1), "{{name}}"),
			"greet/v3.md": file(front(3), "{{name}}"),
		},
		"version mismatch": {"greet/v1.md": file(front(2), "{{name}}")},
		"id mismatch":      {"other/v1.md": file(front(1), "{{name}}")},
		"bad tier":         {"greet/v1.md": file(strings.Replace(front(1), "stable", "fixed", 1), "{{name}}")},
		"unknown field":    {"greet/v1.md": file(front(1)+"respons_schema: x.schema.json\n", "{{name}}")},
		"missing field":    {"greet/v1.md": file(strings.Replace(front(1), "description: Say hello.\n", "", 1), "{{name}}")},
		"undeclared slot":  {"greet/v1.md": file(front(1), "{{name}} {{age}}")},
		"unused input":     {"greet/v1.md": file(front(1), "no slots")},
		"malformed slot":   {"greet/v1.md": file(front(1), "{{name}} {{ Name }}")},
		"no frontmatter":   {"greet/v1.md": &fstest.MapFile{Data: []byte("just a body")}},
		"schema missing":   {"greet/v1.md": file(front(1)+"response_schema: r.schema.json\n", "{{name}}")},
		"schema not json": {
			"greet/v1.md":         file(front(1)+"response_schema: r.schema.json\n", "{{name}}"),
			"greet/r.schema.json": &fstest.MapFile{Data: []byte("{nope")},
		},
		"schema not sibling": {"greet/v1.md": file(front(1)+"response_schema: ../r.schema.json\n", "{{name}}")},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(fsys); err == nil {
				t.Fatal("Load accepted an invalid prompt tree")
			}
		})
	}
}

func TestResponseSchemaFileAndInline(t *testing.T) {
	lib, err := Load(fstest.MapFS{
		"greet/v1.md":         file(front(1)+"response_schema: r.schema.json\n", "{{name}}"),
		"greet/r.schema.json": &fstest.MapFile{Data: []byte("{\"type\": \"object\"}\r\n")},
		"inline/v1.md": file("id: inline\nversion: 1\ntier: volatile\ndescription: d\ninputs: []\n"+
			`response_schema: {"type": "object", "required": ["a"]}`+"\n", "static"),
	})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := lib.Get("greet")
	if string(p.ResponseSchema) != "{\"type\": \"object\"}\n" {
		t.Fatalf("file schema = %q", p.ResponseSchema)
	}
	q, _ := lib.Get("inline")
	if string(q.ResponseSchema) != `{"required":["a"],"type":"object"}` {
		t.Fatalf("inline schema = %q", q.ResponseSchema)
	}
	r, err := q.Render(map[string]string{})
	if err != nil || string(r.ResponseSchema) != string(q.ResponseSchema) {
		t.Fatalf("render must carry the schema: %v", err)
	}
}

func TestEmbeddedLibraryLoads(t *testing.T) {
	lib, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.IDs()) == 0 {
		t.Fatal("no embedded prompts")
	}
	if _, err := Get("clarifier"); err != nil {
		t.Fatal(err)
	}
}
