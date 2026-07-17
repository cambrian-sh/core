package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// docExts are the extensions the filesystem summary counts as "documents" — the signal
// the motivating helicopter case needs ("10 sections, 3 written, 7 missing").
var docExts = map[string]bool{".md": true, ".txt": true, ".json": true, ".yaml": true, ".yml": true}

// FilesystemSource deterministically observes a filesystem path (ADR-0078 D2): existence,
// file size, or — for a directory — entry count, a document-extension breakdown, and a
// small sample of names. READ-ONLY (Stat/ReadDir only; never writes), and CONFINED to an
// allowlist of roots so a path lifted from an untrusted request cannot read outside the
// sanctioned tree (the deterministic analogue of ADR-0051 D6's discovery-safe grant).
type FilesystemSource struct {
	// Roots are the absolute directories reads are confined to. A target resolving
	// outside every root is refused (returned as a not-exists observation, not an error).
	// Empty Roots ⇒ the source refuses everything (fail-closed).
	Roots []string
	// MaxEntries caps how many names are sampled into the summary (0 ⇒ 12).
	MaxEntries int
}

// NewFilesystemSource confines reads to the given roots (cleaned to absolute paths).
func NewFilesystemSource(roots ...string) *FilesystemSource {
	abs := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" {
			continue
		}
		if a, err := filepath.Abs(r); err == nil {
			abs = append(abs, filepath.Clean(a))
		}
	}
	return &FilesystemSource{Roots: abs, MaxEntries: 12}
}

func (s *FilesystemSource) Kind() string { return "filesystem" }

// withinRoots reports whether abs is inside one of the allowlisted roots (or is a root).
func (s *FilesystemSource) withinRoots(abs string) bool {
	for _, root := range s.Roots {
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			continue
		}
		if rel == "." || (!strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != "..") {
			return true
		}
	}
	return false
}

// resolve turns a (possibly relative) target ref into a cleaned absolute path, preferring
// resolution under the first root when the ref is relative.
func (s *FilesystemSource) resolve(ref string) (string, bool) {
	ref = filepath.FromSlash(ref)
	if filepath.IsAbs(ref) {
		abs := filepath.Clean(ref)
		return abs, s.withinRoots(abs)
	}
	for _, root := range s.Roots {
		abs := filepath.Clean(filepath.Join(root, ref))
		if s.withinRoots(abs) {
			return abs, true
		}
	}
	return filepath.Clean(ref), false
}

func (s *FilesystemSource) Probe(_ context.Context, target domain.DiscoveryTarget) ([]domain.DiscoveredEntity, error) {
	abs, ok := s.resolve(target.Ref)
	if !ok {
		// Outside the sanctioned roots: report as unobservable-because-out-of-scope rather
		// than reading it. Kept as a not-exists entity so the planner sees the boundary.
		return []domain.DiscoveredEntity{{
			Kind: "dir", ID: target.Ref, Exists: false, Summary: "outside discovery roots (not observed)",
		}}, nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return []domain.DiscoveredEntity{{Kind: "file", ID: target.Ref, Exists: false, Summary: "does not exist"}}, nil
		}
		return nil, err // a real IO error (permission, etc.) → registry stamps unobserved
	}
	if !info.IsDir() {
		return []domain.DiscoveredEntity{{
			Kind: "file", ID: target.Ref, Exists: true,
			Summary: fmt.Sprintf("file, %d bytes", info.Size()),
		}}, nil
	}
	return []domain.DiscoveredEntity{s.summarizeDir(target.Ref, abs)}, nil
}

func (s *FilesystemSource) summarizeDir(ref, abs string) domain.DiscoveredEntity {
	entries, err := os.ReadDir(abs)
	if err != nil {
		return domain.DiscoveredEntity{Kind: "dir", ID: ref, Exists: true, Summary: "directory (unreadable)"}
	}
	limit := s.MaxEntries
	if limit <= 0 {
		limit = 12
	}
	var files, dirs, docs int
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			dirs++
		} else {
			files++
			if docExts[strings.ToLower(filepath.Ext(e.Name()))] {
				docs++
			}
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	sample := names
	truncated := ""
	if len(sample) > limit {
		sample = sample[:limit]
		truncated = ", ..."
	}
	summary := fmt.Sprintf("directory: %d entries (%d files, %d dirs, %d docs); contains: %s%s",
		len(entries), files, dirs, docs, strings.Join(sample, ", "), truncated)
	return domain.DiscoveredEntity{Kind: "dir", ID: ref, Exists: true, Summary: summary}
}
