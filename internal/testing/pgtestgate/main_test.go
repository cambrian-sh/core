package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func ev(action, pkg, test, output string) string {
	s := `{"Action":"` + action + `","Package":"` + pkg + `"`
	if test != "" {
		s += `,"Test":"` + test + `"`
	}
	if output != "" {
		s += `,"Output":` + quote(output)
	}
	return s + "}\n"
}

func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

const p = "example.com/p"

func run(t *testing.T, stream string, canaries map[string]string) result {
	t.Helper()
	res, err := gate(strings.NewReader(stream), io.Discard, canaries)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGate_AllPassWithCanaryIsClean(t *testing.T) {
	res := run(t, ev("run", p, "TestPg_A", "")+ev("pass", p, "TestPg_A", "")+ev("pass", p, "", ""),
		map[string]string{p: "TestPg_A"})
	if len(res.failed)+len(res.pgSkipped)+len(res.missing) != 0 || res.passed != 1 {
		t.Fatalf("clean run flagged: %+v", res)
	}
}

// Every skip phrase the core Pg helpers use is a Postgres skip, and fails the gate.
func TestGate_PostgresSkipsFail(t *testing.T) {
	for _, msg := range []string{
		"x_test.go:12: PG_TEST_DSN not set; skipping integration test that requires PostgreSQL",
		"x_test.go:12: PG_TEST_DSN has no database name",
		"x_test.go:12: tool_grants missing (run `orchestrator migrate up` first)",
		"x_test.go:12: principal registry schema missing (run `orchestrator migrate up`): boom",
		`x_test.go:12: refusing to touch "cambrian": its name does not say test`,
		"x_test.go:12: refusing to run the document-split migration against cambrian",
		"x_test.go:12: pg_dump not on PATH and PG_TEST_CONTAINER not set",
	} {
		res := run(t, ev("output", p, "TestPg_A", msg+"\n")+ev("output", p, "TestPg_A", "--- SKIP: TestPg_A (0.00s)\n")+
			ev("skip", p, "TestPg_A", ""), nil)
		if len(res.pgSkipped) != 1 {
			t.Errorf("skip %q not flagged: %+v", msg, res)
		}
	}
}

// A skip for a non-Postgres reason (no Python agent env, wrong OS) stays a skip; even
// one whose message names PG_TEST_DSN among the variables it wants.
func TestGate_OtherSkipsPass(t *testing.T) {
	for _, msg := range []string{
		"x_test.go:12: set ARTIFACT_E2E_PY and PG_TEST_DSN to run the artifact e2e",
		"x_test.go:12: bubblewrap is Linux-only",
	} {
		res := run(t, ev("output", p, "TestE2E_A", msg+"\n")+ev("skip", p, "TestE2E_A", ""), nil)
		if len(res.pgSkipped) != 0 || res.skipped != 1 {
			t.Errorf("skip %q flagged: %+v", msg, res)
		}
	}
}

func TestGate_FailuresAndMissingCanariesFail(t *testing.T) {
	stream := ev("fail", p, "TestPg_B", "") + ev("fail", p, "", "") +
		`{"Action":"build-fail","ImportPath":"example.com/q [example.com/q.test]"}` + "\n" +
		"# example.com/q\nq.go:1: syntax error\n"
	res := run(t, stream, map[string]string{p: "TestPg_A", "example.com/r": "TestPg_R"})
	if len(res.failed) != 3 {
		t.Errorf("failed = %v, want the test, its package and the build failure", res.failed)
	}
	if len(res.missing) != 2 {
		t.Errorf("missing canaries = %v, want both", res.missing)
	}
}

// Every canary names a test function that exists in its package's _test.go files, so a
// rename cannot turn the canary check into a permanent failure nobody understands — or,
// worse, be "fixed" by deleting the canary.
func TestCanaries_NameExistingTests(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	const mod = "github.com/cambrian-sh/core"
	for pkg, test := range Canaries {
		dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(pkg, mod), "/")))
		fn := strings.SplitN(test, "/", 2)[0]
		decl := regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(fn) + `\(t \*testing\.T\)`)
		files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil || len(files) == 0 {
			t.Errorf("%s: no test files in %s (%v)", pkg, dir, err)
			continue
		}
		found := false
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if decl.Match(b) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("canary %s: no func %s in %s", pkg, fn, dir)
		}
	}
}

const module = "github.com/cambrian-sh/core"

// Every package whose tests reach PG_TEST_DSN (directly or through pgscratch) has a
// canary, and the Makefile's PG_PACKAGES — what CI runs — lists exactly the canary
// packages. A new Pg package that joins neither would run its Pg tests nowhere.
func TestCanaries_CoverEveryPgPackageAndMatchTheMakefile(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	uses := regexp.MustCompile(`PG_TEST_DSN|pgscratch\.(Pool|Migrated)\(`)
	pgDirs := map[string]bool{}
	for _, top := range []string{"app", "internal", "domain", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if uses.Match(b) {
				rel, err := filepath.Rel(root, filepath.Dir(path))
				if err != nil {
					return err
				}
				pgDirs[module+"/"+filepath.ToSlash(rel)] = true
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	delete(pgDirs, module+"/internal/testing/pgtestgate") // this package's fixtures quote the messages
	if len(pgDirs) < len(Canaries)/2 {
		t.Fatalf("found only %d Pg test packages; the walk is broken", len(pgDirs))
	}
	for d := range pgDirs {
		if _, ok := Canaries[d]; !ok {
			t.Errorf("%s has Postgres tests but no canary in pgtestgate.Canaries (and so is not in PG_PACKAGES)", d)
		}
	}

	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?ms)^PG_PACKAGES :=(.*?)\n\n`).FindSubmatch(mk)
	if m == nil {
		t.Fatal("Makefile: no PG_PACKAGES block")
	}
	listed := map[string]bool{}
	for _, f := range strings.Fields(strings.ReplaceAll(string(m[1]), `\`, " ")) {
		listed[module+"/"+strings.TrimSuffix(strings.TrimPrefix(f, "./"), "/")] = true
	}
	for p := range Canaries {
		if !listed[p] {
			t.Errorf("canary package %s is missing from the Makefile's PG_PACKAGES", p)
		}
	}
	for p := range listed {
		if _, ok := Canaries[p]; !ok {
			t.Errorf("PG_PACKAGES lists %s, which has no canary", p)
		}
	}
}
