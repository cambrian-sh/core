package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func kinds(ts []domain.DiscoveryTarget) map[string][]string {
	m := map[string][]string{}
	for _, t := range ts {
		m[t.Kind] = append(m[t.Kind], t.Ref)
	}
	return m
}

func TestSelectTargets(t *testing.T) {
	got := kinds(SelectTargets(
		"continue the helicopter folder and update internal/x/y.go, then GET https://api.example.com/openapi.json; also list network interfaces",
	))
	if refs := got["filesystem"]; len(refs) < 2 {
		t.Errorf("want filesystem path + folder-name, got %v", refs)
	}
	if !contains(got["filesystem"], "helicopter") {
		t.Errorf("folder-name phrasing not captured: %v", got["filesystem"])
	}
	if !contains(got["filesystem"], "internal/x/y.go") {
		t.Errorf("path token not captured: %v", got["filesystem"])
	}
	if !contains(got["http"], "https://api.example.com/openapi.json") {
		t.Errorf("url not captured: %v", got["http"])
	}
	if len(got["system"]) != 1 {
		t.Errorf("system keyword not captured: %v", got["system"])
	}
}

func TestSelectTargets_URLNotReCapturedAsPath(t *testing.T) {
	got := kinds(SelectTargets("check https://example.com/a/b/c for status"))
	if len(got["http"]) != 1 {
		t.Fatalf("want 1 http target, got %v", got["http"])
	}
	for _, ref := range got["filesystem"] {
		if strings.Contains(ref, "example.com") {
			t.Errorf("url path leaked into a filesystem target: %q", ref)
		}
	}
}

func TestSelectTargets_Dedup(t *testing.T) {
	got := SelectTargets("a/b and a/b again")
	n := 0
	for _, tt := range got {
		if tt.Kind == "filesystem" && tt.Ref == "a/b" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("duplicate targets not collapsed: %d", n)
	}
}

func TestFilesystemSource_DirSummary(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "helicopter")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"intro.md", "rotor.md", "tail.md", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(sub, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := NewFilesystemSource(root)
	ents, err := src.Probe(context.Background(), domain.DiscoveryTarget{Kind: "filesystem", Ref: "helicopter"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || !ents[0].Exists || ents[0].Kind != "dir" {
		t.Fatalf("want an existing dir entity, got %+v", ents)
	}
	if !strings.Contains(ents[0].Summary, "4 entries") || !strings.Contains(ents[0].Summary, "4 docs") {
		t.Errorf("summary missing counts: %q", ents[0].Summary)
	}
}

func TestFilesystemSource_ConfinedToRoots(t *testing.T) {
	root := t.TempDir()
	src := NewFilesystemSource(root)
	// A traversal escaping the root must be refused, not read.
	ents, err := src.Probe(context.Background(), domain.DiscoveryTarget{Kind: "filesystem", Ref: "../../etc"})
	if err != nil {
		t.Fatal(err)
	}
	if ents[0].Exists || !strings.Contains(ents[0].Summary, "outside discovery roots") {
		t.Errorf("path escape not refused: %+v", ents[0])
	}
}

func TestFilesystemSource_NotExists(t *testing.T) {
	src := NewFilesystemSource(t.TempDir())
	ents, err := src.Probe(context.Background(), domain.DiscoveryTarget{Kind: "filesystem", Ref: "nope.md"})
	if err != nil {
		t.Fatal(err)
	}
	if ents[0].Exists {
		t.Errorf("nonexistent path reported as existing: %+v", ents[0])
	}
}

// erroringSource always fails — to prove a probe error becomes an unobserved stamp.
type erroringSource struct{}

func (erroringSource) Kind() string { return "filesystem" }
func (erroringSource) Probe(context.Context, domain.DiscoveryTarget) ([]domain.DiscoveredEntity, error) {
	return nil, errors.New("boom")
}

func TestRegistry_ProbeErrorBecomesUnobserved(t *testing.T) {
	r := NewRegistry(erroringSource{})
	ents, unobserved := r.Discover(context.Background(), "look at a/b/c.md")
	if len(ents) != 0 {
		t.Errorf("errored probe should yield no entities: %v", ents)
	}
	if len(unobserved) != 1 || !strings.HasPrefix(unobserved[0], "filesystem:") {
		t.Errorf("want one unobserved filesystem stamp, got %v", unobserved)
	}
}

func TestRegistry_ScanCap(t *testing.T) {
	r := NewRegistry(NewFilesystemSource(t.TempDir())).WithGovernors(1, 0)
	// three distinct filesystem targets; cap = 1 ⇒ 2 unobserved.
	_, unobserved := r.Discover(context.Background(), "a/b and c/d and e/f")
	if len(unobserved) != 2 {
		t.Errorf("scan cap not enforced: unobserved=%v", unobserved)
	}
}

func TestRegistry_UnknownKindSkipped(t *testing.T) {
	r := NewRegistry(NewFilesystemSource(t.TempDir())) // no http source
	_, unobserved := r.Discover(context.Background(), "GET https://example.com/x")
	for _, u := range unobserved {
		if strings.HasPrefix(u, "http:") {
			t.Errorf("unregistered kind should be skipped, not stamped unobserved: %v", unobserved)
		}
	}
}

func TestHTTPSource_SSRFGuardBlocksLoopback(t *testing.T) {
	src := NewHTTPSource(false) // AllowPrivate=false
	ents, err := src.Probe(context.Background(), domain.DiscoveryTarget{Kind: "http", Ref: "http://127.0.0.1:8080/x"})
	if err != nil {
		t.Fatal(err)
	}
	if ents[0].Exists || !strings.Contains(ents[0].Summary, "SSRF") {
		t.Errorf("loopback not blocked: %+v", ents[0])
	}
}

func TestHTTPSource_SSRFGuardBlocksMetadataIP(t *testing.T) {
	src := NewHTTPSource(false)
	// link-local (cloud metadata) must be refused.
	if _, err := src.guard("http://169.254.169.254/latest/meta-data"); !errors.Is(err, errBlockedHost) {
		t.Errorf("link-local metadata IP not blocked, err=%v", err)
	}
}

func TestOpenAPIShape(t *testing.T) {
	body := []byte(`{"openapi":"3.0.0","info":{"title":"Pet Store","version":"1.2.3"}}`)
	title, ver, ok := openAPIShape("application/json", body)
	if !ok || title != "Pet Store" || ver != "1.2.3" {
		t.Errorf("openapi not detected: title=%q ver=%q ok=%v", title, ver, ok)
	}
	if _, _, ok := openAPIShape("text/html", []byte("<html>")); ok {
		t.Error("non-json wrongly detected as openapi")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
