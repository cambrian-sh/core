package main

import (
	"context"
	"errors"
	"fmt"
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
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version" || os.Args[1] == "version") {
		fmt.Println("cambrian-core", version)
		return
	}
	// PLAT-02 / ADR-0064: `migrate [up|status]` DB migration subcommand.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := app.RunMigrate(context.Background(), os.Args[2:]); err != nil {
			slog.Error("❌ migrate failed", "err", err)
			os.Exit(1)
		}
		return
	}
	// ADR-0122: the binary is its own installer and lifecycle manager. `setup`
	// bootstraps dependencies + config + migrations, then starts and
	// health-verifies the kernel; `status`/`stop` manage the detached kernel a
	// setup run left behind.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			os.Exit(app.RunSetup(context.Background(), os.Args[2:]))
		case "status":
			os.Exit(app.RunStatus(context.Background()))
		case "stop":
			os.Exit(app.RunStop(context.Background()))
		// ADR-0126 E5: inbound-MCP client-token lifecycle (offline) and the
		// stdio bridge relaying a local MCP client to the kernel's endpoint.
		case "mcp":
			os.Exit(app.RunMCP(context.Background(), os.Args[2:]))
		}
	}
	if err := app.Run(context.Background(), app.DefaultOptions()); err != nil {
		var cfgErr *config.ConfigError
		if errors.As(err, &cfgErr) {
			slog.Error("❌ Configuration error — fix the problem and restart", "field", cfgErr.Field, "detail", cfgErr.Message)
		} else {
			slog.Error("❌ Kernel Panic", "err", err)
		}
		os.Exit(1)
	}
}
