package app

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// RunMigrate implements the `migrate [up|status]` DB migration subcommand. It lives in the
// importable `app` package (not in cmd/orchestrator) so BOTH the OSS binary and the premium
// binary can dispatch to it — premium is a separate module and cannot reach core's
// internal/config or internal/infrastructure/postgres directly. PLAT-02 / ADR-0064.
//
// It loads the same config the kernel boots with (via ResolveBaseDir → configs/config.json),
// so the DB DSN and embedder dimension match the running kernel exactly, then drives the
// pure-Go runner. `args` is os.Args past the "migrate" token (e.g. ["up"] or ["status"]).
func RunMigrate(ctx context.Context, args []string) error {
	return RunMigrateWith(ctx, args, Options{})
}

// RunMigrateWith is RunMigrate for a binary that carries plugins: opts.Plugins are
// composed (Register only — nothing is built or served) and every schema migration
// lane they register (Registry.AddMigrations) is applied after core's on `up` and
// listed after core's on `status`, each line prefixed by its namespace
// (T-20261113-premium-has-no-schema-migration-chain-41-create). The premium binary
// calls this with the Options it boots with; with no plugins it is RunMigrate.
func RunMigrateWith(ctx context.Context, args []string, opts Options) error {
	composed, err := applyPlugins(opts)
	if err != nil {
		return fmt.Errorf("compose plugins for migrate: %w", err)
	}
	lanes := composed.migrations
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
		if err := postgres.RunPluginMigrations(ctx, cfg, lanes); err != nil {
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
		laneRecs, err := postgres.PluginMigrationStatus(ctx, cfg, lanes)
		if err != nil {
			return err
		}
		for _, r := range laneRecs {
			status := "applied " + r.AppliedAt.Format("2006-01-02 15:04:05")
			if r.Pending {
				status = "PENDING"
			}
			fmt.Printf("%-8s %-24s %s\n", fmt.Sprintf("%s/%d", r.Namespace, r.Version), r.Name, status)
		}
		return nil
	default:
		return fmt.Errorf("unknown migrate subcommand %q (want: up | status)", sub)
	}
}
