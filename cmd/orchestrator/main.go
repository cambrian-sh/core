package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cambrian-sh/core/app"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
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
		if err := runMigrateCmd(context.Background(), os.Args[2:]); err != nil {
			slog.Error("❌ migrate failed", "err", err)
			os.Exit(1)
		}
		return
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

// runMigrateCmd implements `orchestrator migrate [up|status]`. It loads the same
// config the kernel boots with (so the DB DSN + embedder dimension match) and drives
// the pure-Go runner. PLAT-02 / ADR-0064.
func runMigrateCmd(ctx context.Context, args []string) error {
	sub := "up"
	if len(args) > 0 {
		sub = args[0]
	}
	base := config.ResolveBaseDir()
	cfg, err := config.LoadConfig(filepath.Join(base, "configs", "config.json"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	switch sub {
	case "up":
		if err := postgres.RunMigrations(ctx, cfg); err != nil {
			return err
		}
		fmt.Println("migrations applied ✓")
		return nil
	case "status":
		recs, err := postgres.MigrationStatus(ctx, cfg)
		if err != nil {
			return err
		}
		fmt.Printf("%-8s %-24s %s\n", "VERSION", "NAME", "STATUS")
		for _, r := range recs {
			status := "applied " + r.AppliedAt.Format("2006-01-02 15:04:05")
			if r.Pending {
				status = "PENDING"
			}
			fmt.Printf("%-8d %-24s %s\n", r.Version, r.Name, status)
		}
		return nil
	default:
		return fmt.Errorf("unknown migrate subcommand %q (want: up | status)", sub)
	}
}
