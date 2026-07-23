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
