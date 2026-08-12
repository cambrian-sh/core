// `cambrian-orchestrator setup` — self-installing bootstrap (ADR-0122).
//
// The binary IS the installer: it materializes the install prefix (~/.cambrian),
// provisions the dependencies that are missing (local Postgres via docker compose,
// the Python agent runtime via uv — downloading uv itself, and CPython through it,
// when absent), writes the kernel config bundle, applies DB migrations in-process
// (ADR-0064), then starts the kernel detached and verifies readiness via the
// grpc.health.v1 service (ADR-0065, DB-gated).
//
// Ported from the retired Bun CLI's `cambrian init` sequential runner (CLI-005):
// every step is check-then-do and idempotent, so re-running `setup` repairs a
// half-install. Prompts are plain stdin/stdout — no TUI dependency — and every
// prompt takes its default under --yes or a non-TTY stdin, so the same flow works
// under `curl | sh`, CI, and an interactive terminal.
//
// LLM providers are deliberately NOT configured here: the kernel boots without
// them and keys are added later via the operator console (ADR-0101 store) or
// configs/providers.json. Exit codes: 0 ok, 2 finished-with-warnings, 1 fatal.
package app

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// setupDB is the Postgres connection the user chose (or the defaults, which
// match the generated docker-compose service).
type setupDB struct {
	host, port, user, password, dbname string
}

type setupState struct {
	ui      *setupUI
	prefix  string // install root (~/.cambrian)
	origCwd string // CWD before setup chdirs to prefix (used for dev-tree SDK lookup)
	binPath string // installed kernel binary (<prefix>/bin/cambrian-orchestrator)

	skipModels bool
	skipPython bool
	skipJS     bool
	noStart    bool
	waitReady  bool

	degraded bool

	db         setupDB
	dbUp       bool
	vectorOK   bool
	dockerOK   bool
	ollamaOK   bool
	uvBin      string
	venvPy     string
	bunBin     string
	nodeBin    string
	agentsDir  string
	serverPort string
}

// warnf reports a warning that leaves the install degraded (exit 2, kernel not
// auto-started). Non-degrading warnings go through ui.line("warn", …) directly.
func (s *setupState) warnf(label, detail string) {
	s.ui.line("warn", label, detail)
	s.degraded = true
}

// RunSetup implements the `setup` subcommand. args is os.Args past the "setup"
// token. Returns the process exit code.
func RunSetup(ctx context.Context, args []string) int {
	fl := flag.NewFlagSet("setup", flag.ContinueOnError)
	fl.SetOutput(os.Stdout)
	var (
		yes, yShort                    bool
		home                           string
		skipModels, skipPython, skipJS bool
		noStart, waitReady             bool
	)
	fl.BoolVar(&yes, "yes", false, "non-interactive: every prompt takes its default")
	fl.BoolVar(&yShort, "y", false, "shorthand for --yes")
	fl.StringVar(&home, "home", "", "install prefix (default $CAMBRIAN_HOME, else ~/.cambrian)")
	fl.BoolVar(&skipModels, "skip-models", false, "skip embedder/reranker/docling model pre-fetch")
	fl.BoolVar(&skipPython, "skip-python", false, "skip Python agent-runtime provisioning (venv/SDK/deps)")
	fl.BoolVar(&skipJS, "skip-js", false, "skip JS agent-runtime provisioning (bun/node probe + installs, ADR-0125)")
	fl.BoolVar(&noStart, "no-start", false, "configure and migrate, but do not start the kernel")
	fl.BoolVar(&waitReady, "wait", false, "block until the started kernel reports DB-gated SERVING (default: fire-and-forget)")
	fl.Usage = func() {
		fmt.Println("usage: cambrian-orchestrator setup [--yes] [--home DIR] [--skip-models] [--skip-python] [--skip-js] [--no-start] [--wait]")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	yes = yes || yShort

	prefix := home
	if prefix == "" {
		prefix = os.Getenv("CAMBRIAN_HOME")
	}
	if prefix == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			fmt.Println("cannot resolve home directory:", err)
			return 1
		}
		prefix = filepath.Join(h, ".cambrian")
	}
	abs, err := filepath.Abs(prefix)
	if err != nil {
		fmt.Println("cannot resolve install prefix:", err)
		return 1
	}
	origCwd, _ := os.Getwd()

	// DB defaults honor pre-set CAMBRIAN_DATABASE__* env (the kernel's own
	// highest-precedence override), so an unattended `setup --yes` can target a
	// specific database; the interactive prompts then start from those values.
	db := setupDB{host: "localhost", port: "5432", user: "cambrian_admin", password: "cambrian_password", dbname: "cambrian_db"}
	for _, f := range []struct {
		env string
		dst *string
	}{
		{"CAMBRIAN_DATABASE__HOST", &db.host},
		{"CAMBRIAN_DATABASE__PORT", &db.port},
		{"CAMBRIAN_DATABASE__USER", &db.user},
		{"CAMBRIAN_DATABASE__PASSWORD", &db.password},
		{"CAMBRIAN_DATABASE__DBNAME", &db.dbname},
	} {
		if v := os.Getenv(f.env); v != "" {
			*f.dst = v
		}
	}

	s := &setupState{
		ui:         newSetupUI(yes),
		prefix:     abs,
		origCwd:    origCwd,
		skipModels: skipModels,
		skipPython: skipPython,
		skipJS:     skipJS,
		noStart:    noStart,
		waitReady:  waitReady,
		db:         db,
		serverPort: "50051",
	}
	u := s.ui

	fmt.Println()
	fmt.Println(u.bold("  Cambrian setup") + u.dim("  ·  cambrian-orchestrator setup"))
	fmt.Println(u.dim("  Installs and configures everything the kernel needs. Safe to re-run."))
	fmt.Println()

	if !s.stepInstall() {
		return 1
	}
	s.stepPreflight(ctx)
	s.stepDatabase(ctx)
	s.stepPython(ctx)
	s.stepJSRuntime(ctx)
	s.stepModels(ctx)
	s.stepConfig()
	s.stepMigrate(ctx)
	s.stepOperator()
	running := s.stepStart(ctx)

	fmt.Println()
	switch {
	case s.degraded:
		fmt.Println(u.yellow(u.bold("  Setup finished with warnings.")) + u.dim("  Fix the items above, then re-run setup — it picks up where it left off."))
	case running && s.waitReady:
		fmt.Println(u.green(u.bold("  ✓ Cambrian is ready.")) + u.dim("  kernel serving on :"+s.serverPort))
		fmt.Println(u.dim("     LLM provider keys can be added later from the operator console."))
	case running:
		fmt.Println(u.green(u.bold("  ✓ Setup complete.")) + u.dim("  kernel booting on :"+s.serverPort+" — check: cambrian-orchestrator status"))
		fmt.Println(u.dim("     LLM provider keys can be added later from the operator console."))
	default:
		fmt.Println(u.green(u.bold("  ✓ Setup complete.")))
	}
	fmt.Println()
	if s.degraded {
		return 2
	}
	return 0
}

// ---- plain terminal UI (no TUI dependency; degrades to defaults + no color) ----

type setupUI struct {
	interactive bool
	color       bool
	in          *bufio.Reader
}

func newSetupUI(yes bool) *setupUI {
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		interactive = true
	}
	color := false
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		color = true
	}
	// Classic conhost ships with VT processing off; Windows Terminal (WT_SESSION)
	// and TERM-setting shells handle ANSI fine.
	if runtime.GOOS == "windows" && os.Getenv("WT_SESSION") == "" && os.Getenv("TERM") == "" && os.Getenv("ANSICON") == "" {
		color = false
	}
	return &setupUI{interactive: interactive && !yes, color: color, in: bufio.NewReader(os.Stdin)}
}

func (u *setupUI) c(code, s string) string {
	if !u.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func (u *setupUI) bold(s string) string   { return u.c("1", s) }
func (u *setupUI) dim(s string) string    { return u.c("2", s) }
func (u *setupUI) green(s string) string  { return u.c("32", s) }
func (u *setupUI) red(s string) string    { return u.c("31", s) }
func (u *setupUI) yellow(s string) string { return u.c("33", s) }

func (u *setupUI) mark(state string) string {
	switch state {
	case "ok":
		return u.green("✓")
	case "warn":
		return u.yellow("!")
	default:
		return u.red("✗")
	}
}

func (u *setupUI) line(state, label, detail string) {
	if detail != "" {
		detail = u.dim("  " + detail)
	}
	fmt.Printf("  %s %s%s\n", u.mark(state), label, detail)
}

func (u *setupUI) section(title string) { fmt.Println(u.bold(title)) }

// ask prompts for a value; empty input (or non-interactive mode) yields def.
func (u *setupUI) ask(q, def string) string {
	if !u.interactive {
		return def
	}
	suffix := ""
	if def != "" {
		suffix = u.dim(" [" + def + "]")
	}
	fmt.Printf("  %s%s: ", q, suffix)
	line, err := u.in.ReadString('\n')
	if err != nil {
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

type setupChoice struct{ key, label string }

func (u *setupUI) askChoice(q string, opts []setupChoice, def string) string {
	if !u.interactive {
		return def
	}
	fmt.Println("  " + q)
	for _, o := range opts {
		k := "(" + o.key + ")"
		if o.key == def {
			k = u.bold(k)
		}
		fmt.Println("    " + k + " " + o.label)
	}
	a := u.ask("choice", def)
	for _, o := range opts {
		if o.key == a {
			return a
		}
	}
	return def
}
