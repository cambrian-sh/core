package domain

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// NormalizeDescriptiveField normalises one client-supplied descriptive value of an
// ingest — source_kind, title, author, a part's speaker (ADR-0136 A3.1) — the ONE
// rule both planes apply: trimmed; refused when it holds a control character (a
// newline in a label would let a value forge extra lines in a prompt's Source block
// or a passage label); cut to max characters at a rune boundary (max <= 0: no cap).
// Empty stays empty. The error names no field: callers add the field name and map it
// to their plane's INVALID_ARGUMENT.
func NormalizeDescriptiveField(v string, max int) (string, error) {
	v = strings.TrimSpace(v)
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("control character %U is not allowed", r)
		}
	}
	if max > 0 && utf8.RuneCountInString(v) > max {
		n := 0
		for i := range v {
			if n == max {
				v = strings.TrimSpace(v[:i])
				break
			}
			n++
		}
	}
	return v, nil
}
