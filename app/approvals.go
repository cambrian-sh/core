package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// buildApprovalHub assembles THE approval hub (ADR-0142 D11): the config
// policy (the `approvals` block over the shipped defaults), durable storage
// (the 0033 tables when the database has them, else in-process with a loud
// warning), and the consent window from mcp.worker_consent_timeout_ms.
//
// It is the only construction of the hub; every approval kind — dangerous
// tools, skill access, schedules, proactive plans, machine consent and choice,
// account confirmation, publish — rides the instance it returns.
func buildApprovalHub(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool) (*domain.AddressedHub, error) {
	policy, err := domain.NewConfigApprovalPolicy(cfg.Approvals.PolicyConfig())
	if err != nil {
		return nil, fmt.Errorf("approvals: %w", err)
	}
	var store domain.ApprovalStore
	if pool != nil {
		pg := postgres.NewPgApprovalStore(pool)
		if pg.Ready(ctx) {
			store = pg
		}
	}
	if store == nil {
		// Not silent (AGENTS.md §3.2): approvals still work, but a restart loses
		// pending ones — which ADR-0142 D11 says must not happen in production.
		slog.Warn("ADR-0142: approval tables missing (migration 0033 not applied); approvals are in-process only and will not survive a restart")
		store = domain.NewMemoryApprovalStore()
	}
	return domain.NewAddressedHub(domain.AddressedHubConfig{
		Store:          store,
		Policy:         policy,
		DefaultTimeout: cfg.Approvals.DefaultTimeout(),
		ConsentTimeout: time.Duration(cfg.MCP.WorkerConsentTimeoutMs) * time.Millisecond, // 0 ⇒ 120 s
		SweepInterval:  cfg.Approvals.SweepInterval(),
	}), nil
}
