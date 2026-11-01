package network

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// Response metadata keys carrying what one Execute spent on LLM calls
// (ADR-0131 gate criterion 1: tokens_in, tokens_out, cache_read and $ per
// Execute, from provider usage). The figures are the session's spend between
// the start and the end of the Execute, so they include the planner, every
// step's agent stream, thought steps, checkpoints, replans, tier-2 scoring and
// the agentic retrieval chain of this plan. Router classification runs before
// the session is resolved and is booked unattributed (see ADR-0131
// observations). _llm_estimated_calls > 0 means part of the figure is the
// len/4 fallback, not provider usage.
const (
	MetaLLMCalls           = "_llm_calls"
	MetaLLMInputTokens     = "_llm_input_tokens"
	MetaLLMOutputTokens    = "_llm_output_tokens"
	MetaLLMCacheReadTokens = "_llm_cache_read_tokens"
	MetaLLMCostUSD         = "_llm_cost_usd"
	MetaLLMEstimatedCalls  = "_llm_estimated_calls"
)

// stampSpend writes this Execute's spend (session total now minus base) onto
// the response metadata. No-op without a meter or a session.
func (s *Server) stampSpend(meta map[string]string, sessionID domain.SessionID, base llm.SessionSpend) {
	if s.SpendMeter == nil || sessionID == "" || meta == nil {
		return
	}
	d := s.SpendMeter.Session(sessionID).Sub(base)
	meta[MetaLLMCalls] = strconv.Itoa(d.Calls)
	meta[MetaLLMInputTokens] = strconv.Itoa(d.InputTokens)
	meta[MetaLLMOutputTokens] = strconv.Itoa(d.OutputTokens)
	meta[MetaLLMCacheReadTokens] = strconv.Itoa(d.CacheReadTokens)
	meta[MetaLLMCostUSD] = strconv.FormatFloat(d.CostUSD, 'f', 6, 64)
	meta[MetaLLMEstimatedCalls] = strconv.Itoa(d.EstimatedCalls)
}

// spendCeilingFailure is the error Execute returns when a plan ends at the
// session spend ceiling. It unwraps to the meter's *llm.SpendCeilingError (so
// errors.Is(err, llm.ErrSpendCeiling) holds for in-process callers) and maps to
// gRPC ResourceExhausted for remote ones — a quota, not an internal fault.
type spendCeilingFailure struct {
	cause error
}

func (e *spendCeilingFailure) Error() string {
	return "plan ended at the session LLM spend ceiling: " + e.cause.Error()
}

func (e *spendCeilingFailure) Unwrap() error { return e.cause }

// GRPCStatus lets grpc-go map the failure to ResourceExhausted on the wire.
func (e *spendCeilingFailure) GRPCStatus() *status.Status {
	return status.New(codes.ResourceExhausted, e.Error())
}

// endPlanAtSpendCeiling records that a plan ended at the ADR-0131 D3 ceiling and
// returns the visible failure.
//
// The record is a TaskEvent — the kernel's durable per-execution record, which
// the token recorder also folds into the hourly series — carrying the session's
// real token totals and BudgetOverrun=true. Its TaskID is derived from the run
// (AGENTS.md §2.6), so a retry of the same record is idempotent. The plan-end
// receipt and the operator-feed LLMSpendEvent are ADR-0131 D6, deferred to the
// benchmark phase; this record is what they will read.
func (s *Server) endPlanAtSpendCeiling(sessionID domain.SessionID, runID domain.RunID, base llm.SessionSpend, cause error) error {
	if cause == nil {
		cause = llm.ErrSpendCeiling
	}
	spent := s.SpendMeter.Session(sessionID)
	planSpend := spent.Sub(base)
	slog.Warn("plan ended at the session LLM spend ceiling",
		"session", sessionID, "run", runID, "ceiling", s.SpendMeter.Ceiling(),
		"session_tokens", spent.TotalTokens(), "plan_tokens", planSpend.TotalTokens(),
		"plan_cost_usd", planSpend.CostUSD, "adr", "ADR-0131 D3")

	if w := s.taskEventWriter(); w != nil {
		taskID := "spend-ceiling:" + string(sessionID)
		if runID != "" {
			taskID = "spend-ceiling:" + string(runID)
		}
		// AgentID follows the "System_Thought" convention for kernel-authored
		// rows, so no agent's merit is charged for the session's spend.
		ev := domain.TaskEvent{
			TaskID:           taskID,
			AgentID:          spendCeilingAgentID,
			PromptTokens:     planSpend.InputTokens,
			CompletionTokens: planSpend.OutputTokens,
			TotalTokens:      planSpend.TotalTokens(),
			EstimatedCost:    planSpend.CostUSD,
			BudgetOverrun:    true,
			Timestamp:        time.Now(),
		}
		if err := w.WriteTaskEvent(ev); err != nil {
			slog.Warn("could not record the spend-ceiling TaskEvent", "session", sessionID, "err", err)
		}
	}
	return &spendCeilingFailure{cause: fmt.Errorf("session %s: %w", sessionID, cause)}
}

// spendCeilingAgentID is the AgentID of the spend-ceiling TaskEvent.
const spendCeilingAgentID = "System_SpendCeiling"
