package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/storage"
)

// The `orchestrator config` subcommand group (ADR-0101 D8): inspect and clear
// the embedded store's config overrides without a running console.
//
// It replaces the hand-written bbolt read/delete program used on 2026-11-01 to
// find why a generator added to providers.json was invisible
// (T-20261101-config-store-silently-shadows-file-edits-a).
//
// OFFLINE, like `mcp token`: config.db is bbolt, single-process by construction,
// so while the kernel runs it holds the file lock and these commands refuse with
// a message saying so — the console's revert (DeleteConfig) is the online path.
// `list` opens the store read-only and changes nothing.

// configStoreLockWait bounds how long the CLI waits for the store's lock before
// concluding the kernel holds it.
const configStoreLockWait = time.Second

const configUsage = `usage:
  orchestrator config overrides list  [--dir <kernel-dir>]
  orchestrator config overrides unset <key>... [--dir <kernel-dir>]

list   shows every config override held in configs/config.db and, for each,
       the config file that also sets the key and whether the values differ
       (a differing override is why an edit to that file has no effect).
unset  removes overrides by exact key (as list prints it), so the key falls
       back to the config files. Takes effect at the next kernel start.

Both are OFFLINE: stop the kernel first (the config store is single-process).
While the kernel runs, use the console's "revert to file" instead.
Set CAMBRIAN_CONFIG_STORE to point at a store elsewhere, as for the kernel.
`

// RunConfig dispatches `config overrides list|unset`. It returns a process exit
// code, matching RunSetup/RunStatus/RunMCP.
func RunConfig(_ context.Context, args []string) int {
	return runConfig(args, os.Stdout, os.Stderr)
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[0] != "overrides" {
		fmt.Fprint(stderr, configUsage)
		return 2
	}
	verb := args[1]
	fs := flag.NewFlagSet("config overrides "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", config.ResolveBaseDir(), "kernel base directory (the one holding configs/)")
	// Interleaved parse: `unset a.b --dir x c.d` must keep every key and the flag.
	var keys []string
	rest := args[2:]
	for {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		keys = append(keys, fs.Arg(0))
		rest = fs.Args()[1:]
	}

	path, off := configStorePath(*dir)
	if off {
		fmt.Fprintf(stderr, "the config store is disabled (%s=off); there are no overrides to manage\n", ConfigStoreEnv)
		return 1
	}
	configPath := filepath.Join(*dir, "configs", "config.json")

	switch verb {
	case "list":
		if len(keys) > 0 {
			fmt.Fprint(stderr, configUsage)
			return 2
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Fprintf(stdout, "no config store at %s — nothing overrides the config files\n", path)
			return 0
		}
		store, err := storage.OpenConfigStoreReadOnly(path, configStoreLockWait)
		if err != nil {
			return reportStoreOpenError(stderr, path, err)
		}
		defer func() { _ = store.Close() }()
		return listOverrides(stdout, stderr, configPath, path, store)

	case "unset":
		if len(keys) == 0 {
			fmt.Fprint(stderr, configUsage)
			return 2
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Fprintf(stderr, "no config store at %s\n", path)
			return 1
		}
		store, err := storage.OpenConfigStoreTimeout(path, configStoreLockWait)
		if err != nil {
			return reportStoreOpenError(stderr, path, err)
		}
		defer func() { _ = store.Close() }()
		return unsetOverrides(stdout, stderr, store, keys)

	default:
		fmt.Fprintf(stderr, "unknown config overrides subcommand %q\n%s", verb, configUsage)
		return 2
	}
}

// configStorePath resolves the store file the kernel at baseDir uses, honouring
// CAMBRIAN_CONFIG_STORE exactly as OpenConfigStore does.
func configStorePath(baseDir string) (path string, off bool) {
	path = os.Getenv(ConfigStoreEnv)
	if path == "off" {
		return "", true
	}
	if path == "" {
		path = filepath.Join(baseDir, "configs", "config.db")
	}
	return path, false
}

func reportStoreOpenError(stderr io.Writer, path string, err error) int {
	if storage.IsConfigStoreLocked(err) {
		fmt.Fprintf(stderr, "%s is locked by another process — the kernel is probably running.\n"+
			"Stop it first (`orchestrator stop`), or use the console's revert while it runs.\n", path)
		return 1
	}
	fmt.Fprintf(stderr, "cannot open the config store %s: %v\n", path, err)
	return 1
}

func listOverrides(stdout, stderr io.Writer, configPath, storePath string, store *storage.BoltConfigStore) int {
	overrides, err := store.Overrides()
	if err != nil {
		fmt.Fprintf(stderr, "read overrides: %v\n", err)
		return 1
	}
	if len(overrides) == 0 {
		fmt.Fprintf(stdout, "no overrides in %s — every key comes from the config files (or defaults/env)\n", storePath)
		return 0
	}
	report := config.StoreOverrideReport(configPath, overrides)
	fmt.Fprintf(stdout, "%d override(s) in %s:\n\n", len(report), storePath)
	differing := 0
	for _, o := range report {
		fmt.Fprintf(stdout, "  %s = %s\n", o.Key, config.DisplayValue(o.StoreValue, o.Secret))
		switch {
		case o.File == "":
			fmt.Fprintln(stdout, "      no config file sets this key")
		case o.Differs:
			differing++
			fmt.Fprintf(stdout, "      OVERRIDES %s = %s  (edits to %s have no effect for this key)\n",
				o.File, config.DisplayValue(o.FileValue, o.Secret), o.File)
		default:
			fmt.Fprintf(stdout, "      same value as %s\n", o.File)
		}
	}
	if differing > 0 {
		fmt.Fprintf(stdout, "\n%d override(s) beat a different file value. Clear one with:\n  orchestrator config overrides unset <key>\n", differing)
	}
	return 0
}

func unsetOverrides(stdout, stderr io.Writer, store *storage.BoltConfigStore, keys []string) int {
	overrides, err := store.Overrides()
	if err != nil {
		fmt.Fprintf(stderr, "read overrides: %v\n", err)
		return 1
	}
	code := 0
	for _, key := range keys {
		if _, ok := overrides[key]; !ok {
			// Reported, not silently accepted: a typo here leaves the real
			// override in place while the operator believes it is gone.
			fmt.Fprintf(stderr, "no override %q in the store (run `orchestrator config overrides list`)\n", key)
			code = 1
			continue
		}
		if err := store.DeleteOverride(key); err != nil {
			fmt.Fprintf(stderr, "unset %s: %v\n", key, err)
			code = 1
			continue
		}
		fmt.Fprintf(stdout, "unset %s — the config files supply it from the next kernel start\n", key)
	}
	return code
}
