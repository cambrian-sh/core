package app

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/contentscan"
	"github.com/cambrian-sh/core/internal/memorynotes"
	"github.com/cambrian-sh/core/internal/substrate/network"
)

// ADR-0139 D14: the principal memory-notes service, composed once here and handed to
// both planes (network.Server for GetMemoryNotes/ProposeMemoryNote, the operator
// service for List/Get/Set/Delete/SetMemoryNotesBudget). One service, so the budget,
// the injection scan and the C8 check cannot be skipped by either plane.

// notesScanner binds the shared deterministic content scanner (internal/contentscan,
// also D13's skill scanner) to the notes port. Notes have no approval queue — they
// reach the next run's prompt directly — so ANY finding (suspicious or dangerous)
// refuses an agent write; an operator may store past it with
// override_injection_flag, which is audited.
var notesScanner = memorynotes.ScannerFunc(func(text string) (bool, string) {
	r := contentscan.Scan(text)
	if r.Verdict == contentscan.VerdictClean {
		return false, ""
	}
	return true, string(r.Verdict) + ": " + strings.Join(r.FindingStrings(), "; ")
})

// buildMemoryNotes returns the notes service, or nil when execution.memory_notes is
// disabled. Postgres (migration 0030) when the schema is there; otherwise an
// in-memory store, logged, so an unmigrated database degrades to notes that last
// for the process rather than a kernel that will not boot.
func buildMemoryNotes(ctx context.Context, cfg config.MemoryNotesConfig, pool *pgxpool.Pool, authorizer domain.Authorizer) *memorynotes.Service {
	if cfg.Disabled {
		slog.Info("ADR-0139 D14: memory notes disabled (execution.memory_notes.disabled)")
		return nil
	}
	var store memorynotes.Store
	if pool != nil {
		pg, err := memorynotes.NewPgStore(ctx, pool)
		if err == nil {
			store = pg
		} else {
			slog.Warn("ADR-0139 D14: memory notes Postgres store unavailable; notes will NOT survive a restart", "err", err)
		}
	}
	if store == nil {
		store = memorynotes.NewMemStore()
	}
	svc, err := memorynotes.New(store, notesScanner, authorizer, memorynotes.Config{
		PersonalBudgetChars: cfg.PersonalBudget(),
		OrgBudgetChars:      cfg.OrgBudget(),
		OrgID:               cfg.OrgID,
		AgentOrgWrite:       cfg.AgentOrgWrite,
		SnapshotTTL:         time.Duration(cfg.SnapshotTTLSeconds) * time.Second,
		MaxSnapshots:        cfg.MaxSnapshots,
	})
	if err != nil {
		// Unreachable with the defaults above; logged rather than fatal because notes
		// are additive context, not a dependency of any other subsystem.
		slog.Error("ADR-0139 D14: memory notes service not built", "err", err)
		return nil
	}
	return svc
}

// notesSessionReader is the one session read the principal resolver needs.
type notesSessionReader interface {
	GetSession(ctx context.Context, id domain.SessionID) (*domain.Session, error)
}

// memoryNotesPrincipal resolves a lease binding to the run principal (the "sub"),
// from kernel-held facts only (INV-5): the owner of the conversation that ordered
// the work (on the lease, or on its task session), else the task's beneficiary
// (ADR-0126 MCP task lane), else the configured default principal (single-principal
// personal profile). "" = none: the run then gets no personal notes.
func memoryNotesPrincipal(defaultPrincipal string, conversations domain.ConversationStore,
	sessions notesSessionReader, owners network.TaskBeneficiarySource) network.MemoryNotesPrincipalFunc {
	return func(ctx context.Context, b domain.LeaseBinding) string {
		convID := b.ConversationID
		if convID == "" && b.SessionID != "" && sessions != nil {
			if ses, err := sessions.GetSession(ctx, b.SessionID); err == nil && ses != nil {
				convID = ses.ConversationID
			}
		}
		if convID != "" && conversations != nil {
			if c, err := conversations.GetConversation(ctx, convID); err == nil && c != nil && c.OwnerID != "" {
				return c.OwnerID
			}
		}
		if b.SessionID != "" && owners != nil {
			if p := owners.BeneficiaryOf(b.SessionID); !p.IsZero() {
				return p.ID
			}
		}
		return defaultPrincipal
	}
}
