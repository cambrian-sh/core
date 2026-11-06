package memorynotes

import "strings"

// A notes document is markdown whose level-2 ("## ") headings are its SECTIONS.
// The edit ops work on that structure and leave every untouched section byte-for-
// byte as it was: an agent appending one line must not reformat the operator's text.
// Headings inside fenced code blocks are content, not sections.

// block is one contiguous span of the document: the preamble (heading == "",
// isSection false) or one section, kept as its raw text (heading line included).
type block struct {
	isSection bool
	heading   string
	raw       string
}

// splitBlocks cuts content into the preamble and its sections, in order.
func splitBlocks(content string) []block {
	if content == "" {
		return nil
	}
	lines := strings.SplitAfter(content, "\n")
	var out []block
	cur := block{}
	var b strings.Builder
	inFence := false
	flush := func() {
		cur.raw = b.String()
		if cur.isSection || cur.raw != "" {
			out = append(out, cur)
		}
		b.Reset()
	}
	for _, ln := range lines {
		trimmed := strings.TrimRight(ln, "\r\n")
		if fence := strings.TrimLeft(trimmed, " "); strings.HasPrefix(fence, "```") || strings.HasPrefix(fence, "~~~") {
			inFence = !inFence
		}
		if !inFence {
			if name, ok := headingName(trimmed); ok {
				flush()
				cur = block{isSection: true, heading: name}
			}
		}
		b.WriteString(ln)
	}
	flush()
	return out
}

// headingName reports whether line is a "## " heading and returns its text.
func headingName(line string) (string, bool) {
	if !strings.HasPrefix(line, "## ") {
		return "", false
	}
	return strings.TrimSpace(line[3:]), true
}

func joinBlocks(bs []block) string {
	var b strings.Builder
	for _, x := range bs {
		b.WriteString(x.raw)
	}
	return b.String()
}

func findSection(bs []block, name string) int {
	for i, x := range bs {
		if x.isSection && x.heading == name {
			return i
		}
	}
	return -1
}

// withTrailingNewline ends s with exactly one "\n" (s non-empty).
func withTrailingNewline(s string) string {
	return strings.TrimRight(s, "\r\n") + "\n"
}

// appendSeparated appends add to base with one blank line between them.
func appendSeparated(base, add string) string {
	base = strings.TrimRight(base, "\r\n")
	if base == "" {
		return withTrailingNewline(add)
	}
	return base + "\n\n" + withTrailingNewline(add)
}

// sectionRaw renders a section from its heading and body.
func sectionRaw(heading, body string) string {
	body = strings.Trim(body, "\r\n")
	if body == "" {
		return "## " + heading + "\n"
	}
	return "## " + heading + "\n" + body + "\n"
}

// sectionBody returns a section block's body (the text after the heading line).
func sectionBody(x block) string {
	if i := strings.IndexByte(x.raw, '\n'); i >= 0 {
		return x.raw[i+1:]
	}
	return ""
}

// spaced keeps a blank line between a rewritten block and the block after it.
func spaced(raw string, last bool) string {
	if last {
		return raw
	}
	return strings.TrimRight(raw, "\r\n") + "\n\n"
}

// applyOp applies one agent-plane edit. ok=false means the named section does not
// exist (REPLACE/REMOVE only; APPEND creates it).
func applyOp(content string, op Op, section, text string) (string, bool) {
	bs := splitBlocks(content)
	idx := -1
	if section != "" {
		idx = findSection(bs, section)
	}
	switch op {
	case OpAppend:
		if section == "" {
			return appendSeparated(content, text), true
		}
		if idx < 0 {
			return appendSeparated(content, sectionRaw(section, text)), true
		}
		body := strings.TrimRight(sectionBody(bs[idx]), "\r\n")
		if strings.TrimSpace(body) == "" {
			body = text
		} else {
			body = body + "\n" + strings.Trim(text, "\r\n")
		}
		bs[idx].raw = spaced(sectionRaw(section, body), idx == len(bs)-1)
		return joinBlocks(bs), true
	case OpReplaceSection:
		if idx < 0 {
			return content, false
		}
		bs[idx].raw = spaced(sectionRaw(section, text), idx == len(bs)-1)
		return joinBlocks(bs), true
	case OpRemoveSection:
		if idx < 0 {
			return content, false
		}
		bs = append(bs[:idx], bs[idx+1:]...)
		out := joinBlocks(bs)
		if strings.TrimSpace(out) == "" {
			return "", true
		}
		return withTrailingNewline(out), true
	}
	return content, false
}
