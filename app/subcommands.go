package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Subcommand is one offline verb of an orchestrator binary: it runs instead of
// the kernel and returns the process exit code.
type Subcommand struct {
	Name    string
	Summary string
	Run     func(ctx context.Context, args []string) int
}

// SubcommandSpec is what differs between the binaries that share the verb
// table: the name and version `version` prints, and the Options `migrate`
// composes (premium adds its schema-migration lane). Nothing else may differ —
// a per-binary verb list is how the OSS and premium binaries drifted before
// (T-20261103-premium-cmd-orchestrator-boots-a-full-kernel-on, ENG-1527).
type SubcommandSpec struct {
	// Binary is the name printed by `version` and in usage errors
	// ("cambrian-core", "cambrian-premium").
	Binary string
	// Version is the build version (-ldflags "-X main.version=…").
	Version string
	// MigrateOptions, when set, builds the Options `migrate` composes
	// (RunMigrateWith); nil means core's migrations only (RunMigrate).
	MigrateOptions func() Options
}

// Subcommands is the ONE dispatch table of the orchestrator binaries. The OSS
// main and the premium main both dispatch through it, so a verb added here
// exists in both, and every entry delegates to the shared app.Run*
// implementation.
func Subcommands(spec SubcommandSpec) []Subcommand {
	return []Subcommand{
		{"version", "print the binary version", func(context.Context, []string) int {
			fmt.Println(spec.Binary, spec.Version)
			return 0
		}},
		// PLAT-02 / ADR-0064: `migrate [up|status]`.
		{"migrate", "apply or report database migrations: migrate [up|status]", func(ctx context.Context, args []string) int {
			var err error
			if spec.MigrateOptions != nil {
				err = RunMigrateWith(ctx, args, spec.MigrateOptions())
			} else {
				err = RunMigrate(ctx, args)
			}
			if err != nil {
				slog.Error("❌ migrate failed", "err", err)
				return 1
			}
			return 0
		}},
		// ADR-0122: the binary is its own installer and lifecycle manager. `setup`
		// bootstraps dependencies + config + migrations, then starts and
		// health-verifies the kernel; `status`/`stop` manage the detached kernel a
		// setup run left behind.
		{"setup", "install dependencies, config and migrations, then start the kernel", RunSetup},
		{"status", "report the detached kernel a setup run started", func(ctx context.Context, _ []string) int { return RunStatus(ctx) }},
		{"stop", "stop the detached kernel a setup run started", func(ctx context.Context, _ []string) int { return RunStop(ctx) }},
		// ADR-0126 E5: inbound-MCP client-token lifecycle (offline) and the stdio
		// bridge relaying a local MCP client to the kernel's endpoint.
		{"mcp", "manage inbound-MCP client tokens / run the stdio bridge", RunMCP},
		// ADR-0142 D3: offline principal-registry administration (pre-provision an
		// OIDC subject, local accounts, suspend/activate).
		{"principal", "administer the principal registry offline", RunPrincipal},
		// ADR-0101 D8: list and clear config.db overrides offline (the store beats
		// every config file, so an override is why a file edit "didn't take").
		{"config", "list or clear config-store overrides offline", RunConfig},
		// ENG-681: deployment-owned backup, verify, rotation, restore and the
		// weekly restore drill (internal/backup; docs/backup-and-restore.md).
		{"backup", "back up, verify, rotate, restore or drill the kernel's durable state", RunBackup},
		// ADR-0157 D7: the kernel-managed sandbox Python env — its boot check and
		// the build the kernel names as the fix when it refuses Python agents.
		{"python-env", "check or build the kernel-managed sandbox Python env", RunPythonEnv},
	}
}

// DispatchSubcommand routes argv (os.Args[1:]) before the kernel boots. It
// returns handled=false only when the kernel should boot: no arguments, or a
// first argument that is a flag (Run calls flag.Parse, so `-h`/`--help` and
// any kernel flag keep working).
//
// Any other first argument must name a subcommand. An unrecognised word is a
// usage error (exit 2), never a kernel boot: before, a typo (`confg`), `start`
// or a verb only one binary knew fell through to Run, which boots the full
// kernel against whatever database the config names and applies pending
// migrations (ENG-1527).
func DispatchSubcommand(ctx context.Context, args []string, cmds []Subcommand, stderr io.Writer) (code int, handled bool) {
	if len(args) == 0 {
		return 0, false
	}
	verb := args[0]
	if verb == "--version" || verb == "-version" {
		verb = "version"
	}
	if strings.HasPrefix(verb, "-") {
		return 0, false
	}
	if verb == "help" {
		printSubcommandUsage(stderr, cmds)
		return 0, true
	}
	for _, c := range cmds {
		if c.Name == verb {
			return c.Run(ctx, args[1:]), true
		}
	}
	fmt.Fprintf(stderr, "orchestrator: unknown subcommand %q (the kernel was NOT started)\n\n", verb)
	printSubcommandUsage(stderr, cmds)
	return 2, true
}

func printSubcommandUsage(w io.Writer, cmds []Subcommand) {
	fmt.Fprintln(w, "usage: orchestrator [flags]            boot the kernel")
	fmt.Fprintln(w, "       orchestrator <subcommand> [args]")
	fmt.Fprintln(w, "\nsubcommands:")
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-10s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintf(w, "  %-10s %s\n", "help", "show this message")
}
