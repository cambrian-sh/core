package app

import (
	"log/slog"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/stagedinputs"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// wireStagedInputs gives the ToolExecutor its input stager
// (execution.tools.staged_inputs, default on; owner decision 2026-11-10, option C;
// decisions/code-sandbox-files-per-run-mount.md). It acts only on a tool whose MCP
// policy declares staged_inputs (the code sandbox's execute_code): each call's
// `inputs` are read under the caller's access — ledger ids through the run
// ledger's ENFORCED reader, paths through the configured file-reading tool via
// the executor — into a per-run folder the server mounts read-only for that call.
// Called after wireLedger: with the ledger off, ledger-id inputs are refused and
// output files are named in the result but not recorded.
func wireStagedInputs(cfg config.StagedInputsConfig, server *subnetwork.Server) {
	if server == nil || server.ToolExecutor == nil {
		return
	}
	if !cfg.Enabled {
		slog.Info("staged tool inputs off (execution.tools.staged_inputs.enabled=false): a staged tool called with inputs is refused")
		return
	}
	sc := stagedinputs.Config{
		Root: cfg.Root, Argument: cfg.Argument, MaxFiles: cfg.MaxFiles, MaxTotalBytes: cfg.MaxTotalBytes,
		MaxOutputFiles: cfg.MaxOutputFiles, MaxOutputBytes: cfg.MaxOutputBytes, BinaryExtensions: cfg.BinaryExtensions,
	}
	if r := cfg.PathReader; r != nil {
		sc.TextReader = stagedinputs.Reader{Tool: r.Tool, Argument: r.Argument}
	}
	if r := cfg.BinaryPathReader; r != nil {
		sc.BinaryReader = stagedinputs.Reader{Tool: r.Tool, Argument: r.Argument}
	}
	var st *stagedinputs.Stager
	if server.Ledger != nil {
		st = stagedinputs.New(sc, server.Ledger.Reader)
	} else {
		st = stagedinputs.New(sc, nil)
	}
	server.ToolExecutor.Stager = st
	slog.Info("staged tool inputs on", "root", st.Root(), "argument", st.Argument(), "ledger", server.Ledger != nil)
}
