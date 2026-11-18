package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/cambrian-sh/core/app"
	"github.com/cambrian-sh/core/internal/config"
)

// version is set at build time: -ldflags "-X main.version=$(git describe --tags --always)".
// Defaults to "dev" for un-tagged local builds.
var version = "dev"

// main is a thin shell over app.Run — the composition root lives in the importable
// `app` package so a downstream (premium) binary can reuse the same bootstrap and
// inject proprietary components via app.Options. ADR-0057 (Model C).
func main() {
	// Offline subcommands (version, migrate, setup, status, stop, mcp, principal,
	// config, backup) run before app.Run, which boots the full kernel. The table
	// and dispatch live in app/subcommands.go, shared with the premium binary; an
	// unrecognised non-flag first argument is a usage error (exit 2), never a
	// kernel boot (ENG-1527).
	cmds := app.Subcommands(app.SubcommandSpec{Binary: "cambrian-core", Version: version})
	if code, handled := app.DispatchSubcommand(context.Background(), os.Args[1:], cmds, os.Stderr); handled {
		os.Exit(code)
	}
	if err := app.Run(context.Background(), app.DefaultOptions()); err != nil {
		var cfgErr *config.ConfigError
		if errors.As(err, &cfgErr) {
			slog.Error("❌ Configuration error — fix the problem and restart", "field", cfgErr.Field, "detail", cfgErr.Message)
		} else if errors.Is(err, config.ErrMalformedConfigFile) {
			// ENG-2121: a present config file that does not parse; err names it.
			slog.Error("❌ Configuration file does not parse — fix or remove it and restart", "err", err)
		} else {
			slog.Error("❌ Kernel Panic", "err", err)
		}
		os.Exit(1)
	}
}
