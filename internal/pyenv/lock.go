package pyenv

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Requirement is one line of a hashed lock: an exact pin, an optional
// environment marker, and the SHA-256 of every file the pin may install.
type Requirement struct {
	Name    string
	Version string
	Marker  string
	Hashes  []string
}

// Lock is a parsed hashed lock (agents/base.lock, D7.4.2).
type Lock struct {
	Requirements []Requirement
	// SHA256 is the digest of the lock text; it names the base generation's
	// inputs together with the interpreter and installer digests.
	SHA256 string
}

var (
	pinRe  = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)(\[[A-Za-z0-9,._ -]*\])?==([A-Za-z0-9.+!_-]+)$`)
	hashRe = regexp.MustCompile(`^--hash=sha256:([0-9a-f]{64})$`)
	sepRe  = regexp.MustCompile(`[-_.]+`)
)

// NormalizeName is the PEP 503 normalized project name.
func NormalizeName(n string) string { return sepRe.ReplaceAllString(strings.ToLower(n), "-") }

// ParseLock parses a hashed lock and refuses one the kernel cannot install in
// hash-checking mode: every requirement must be an exact `name==version` pin
// with at least one sha256 hash, and no line may be an option, URL, path or
// editable install (the release lock is the vetted unit, D7.4.4).
func ParseLock(b []byte) (Lock, error) {
	sum := sha256.Sum256(b)
	l := Lock{SHA256: hex.EncodeToString(sum[:])}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\\\n", " ")
	seen := map[string]bool{}
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "-") {
			return Lock{}, fmt.Errorf("base lock line %d: option %q is not allowed in a hashed lock", i+1, strings.Fields(line)[0])
		}
		// name==version [; marker] [--hash=… …]: the marker runs from ';' to
		// the first option.
		spec, options := line, ""
		if idx := strings.Index(line, " --"); idx >= 0 {
			spec, options = line[:idx], line[idx:]
		}
		pin, marker, _ := strings.Cut(spec, ";")
		pin = strings.TrimSpace(pin)
		m := pinRe.FindStringSubmatch(pin)
		if m == nil {
			return Lock{}, fmt.Errorf("base lock line %d: %q is not an exact name==version pin", i+1, pin)
		}
		req := Requirement{Name: m[1], Version: m[3], Marker: strings.TrimSpace(marker)}
		for _, f := range strings.Fields(options) {
			hm := hashRe.FindStringSubmatch(f)
			if hm == nil {
				return Lock{}, fmt.Errorf("base lock line %d (%s): %q is not a --hash=sha256:<hex> option", i+1, req.Name, f)
			}
			req.Hashes = append(req.Hashes, hm[1])
		}
		if len(req.Hashes) == 0 {
			return Lock{}, fmt.Errorf("base lock line %d: %s==%s has no --hash; the base lock must be generated with hashes (scripts/gen_agent_requirements.py)", i+1, req.Name, req.Version)
		}
		key := NormalizeName(req.Name) + ";" + req.Marker
		if seen[key] {
			return Lock{}, fmt.Errorf("base lock line %d: %s listed twice", i+1, req.Name)
		}
		seen[key] = true
		l.Requirements = append(l.Requirements, req)
	}
	if len(l.Requirements) == 0 {
		return Lock{}, fmt.Errorf("base lock is empty")
	}
	return l, nil
}

// Has reports whether the lock pins the (normalized) project name.
func (l Lock) Has(name string) bool {
	n := NormalizeName(name)
	for _, r := range l.Requirements {
		if NormalizeName(r.Name) == n {
			return true
		}
	}
	return false
}

// Render writes the lock back in requirements format, leaving out the named
// projects (a dev tree installs its locally built SDK wheel instead of the
// lock's published one, D7.4.5).
func (l Lock) Render(exclude ...string) []byte {
	skip := map[string]bool{}
	for _, e := range exclude {
		skip[NormalizeName(e)] = true
	}
	var sb strings.Builder
	for _, r := range l.Requirements {
		if skip[NormalizeName(r.Name)] {
			continue
		}
		sb.WriteString(r.Name + "==" + r.Version)
		if r.Marker != "" {
			sb.WriteString(" ; " + r.Marker)
		}
		for _, h := range r.Hashes {
			sb.WriteString(" \\\n    --hash=sha256:" + h)
		}
		sb.WriteString("\n")
	}
	return []byte(sb.String())
}
