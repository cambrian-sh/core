package app

import (
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/subtasks"
)

// wireSubtasks threads execution.lane.subtasks to its consumers in one place
// (ADR-0150 D2/D5/D6; T-20261113-adr-0150-slice-2-run-subtasks-kernel-tool-brief),
// every key read here (AGENTS.md §3.4):
//
//   - the registry of subtask parents and child leases (domain.Subtasks) goes to
//     the ToolExecutor (effect ceiling checked by the kernel itself, deterministic
//     keys for a subtask's writes, labelled approvals, observed evidence), to its
//     per-lease tool ceiling, and to the server (child leases, Execute refusal);
//   - run_subtasks joins the kernel tools, offered only to a registered parent
//     (the single-agent lane's agent, Server.RegisterSubtaskParent) or a subtask
//     below max_depth.
//
// Called after wireLedger (the tool reads input refs through the enforced reader
// and records through the classified writer) and wireComposedWorkers (subtask
// workers run on the composed runtime). Off, in the planner lane, or without the
// composed runtime: nothing is wired and the tool is not on any menu.
//
// reg is the subtask registry the executor was CONSTRUCTED with (its lease
// ceiling and Subtasks control, buildExecutorLeases, AGENTS.md §2.3), over the
// ADR-0154 durable action record so a subtask resumed after a restart never
// repeats a write and a finished subtask is answered from its stored result
// (ENG-1760); nil when the config does not serve run_subtasks.
func wireSubtasks(cfg config.ExecutionConfig, server *subnetwork.Server, reg *domain.Subtasks) {
	sc := cfg.Lane.Subtasks
	if !sc.On() || server == nil || server.ToolExecutor == nil {
		return
	}
	if !cfg.Lane.IsSingleAgent() {
		slog.Info("ADR-0150: run_subtasks not offered (execution.lane.default is not single_agent; planner-lane workers never get it)")
		return
	}
	if server.Compositions == nil || server.ComposedRuntimeID == "" {
		slog.Error("ADR-0150: execution.lane.subtasks.enabled but subtask workers need the composed runtime (execution.workers.composed is off); run_subtasks is OFF")
		return
	}
	if reg == nil || server.ToolExecutor.Subtasks() != reg {
		slog.Error("ADR-0150: the tool executor was not constructed with this subtask registry; run_subtasks is OFF")
		return
	}
	te := server.ToolExecutor
	server.Subtasks = reg
	tool := &subtasks.Tool{
		Cfg: subtasks.Config{
			MaxParallel: sc.Parallel(), MaxPerCall: sc.PerCall(), MaxPerRequest: sc.PerRequest(),
			MaxDepth: sc.Depth(), AllowedEffects: sc.Effects(), SummaryMaxChars: sc.SummaryChars(),
			WorkerMaxRounds: sc.WorkerToolRounds(), WorkerMaxWall: sc.WorkerWall(),
			WorkerSummaryGrace: sc.WorkerSummaryGrace(),
		},
		Registry: reg,
		Runner:   server,
		Tools:    server.SubtaskParentTools,
	}
	if server.Ledger != nil {
		tool.Reader, tool.Writer = server.Ledger.Reader, server.Ledger.Writer
	} else {
		slog.Warn("ADR-0150: run_subtasks without the run ledger: input refs are refused and full answers are not recorded (result_ref empty)")
	}
	te.KernelTools = append(te.KernelTools, tool)
	slog.Info("ADR-0150: run_subtasks served", "max_parallel", sc.Parallel(), "max_per_call", sc.PerCall(),
		"max_per_request", sc.PerRequest(), "max_depth", sc.Depth(), "allowed_effects", sc.Effects(),
		"summary_max_chars", sc.SummaryChars(), "worker_max_tool_rounds", sc.WorkerToolRounds(),
		"worker_max_wall", sc.WorkerWall(), "worker_summary_grace", sc.WorkerSummaryGrace(), "runtime", server.ComposedRuntimeID)
}
