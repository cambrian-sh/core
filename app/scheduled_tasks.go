package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/cambrian-sh/core/domain"
	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/scheduledtask"
	"github.com/cambrian-sh/core/internal/substrate/network"
)

// Composition-root wiring for act-for conversations and principal-owned recurring
// tasks (T-20261106-conversations-cannot-be-opened-as-a-given,
// T-20261106-the-assistant-cannot-set-up-a-recurring-task).
//
// Plugins are Built before the chat lane and the server exist, so the two premium
// contributions (a principal directory, a schedule store) and the one kernel
// service a plugin calls back (RunOwnedTurn) go through late-bound holders — the
// same shape as the ADR-0098 progress holders.

// principalSchedulesHolder holds the premium schedule store (one registration).
type principalSchedulesHolder struct {
	v atomic.Pointer[domain.PrincipalSchedules]
}

func (h *principalSchedulesHolder) set(p domain.PrincipalSchedules) {
	if p != nil {
		h.v.Store(&p)
	}
}

func (h *principalSchedulesHolder) get() domain.PrincipalSchedules {
	if p := h.v.Load(); p != nil {
		return *p
	}
	return nil
}

// principalDirectoryHolder holds the premium principal directory.
type principalDirectoryHolder struct {
	v atomic.Pointer[domain.PrincipalDirectory]
}

func (h *principalDirectoryHolder) set(d domain.PrincipalDirectory) {
	if d != nil {
		h.v.Store(&d)
	}
}

func (h *principalDirectoryHolder) get() domain.PrincipalDirectory {
	if d := h.v.Load(); d != nil {
		return *d
	}
	return nil
}

// errNoChatLane is RunOwnedTurn's answer on a kernel with the chat lane off.
var errNoChatLane = errors.New("the chat lane is not running (execution.chat.chat_pool_size = 0 or no conversation store)")

// ownedTurnRunner runs one chat turn FOR a principal on the kernel's own behalf
// (a scheduled task firing). It is KernelServices.RunOwnedTurn.
//
// The conversation is created owned by the principal when absent and refused when
// it exists under anyone else, so a task can never write into — or read the recall
// of — a conversation its owner does not hold. Everything the turn does then keys
// on that owner by the ordinary rule (ADR-0139 D14: the conversation owner is the
// run principal), which is what makes the run happen AS the principal.
type ownedTurnRunner struct {
	mu    sync.RWMutex
	store domain.ConversationStore
	turns *corechat.TurnService
}

func (r *ownedTurnRunner) bind(store domain.ConversationStore, turns *corechat.TurnService) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store, r.turns = store, turns
}

func (r *ownedTurnRunner) run(ctx context.Context, t domain.OwnedTurn) (domain.Message, error) {
	r.mu.RLock()
	store, turns := r.store, r.turns
	r.mu.RUnlock()
	if store == nil || turns == nil {
		return domain.Message{}, errNoChatLane
	}
	if t.OwnerID == "" || t.ConversationID == "" {
		return domain.Message{}, errors.New("owned turn: owner and conversation are required")
	}
	ops := &conversationOps{store: store, turns: turns}
	if _, err := ops.Open(ctx, t.ConversationID, t.OwnerID, t.Title,
		string(domain.ProfileEmployee), t.Policy); err != nil {
		return domain.Message{}, err
	}
	// The owner is the domain principal of the call as well, so nothing on this
	// path can attribute the turn to the kernel or to whoever registered it.
	ctx = domain.WithPrincipal(ctx, domain.UserPrincipal(t.OwnerID))
	ctx = domain.WithRunPrincipal(ctx, t.OwnerID)
	return ops.SendTurn(ctx, t.ConversationID, t.Text, t.ClientID)
}

// conversationOwnerOf resolves a lease binding to the owner of the conversation
// it was issued under — on the lease, or on its task session — and nothing else:
// no beneficiary, no default principal (network.Server.ConversationOwner).
func conversationOwnerOf(conversations domain.ConversationStore, sessions notesSessionReader) network.MemoryNotesPrincipalFunc {
	return func(ctx context.Context, b domain.LeaseBinding) string {
		if conversations == nil {
			return ""
		}
		convID := b.ConversationID
		if convID == "" && b.SessionID != "" && sessions != nil {
			if ses, err := sessions.GetSession(ctx, b.SessionID); err == nil && ses != nil {
				convID = ses.ConversationID
			}
		}
		if convID == "" {
			return ""
		}
		if c, err := conversations.GetConversation(ctx, convID); err == nil && c != nil {
			return c.OwnerID
		}
		return ""
	}
}

// buildScheduledTasks builds the agent-plane gate, or nil (RPCs Unimplemented,
// capability withheld) when disabled, when no premium store registered, or when
// there is no approval channel — a schedule is never created without one.
func buildScheduledTasks(cfg config.SchedulesConfig, store domain.PrincipalSchedules, approvals domain.ApprovalController) *scheduledtask.Service {
	if cfg.Disabled || store == nil {
		return nil
	}
	svc, err := scheduledtask.New(store, approvals, domain.ScheduleLimits{
		MaxPerPrincipal: cfg.MaxPerPrincipalOrDefault(),
		MinInterval:     cfg.MinInterval(),
	}, cfg.ApprovalTimeout())
	if err != nil {
		slog.Warn("scheduled tasks: not enabled", "err", err)
		return nil
	}
	slog.Info("scheduled tasks: enabled (assistant proposals, approval-gated)",
		"max_per_principal", cfg.MaxPerPrincipalOrDefault(), "min_interval", cfg.MinInterval(),
		"approval_timeout", cfg.ApprovalTimeout())
	return svc
}
