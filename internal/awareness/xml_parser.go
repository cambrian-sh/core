package awareness

import (
	"log/slog"
	"strings"
)

const (
	thoughtOpenTag  = "<thought>"
	thoughtCloseTag = "</thought>"
)

// ParseThoughts scans raw for <thought>...</thought> blocks, extracts their
// inner text into thoughts (trimmed), and returns everything outside those
// blocks concatenated as planJSON (trimmed).
//
// Malformed blocks (unclosed <thought> tag) are logged at WARN and skipped;
// the remaining text is appended to planJSON.
func ParseThoughts(raw string) (thoughts []string, planJSON string) {
	var outside strings.Builder
	rest := raw

	for {
		openIdx := strings.Index(rest, thoughtOpenTag)
		if openIdx == -1 {
			outside.WriteString(rest)
			break
		}

		outside.WriteString(rest[:openIdx])
		rest = rest[openIdx+len(thoughtOpenTag):]

		closeIdx := strings.Index(rest, thoughtCloseTag)
		if closeIdx == -1 {
			slog.Warn("xml_parser: unclosed <thought> tag; treating remainder as planJSON")
			outside.WriteString(rest)
			rest = ""
			break
		}

		thoughts = append(thoughts, strings.TrimSpace(rest[:closeIdx]))
		rest = rest[closeIdx+len(thoughtCloseTag):]
	}

	planJSON = strings.TrimSpace(outside.String())
	return
}
