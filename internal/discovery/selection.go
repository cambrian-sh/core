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
	// C:\Users\x, dir\file. Routed to the "filesystem" source. Deliberately requires
	// a separator so a bare word ("helicopter") is not mistaken for a path — the
	// "<name> folder" phrasing is handled by folderRe below.
	pathRe = regexp.MustCompile(`(?:[A-Za-z]:)?(?:\.{0,2}[\\/])?[\w.\-]+(?:[\\/][\w.\-]+)+[\\/]?`)

	// folderRe catches the natural-language "<name> folder|directory|dir" pattern that
	// the motivating helicopter case uses ("continue the helicopter folder"). Captures
	// the bare name and (optionally) a `backtick`/'quote'/"quote" wrapping.
	folderRe = regexp.MustCompile("(?i)[`'\"]?([\\w.\\-]+)[`'\"]?\\s+(?:folder|directory|dir)\\b")

	// systemRe fires a single "system" probe when the request references host/network
	// state (deterministic keyword gate — the system source is local-only, ref is unused).
	systemRe = regexp.MustCompile(`(?i)\b(network|interface|interfaces|ip address|ip addresses|hostname|host name|localhost|system resources|cpu|cpus)\b`)

	// trimPunct strips trailing sentence punctuation clinging to an extracted token.
	trimPunct = ".,;:!?)]}"
)

// SelectTargets deterministically extracts probe targets from the request text — no LLM
// (ADR-0078 D3). URLs → http; path-like tokens and "<name> folder" phrases → filesystem.
// Order is preserved and duplicates (by kind+ref) are dropped so the scan cap (Registry)
// binds on distinct observations.
func SelectTargets(userInput string) []domain.DiscoveryTarget {
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

	// URLs first, and remember their spans so pathRe does not re-capture the host/path.
	urls := urlRe.FindAllString(userInput, -1)
	for _, u := range urls {
		add("http", u)
	}
	masked := userInput
	for _, u := range urls {
		masked = strings.Replace(masked, u, strings.Repeat(" ", len(u)), 1)
	}

	for _, p := range pathRe.FindAllString(masked, -1) {
		if strings.Contains(p, "://") {
			continue
		}
		add("filesystem", p)
	}

	for _, m := range folderRe.FindAllStringSubmatch(masked, -1) {
		if len(m) > 1 {
			add("filesystem", m[1])
		}
	}

	if systemRe.MatchString(masked) {
		add("system", "local")
	}

	return targets
}
