package app

import (
	"context"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/config"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// wireChatGrounding threads the ADR-0145 phase-3 chat keys (execution.chat:
// transcript_budget_tokens, turn_index, turn_index_turns, grounded_reply,
// reply_check, reply_check_min_integer, reply_check_max_entries,
// delegation_rule, ledger_tools, ask_back) to the chat lane in one place, each
// read here and nowhere else (AGENTS.md §3.4). Every key defaults off; the turn
// path is then unchanged.
//
//   - the run ledger's writer and ENFORCED reader (turn index, summaries, the
//     reply check's evidence), when execution.ledger is on;
//   - the kernel-side source of the turn's delegated run results
//     (TurnDelegations.TurnResults: reply check, turn index, ask-back);
//   - the rolling-summary organ call over gen (the planner organ's generator);
//   - the run tracer, so the reply check's events land beside the turn's spans.
//
// The chat agent reads its own keys from the turn handoff (grounded_reply,
// delegation_rule, ledger_tools, reply_check, turn_index, conversation_summary,
// pending_input); nothing else is configured agent-side.
func wireChatGrounding(cfg config.ChatConfig, turns *corechat.TurnService, server *subnetwork.Server,
	delegations *subnetwork.TurnDelegations, gen domain.Generator, tracer domain.RunTracer) {
	if turns == nil {
		return
	}
	turns.Grounding = corechat.Grounding{
		TranscriptBudgetTokens: cfg.TranscriptBudgetTokens,
		TurnIndex:              cfg.TurnIndex,
		TurnIndexTurns:         cfg.TurnIndexWindow(),
		GroundedReply:          cfg.GroundedReply,
		ReplyCheck:             cfg.ReplyCheckMode(),
		ReplyCheckMinInteger:   cfg.ReplyCheckMinInt(),
		ReplyCheckMaxEntries:   cfg.ReplyCheckEntries(),
		DelegationRule:         cfg.DelegationRule,
		LedgerTools:            cfg.LedgerTools,
		AskBack:                cfg.AskBack,
	}
	if server != nil && server.Ledger != nil {
		turns.SetLedger(server.Ledger.Writer, server.Ledger.Reader)
	} else if cfg.NeedsLedger() || cfg.TranscriptBudgetTokens > 0 {
		slog.Warn("ADR-0145 chat: execution.ledger is off; the turn index, rolling summary, reply check, ledger tools and ask-back state are OFF")
	}
	if delegations != nil {
		turns.SetTurnResults(delegations.TurnResults)
	}
	if cfg.TranscriptBudgetTokens > 0 {
		if gen == nil {
			slog.Warn("ADR-0145 D2: transcript_budget_tokens is set but there is no organ generator; older messages leave the transcript without a summary")
		} else {
			turns.SetSummarizer(corechat.NewSummarizer(gen))
		}
	}
	if tracer != nil {
		turns.SetRunTracer(tracer)
	}
	slog.Info("ADR-0145 phase 3 (chat)", "transcript_budget_tokens", cfg.TranscriptBudgetTokens,
		"turn_index", cfg.TurnIndex, "grounded_reply", cfg.GroundedReply, "reply_check", cfg.ReplyCheckMode(),
		"delegation_rule", cfg.DelegationRule, "ledger_tools", cfg.LedgerTools, "ask_back", cfg.AskBack)
}

// wireChatLane threads execution.lane to the chat path (ADR-0150 D3): in the
// single-agent lane (the default) the agent the person talks to is the
// default-lane agent — the turn carries the lane's D4 caps and stuck thresholds,
// the turn's lease is bound to the person's permitted tools (the request
// composition, when composed workers are on; read at turn time, so the order of
// wiring does not matter), the person's own messages survive cross-turn
// compaction up to execution.chat.keep_user_messages_tokens, and
// delegate_to_planner is not offered (the kernel refuses it in Execute). The
// planner lane leaves the chat front desk unchanged.
func wireChatLane(cfg config.ExecutionConfig, turns *corechat.TurnService, server *subnetwork.Server) {
	if turns == nil {
		return
	}
	if !cfg.Lane.IsSingleAgent() {
		slog.Info("ADR-0150: chat runs the planner lane's front desk", "lane", cfg.Lane.Mode())
		return
	}
	turns.Lane = corechat.LaneTurn{
		SingleAgent:            true,
		KeepUserMessagesTokens: cfg.Chat.KeepUserMessagesTokens,
	}
	if server != nil {
		turns.Lane.Limits = server.LaneLimits()
		chatAgent := cfg.Chat.ChatPoolAgentID
		if chatAgent == "" {
			chatAgent = "chat_agent"
		}
		turns.SetTurnComposer(func(ctx context.Context, lease domain.LeaseID, brief string) error {
			// ADR-0150 D2/D3: the merged agent is the lane's depth-0 agent, so
			// run_subtasks is offered on its turn lease (a no-op without subtasks).
			server.RegisterSubtaskParent(lease, chatAgent)
			if !cfg.Workers.Composed {
				// No composed runtime: the agent's own grants are its tools.
				return nil
			}
			return server.ComposeLease(ctx, lease, brief)
		})
	}
	slog.Info("ADR-0150 D3: chat runs the single-agent lane (merged conversation agent)",
		"max_tool_rounds", turns.Lane.Limits.MaxToolRounds, "max_wall_seconds", turns.Lane.Limits.MaxWallSeconds,
		"keep_user_messages_tokens", cfg.Chat.KeepUserMessagesTokens,
		"composed_tools", cfg.Workers.Composed)
}
