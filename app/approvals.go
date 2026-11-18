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
	"github.com/cambrian-sh/core/internal/scheduledtask"
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
	// The ADR-0154 durable action record makes the recovered execution of an
	// approval raised by an earlier kernel run happen once
	// (T-20261118-a-sync-approval-orphaned-by-a-kernel-restart). Without
	// migration 0046 the hub keeps it in process (buildActionRecordStore logs
	// the missing table when the tool stack is built).
	var actions domain.ActionRecordStore
	if pool != nil {
		if pg := postgres.NewPgActionRecordStore(pool); pg.Ready(ctx) {
			actions = pg
		}
	}
	return domain.NewAddressedHub(domain.AddressedHubConfig{
		Store:          store,
		ActionRecords:  actions,
		Policy:         policy,
		DefaultTimeout: cfg.Approvals.DefaultTimeout(),
		ConsentTimeout: time.Duration(cfg.MCP.WorkerConsentTimeoutMs) * time.Millisecond, // 0 ⇒ 120 s
		SweepInterval:  cfg.Approvals.SweepInterval(),
	}), nil
}

// buildActionRecordStore returns the ADR-0154 durable action record over the
// shared pool, or nil when there is no database or migration 0046 is missing
// (logged): idempotent writes then stay in memory, as before (ADR-0154 D6).
func buildActionRecordStore(ctx context.Context, pool *pgxpool.Pool) domain.ActionRecordStore {
	if pool == nil {
		return nil
	}
	pg := postgres.NewPgActionRecordStore(pool)
	if !pg.Ready(ctx) {
		slog.Warn("ADR-0154: action_records table missing (migration 0046 not applied); side-effecting call records stay in memory and do not survive a restart")
		return nil
	}
	return pg
}

// wireApprovalRecovery registers the recoverers that carry out an approval
// approved after the kernel run that raised it ended
// (T-20261118-a-sync-approval-orphaned-by-a-kernel-restart; owner decision
// 2026-11-18, option A): dangerous tool calls through the executor (every
// check runs again) and schedule proposals through the schedule gate (nil when
// schedules are off: such an approve is then refused). directory confirms the
// person is still a known, active principal, as a scheduled firing does.
func wireApprovalRecovery(hub *domain.AddressedHub, exec *domain.ToolExecutor, schedules *scheduledtask.Service,
	directory func() domain.PrincipalDirectory) {
	if hub == nil {
		return
	}
	if exec != nil {
		hub.RecoverWith(domain.ApprovalIntentTool, exec.ApprovalRecoverer(directory))
	}
	if schedules != nil {
		hub.RecoverWith(domain.ApprovalIntentSchedule, schedules.ApprovalRecoverer(directory))
	}
}
