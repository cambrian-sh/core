package archlint

import (
	"slices"
	"sort"
	"testing"
)

// Every rule must be able to fail (ENG-1484). testdata/planted (*.go.planted) is a fake module
// (example.com/core) carrying one planted violation per rule beside sites the
// rules must accept; each rule has to report exactly the planted findings.

const plantedModule = "example.com/core"

func keys(fs []finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.key)
	}
	sort.Strings(out)
	return out
}

func wantKeys(t *testing.T, rule string, got []finding, want ...string) {
	t.Helper()
	sort.Strings(want)
	if g := keys(got); !slices.Equal(g, want) {
		t.Errorf("[%s] on the planted tree:\n got: %q\nwant: %q", rule, g, want)
	}
}

func TestPlanted_ImportLayering(t *testing.T) {
	files := loadTree(t, "testdata/planted")
	wantKeys(t, "imports", ruleImports(files, plantedModule),
		"domain/port.go -> example.com/core/internal/feature",               // domain imports internal
		"domain/port.go -> github.com/cambrian-sh/cambrian-premium/plugins", // premium (twice: boundary + domain set)
		"domain/port.go -> github.com/cambrian-sh/cambrian-premium/plugins",
		"internal/feature/feature.go -> example.com/core/app",                        // internal imports app
		"internal/feature/feature.go -> example.com/core/internal/kernel",            // only app/cmd import kernel
		"internal/infrastructure/pgx/adapter.go -> example.com/core/internal/memory", // adapter imports a service
	)
}

func TestPlanted_RestrictedConstructors(t *testing.T) {
	files := loadTree(t, "testdata/planted")
	wantKeys(t, "constructors", ruleRestrictedConstructors(files, plantedModule),
		"internal/feature/feature.go calls postgres.NewPgVectorAdapter",
		"internal/feature/feature.go calls authz.NewEnforcingVectorStore",
		"cmd/tool/main.go calls postgres.NewPgVectorAdapter without a citation",
	)
}

func TestPlanted_DomainPortsTakeContext(t *testing.T) {
	files := loadTree(t, "testdata/planted")
	wantKeys(t, "domain-ctx", ruleDomainPortContext(files), "domain.Store.Load")
}

func TestPlanted_BackgroundContext(t *testing.T) {
	files := loadTree(t, "testdata/planted")
	// cmd/tool/main.go's Background is a main's root context: not reported.
	wantKeys(t, "background", ruleBackgroundContext(files), "internal/feature/feature.go Serve #1")
}

func TestPlanted_ClockDerivedIDs(t *testing.T) {
	files := loadTree(t, "testdata/planted")
	wantKeys(t, "clock-id", ruleClockDerivedIDs(files),
		"internal/feature/ids.go Mint ID #1",                // r.ID = fmt.Sprintf(..., time.Now().UnixNano())
		"internal/feature/ids.go Mint id #1",                // id := strconv.FormatInt(time.Now().UnixMilli(), 10)
		"internal/feature/ids.go Mint RunID #1",             // RunID: "run-" + now.Format(...), now := time.Now()
		"internal/feature/ids.go newJobID return #1",        // return of an id-named function
		"internal/feature/ids.go (package scope) bootID #1", // package-scope var
	)
}

// The allowlist checker itself fails on an unknown finding, a stale entry and
// an uncited reason.
func TestPlanted_AllowlistIsStrict(t *testing.T) {
	got := []finding{{rule: "x", key: "a", pos: "p", msg: "m"}}
	cases := []struct {
		name  string
		allow map[string]string
	}{
		{"unlisted finding", map[string]string{}},
		{"stale entry", map[string]string{"a": "ENG-1", "gone": "ENG-2"}},
		{"uncited reason", map[string]string{"a": "because"}},
	}
	for _, c := range cases {
		if !failsCheck(got, c.allow) {
			t.Errorf("%s: check passed, want a failure", c.name)
		}
	}
	if failsCheck(got, map[string]string{"a": "ENG-1484"}) {
		t.Error("an exact, cited allowlist failed check")
	}
}

// failsCheck runs check under a recorder and reports whether it failed.
func failsCheck(got []finding, allow map[string]string) bool {
	r := &recorder{}
	checkInto(r, "x", got, allow)
	return r.failed
}
