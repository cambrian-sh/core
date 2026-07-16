package domain

import (
	"regexp"
	"strings"
)

var capSeparatorRun = regexp.MustCompile(`[\s_\-]+`)

// NormalizeCapability canonicalizes a capability tag DETERMINISTICALLY (ROUTE-04 /
// ADR-0067): lowercase, trim, and collapse any run of whitespace / '_' / '-' into a
// single '-'. So `Web-Navigation`, `web_navigation`, and `web navigation` all become
// `web-navigation`. It is purely lexical — it does NOT do fuzzy or embedding-based
// synonym merging (`browser` ↔ `web-navigation`), which risks wrong merges
// (e.g. `file-read` ↔ `file-write`) and worse misroutes than the variance it fixes.
func NormalizeCapability(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = capSeparatorRun.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// NormalizeCapabilities normalizes a slice and de-duplicates it, preserving first-seen
// order and dropping empties.
func NormalizeCapabilities(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, c := range in {
		n := NormalizeCapability(c)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}
