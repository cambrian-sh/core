package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// PromptSection is a single XML-tagged block in a canonical LLM prompt.
// Construct sections with the Prompt* helpers; assemble with PromptBuild.
type PromptSection struct {
	content string
}

// PromptBuild assembles a canonical LLM prompt from sections joined by double
// newlines. Sections with empty content are silently omitted, so callers can
// pass domain.PromptContext(ltmBlock) when ltmBlock may be empty.
func PromptBuild(sections ...PromptSection) string {
	parts := make([]string, 0, len(sections))
	for _, s := range sections {
		if s.content != "" {
			parts = append(parts, s.content)
		}
	}
	return strings.Join(parts, "\n\n")
}

// PromptSystem emits a <System> section containing a <Role> sub-tag and an
// optional <Constraints> sub-tag. Empty constraint strings are silently
// filtered so dynamic sections (capability clusters, model lists) can be
// passed unconditionally.
//
//	<System>
//	<Role>role</Role>
//	<Constraints>
//	constraint1
//	constraint2
//	</Constraints>
//	</System>
func PromptSystem(role string, constraints ...string) PromptSection {
	var b strings.Builder
	b.WriteString("<System>\n")
	fmt.Fprintf(&b, "<Role>\n%s\n</Role>", strings.TrimSpace(role))

	var nonEmpty []string
	for _, c := range constraints {
		if strings.TrimSpace(c) != "" {
			nonEmpty = append(nonEmpty, strings.TrimSpace(c))
		}
	}
	if len(nonEmpty) > 0 {
		b.WriteString("\n<Constraints>\n")
		for i, c := range nonEmpty {
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(c)
		}
		b.WriteString("\n</Constraints>")
	}
	b.WriteString("\n</System>")
	return PromptSection{content: b.String()}
}

// PromptContext wraps pre-built content in a <Context> section.
// It does not decompose or reformat domain-specific XML — callers pass the
// already-formatted block (e.g. buildLTMBlock output) verbatim.
// Returns an empty section when content is blank, which PromptBuild omits.
func PromptContext(content string) PromptSection {
	c := strings.TrimSpace(content)
	if c == "" {
		return PromptSection{}
	}
	return PromptSection{content: fmt.Sprintf("<Context>\n%s\n</Context>", c)}
}

// PromptTask wraps an instruction in a <Task> section.
func PromptTask(instruction string) PromptSection {
	return PromptSection{content: fmt.Sprintf("<Task>\n%s\n</Task>", strings.TrimSpace(instruction))}
}

// PromptOutputSchemaJSON wraps a JSON Schema string in
// <OutputSchema format="json">. The schema string should be valid JSON Schema;
// an optional format example may be appended after the schema object.
func PromptOutputSchemaJSON(schema string) PromptSection {
	return PromptSection{content: fmt.Sprintf("<OutputSchema format=\"json\">\n%s\n</OutputSchema>", strings.TrimSpace(schema))}
}

// PromptOutputSchemaEnum emits <OutputSchema format="enum">A | B | C</OutputSchema>.
func PromptOutputSchemaEnum(values ...string) PromptSection {
	return PromptSection{content: fmt.Sprintf(`<OutputSchema format="enum">%s</OutputSchema>`, strings.Join(values, " | "))}
}

// PromptOutputSchemaString emits a <OutputSchema format="text"> section for
// free-text outputs bounded by a maximum character length. The format is "text"
// (not "string") to stay within the serialization vocabulary structured-output
// layers accept — a "string" format is rejected and the error leaks into the
// completion (e.g. a cluster name becoming an error blob).
func PromptOutputSchemaString(maxLength int, description string) PromptSection {
	return PromptSection{content: fmt.Sprintf(
		"<OutputSchema format=\"text\" maxLength=\"%d\">\n%s\n</OutputSchema>",
		maxLength, strings.TrimSpace(description),
	)}
}

// PromptOutputSchemaInteger emits a <OutputSchema format="integer"> section.
// The existing regex-based parsers for integer responses are compatible with
// bare-integer output; this tag makes the contract explicit to the LLM.
func PromptOutputSchemaInteger(min, max int) PromptSection {
	return PromptSection{content: fmt.Sprintf(
		`<OutputSchema format="integer" min="%d" max="%d">Return ONLY a single integer between %d and %d.</OutputSchema>`,
		min, max, min, max,
	)}
}

// PromptHashOf computes the 8-char SHA-256 hex prefix of a prompt template.
// Used to populate PromptEntry.Hash and written to audit records such as
// PlanEvent.PlannerPromptVersion and Document.ScoringPromptVersion.
// Only the static parts of a prompt (role, constraints, schema) should be
// hashed — dynamic injected content (user input, LTM blocks) is excluded.
func PromptHashOf(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])[:8]
}
