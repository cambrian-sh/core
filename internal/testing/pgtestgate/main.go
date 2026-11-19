// Command pgtestgate turns `go test -json` output from the Postgres packages into a gate
// that FAILS when a Postgres test did not run (T-20261118-internal-evidence-pg-outbox-tests-fail-on-an).
//
// Every core Pg test t.Skip()s without PG_TEST_DSN, and that skip is how the
// internal/evidence outbox, the kernel's forget-through-both-planes e2e and the rest
// stayed out of CI for their whole life: `make test-race` runs without a DSN, so they
// were green by skipping. This gate reads the JSON stream (`go test -json`) on stdin,
// echoes the human-readable test output to stdout, and exits non-zero when
//
//   - any test or package failed (or the build did);
//   - any test SKIPPED for a Postgres reason (pgSkip below: no DSN, a database nobody
//     migrated, a database whose name does not say test, no pg_dump) — a skip for
//     another reason (no Python agent env, not Linux) is fine and stays a skip;
//   - a package in Canaries did not PASS its canary test — the proof that the package's
//     Pg tests actually reached the database rather than all skipping some new way.
//
// Usage (Makefile `test-pg`, CI's Postgres step):
//
//	go test -race -p 1 -count=1 -json $(PG_PACKAGES) | go run ./internal/testing/pgtestgate
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Canaries names, for every package with Postgres tests, one test that must PASS. A new
// package with Pg tests is added here and to PG_PACKAGES in the Makefile; canaries_test.go
// fails when a canary's test function no longer exists.
var Canaries = map[string]string{
	"github.com/cambrian-sh/core/app":                              "TestPg_ToolGrants_OperatorGrantSurvivesRestart",
	"github.com/cambrian-sh/core/app/migratetest":                  "TestPg_ApplyLane",
	"github.com/cambrian-sh/core/internal/backup":                  "TestBackupRestoreDrill_EndToEnd_Pg",
	"github.com/cambrian-sh/core/internal/evidence":                "TestPgOutbox_PoisonDiesAfterFiveAttemptsAndIsListed",
	"github.com/cambrian-sh/core/internal/identity":                "TestRegistry_Contract/pg",
	"github.com/cambrian-sh/core/internal/infrastructure/postgres": "TestBindDelivery_RoundTrips",
	"github.com/cambrian-sh/core/internal/kernel":                  "TestPg_Forget_EndToEndThroughBothPlanes",
	"github.com/cambrian-sh/core/internal/knowledgecommit":         "TestPg_E2E_KnowledgeCommitReachesTheObjectRow",
	"github.com/cambrian-sh/core/internal/memory":                  "TestPg_FactLane_InjectedChunkScoredOnTheActiveModelsVector",
	"github.com/cambrian-sh/core/internal/memory/store":            "TestPg_SaveProfile_FailClosedAuthz_PersistsKernelAuthoredRow",
	"github.com/cambrian-sh/core/internal/memorynotes":             "TestPg_MemoryNotes_CASAndHistory",
	"github.com/cambrian-sh/core/internal/migrate":                 "TestBoot_Modes",
	"github.com/cambrian-sh/core/internal/objects":                 "TestPg_E2E_HistoryWritesMaintainTheObjectRow",
	"github.com/cambrian-sh/core/internal/ontology":                "TestPg_LoadSeedsOnceAndReloadsAppliedChanges",
	"github.com/cambrian-sh/core/internal/substrate/network":       "TestPg_E2E_AgentDraftApprovedByAPersonReachesTheNextWrite",
	"github.com/cambrian-sh/core/internal/substrate/operator":      "TestOperatorPlane_PostgresDown_IsUnavailableThenRecovers",
	"github.com/cambrian-sh/core/internal/testing/pgscratch":       "TestEnsureMigrated_EmptyDatabaseAtTheSharedDimension",
}

// pgSkip matches the skip messages of the Pg test helpers. A helper's skip message must
// keep one of these phrases (they all say why the database was not reached).
var pgSkip = regexp.MustCompile(`PG_TEST_DSN (not set|has no database name)|` +
	`run .?orchestrator migrate up|migrate up. first|refusing to touch|does not say test|` +
	`refusing to run the document-split migration|schema missing|pg_dump not on PATH`)

type event struct {
	Action     string
	Package    string
	ImportPath string // build-output / build-fail events name the package here
	Test       string
	Output     string
}

type key struct{ pkg, test string }

type result struct {
	failed     []string
	pgSkipped  []string
	missing    []string
	passed     int
	skipped    int
	sawPackage map[string]bool
}

func gate(in io.Reader, out io.Writer, canaries map[string]string) (result, error) {
	res := result{sawPackage: map[string]bool{}}
	outputs := map[key][]string{}
	final := map[key]string{}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil || ev.Action == "" {
			// Not a test2json event (a build error line, a stray print): show it.
			fmt.Fprintln(out, string(line))
			continue
		}
		k := key{ev.Package, ev.Test}
		switch ev.Action {
		case "output":
			fmt.Fprint(out, ev.Output)
			if ev.Test != "" {
				outputs[k] = append(outputs[k], ev.Output)
			}
		case "pass", "fail", "skip":
			if ev.Package != "" {
				res.sawPackage[ev.Package] = true
			}
			final[k] = ev.Action
		case "build-output":
			fmt.Fprint(out, ev.Output)
		case "build-fail":
			final[key{ev.ImportPath + " [build failed]", ""}] = "fail"
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	keys := make([]key, 0, len(final))
	for k := range final {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].pkg != keys[j].pkg {
			return keys[i].pkg < keys[j].pkg
		}
		return keys[i].test < keys[j].test
	})
	for _, k := range keys {
		name := strings.TrimSpace(k.pkg + " " + k.test)
		switch final[k] {
		case "fail":
			res.failed = append(res.failed, name)
		case "pass":
			if k.test != "" {
				res.passed++
			}
		case "skip":
			if k.test == "" {
				continue // a package with no test files
			}
			res.skipped++
			if msg := strings.Join(outputs[k], ""); pgSkip.MatchString(msg) {
				res.pgSkipped = append(res.pgSkipped, name+": "+lastReason(outputs[k]))
			}
		}
	}
	pkgs := make([]string, 0, len(canaries))
	for p := range canaries {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		if final[key{p, canaries[p]}] != "pass" {
			res.missing = append(res.missing, p+" "+canaries[p])
		}
	}
	return res, nil
}

// lastReason is the skip message line (the line before "--- SKIP").
func lastReason(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" && !strings.HasPrefix(l, "--- SKIP") && !strings.HasPrefix(l, "=== ") {
			return l
		}
	}
	return ""
}

func main() {
	res, err := gate(os.Stdin, os.Stdout, Canaries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtestgate: read go test -json: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("\npgtestgate: %d passed, %d skipped (%d for a Postgres reason), %d failed\n",
		res.passed, res.skipped, len(res.pgSkipped), len(res.failed))
	bad := false
	for _, f := range res.failed {
		fmt.Printf("FAIL: %s\n", f)
		bad = true
	}
	for _, s := range res.pgSkipped {
		fmt.Printf("FAIL: Postgres test skipped: %s\n", s)
		bad = true
	}
	for _, m := range res.missing {
		fmt.Printf("FAIL: canary did not pass (skipped, failed or not run): %s\n", m)
		bad = true
	}
	if bad {
		fmt.Println("pgtestgate: FAIL — every Postgres test must reach the migrated PG_TEST_DSN database")
		os.Exit(1)
	}
	fmt.Println("pgtestgate: PASS — no Postgres test skipped; every package's canary passed")
}
