// Package discovery is the deterministic-first pre-plan discovery engine (ADR-0078).
// It replaces the LLM ReAct loop on the discovery hot path with a data-driven registry
// of deterministic, read-only probes over foreign state (filesystem, system, HTTP, ...).
// The LLM (ADR-0051's run_think scout) is demoted to an opt-in tier layered on top.
package discovery

import (
	"regexp"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

var (
	// urlRe matches http(s) URLs — routed to the "http" source.
	urlRe = regexp.MustCompile(`https?://[^\s"'<>)]+`)

	// pathRe matches path-like tokens with a separator: a/b, ./x, internal/x/y,
	// C:\Users\x, dir\file. These are UNAMBIGUOUS filesystem references (a slash/backslash
	// is not English), so they are emitted directly without validation.
	pathRe = regexp.MustCompile(`(?:[A-Za-z]:)?(?:\.{0,2}[\\/])?[\w.\-]+(?:[\\/][\w.\-]+)+[\\/]?`)

	// wordRe tokenizes bare identifiers (no separator) so they can be matched against the
	// set of names that ACTUALLY exist under the discovery roots (see SelectTargets).
	wordRe = regexp.MustCompile(`[\w.\-]+`)

	// systemRe fires a single "system" probe when the request references host/network
	// state (deterministic keyword gate — the system source is local-only, ref is unused).
	systemRe = regexp.MustCompile(`(?i)\b(network|interface|interfaces|ip address|ip addresses|hostname|host name|localhost|system resources|cpu|cpus)\b`)

	// trimPunct strips trailing sentence punctuation clinging to an extracted token.
	trimPunct = ".,;:!?)]}"
)

// minBarewordLen filters short tokens before matching them against real names, mirroring
// Aider's get_ident_mentions (identifiers below ~5 chars are too noisy). Set to 4 so
// common short folders still match while function words ("the", "and", "for") are excluded.
const minBarewordLen = 4

// SelectTargets deterministically extracts probe targets from the request text — no LLM
// (ADR-0078 D3). Following Aider's mention-detection approach, a bare word becomes a
// filesystem target ONLY if it names something that ACTUALLY EXISTS under the discovery
// roots — `known` maps a lowercased basename (and its extension-stripped stem) to the real
// relative path. This replaces brittle "<name> folder" grammar guessing: "the folder
// scratch_sections" resolves because `scratch_sections` is a real directory, while "the",
// "folder", and "list" are not. Explicit path tokens and URLs need no validation (a
// separator/scheme is unambiguous). Empty `known` ⇒ only explicit paths + URLs + system.
func SelectTargets(userInput string, known map[string]string) []domain.DiscoveryTarget {
	var targets []domain.DiscoveryTarget
	seen := map[string]bool{}

	add := func(kind, ref string) {
		ref = strings.TrimRight(strings.TrimSpace(ref), trimPunct)
		if ref == "" {
			return
		}
		key := kind + "\x00" + ref
		if seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, domain.DiscoveryTarget{Kind: kind, Ref: ref})
	}

	// URLs first, and blank out their spans so pathRe/wordRe do not re-capture the host.
	urls := urlRe.FindAllString(userInput, -1)
	for _, u := range urls {
		add("http", u)
	}
	masked := userInput
	for _, u := range urls {
		masked = strings.Replace(masked, u, strings.Repeat(" ", len(u)), 1)
	}

	// Explicit path-like tokens (contain a separator) are unambiguous references.
	pathSpans := pathRe.FindAllString(masked, -1)
	for _, p := range pathSpans {
		if strings.Contains(p, "://") {
			continue
		}
		add("filesystem", p)
	}

	// Bare words: emit only those that match a real name under the discovery roots.
	if len(known) > 0 {
		masked2 := masked
		for _, p := range pathSpans { // don't re-tokenize inside an already-matched path
			masked2 = strings.Replace(masked2, p, strings.Repeat(" ", len(p)), 1)
		}
		for _, w := range wordRe.FindAllString(masked2, -1) {
			tok := strings.TrimRight(w, trimPunct)
			if len([]rune(tok)) < minBarewordLen {
				continue
			}
			if rel, ok := known[strings.ToLower(tok)]; ok && rel != "" {
				add("filesystem", rel) // emit the REAL relative path, not the raw word
			}
		}
	}

	if systemRe.MatchString(masked) {
		add("system", "local")
	}

	return targets
}
