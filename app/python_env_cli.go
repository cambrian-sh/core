package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/pyenv"
)

const pythonEnvUsage = `usage: cambrian-orchestrator python-env <status|build> [--dir DIR]

  status   run the boot check of the kernel-managed sandbox Python env (ADR-0157 D7)
  build    build (or repair) it now: pinned + hash-verified Python, the release's
           hashed base lock, wheels only; honours execution.agents.python
           (python_mirror_url, installer_mirror_url, index_url, wheelhouse, offline)

The block is read from DIR/configs/config.json.
`

// RunPythonEnv implements the `python-env` subcommand: the boot check and the
// build by hand, the fix the kernel names when it refuses Python agents
// (ADR-0157 D7.11.2). args is os.Args past the "python-env" token.
func RunPythonEnv(ctx context.Context, args []string) int {
	return runPythonEnv(ctx, args, os.Stdout, os.Stderr)
}

func runPythonEnv(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, pythonEnvUsage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("python-env "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", config.ResolveBaseDir(), "kernel base directory (the one holding configs/)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	configDir := filepath.Join(*dir, "configs")
	pc, err := config.ReadAgentPythonBlock(filepath.Join(configDir, "config.json"))
	if err != nil {
		fmt.Fprintln(stderr, "execution.agents.python:", err)
		return 1
	}
	b, err := newPythonEnvBuilder(pc, *dir, configDir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	switch verb {
	case "status":
		st, err := b.Check(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		printPythonEnvStatus(stdout, b.Root, st)
		if !st.Usable() {
			fmt.Fprintf(stdout, "fix: %s\n", pyenv.FixCommand)
			return 1
		}
		return 0
	case "build":
		b.Actor = "operator-cli"
		b.Progress = func(s string) { fmt.Fprintln(stdout, "  "+s) }
		res, err := b.Build(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "build failed:", err)
			return 1
		}
		st, err := b.Check(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if res.Reused {
			fmt.Fprintln(stdout, "  generation already built and healthy; made current")
		}
		printPythonEnvStatus(stdout, b.Root, st)
		if !st.Usable() {
			return 1
		}
		return 0
	}
	fmt.Fprint(stderr, pythonEnvUsage)
	return 2
}

func printPythonEnvStatus(w io.Writer, root string, st pyenv.Status) {
	fmt.Fprintf(w, "sandbox Python env: %s\n", st.State)
	fmt.Fprintf(w, "  root:        %s\n", root)
	if st.Generation != "" {
		fmt.Fprintf(w, "  generation:  %s (%d packages)\n", st.Generation, st.Packages)
	}
	if st.Interpreter != "" {
		fmt.Fprintf(w, "  interpreter: %s\n", st.Interpreter)
	}
	if st.Reason != "" {
		fmt.Fprintf(w, "  reason:      %s\n", st.Reason)
	}
}
