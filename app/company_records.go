package app

import (
	"log/slog"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/records"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// wireCompanyRecords serves company_records (internal/records; ENG-2117, owner
// decision 2026-11-18) as a kernel tool on the ToolExecutor, beside ledger_get and
// the calc tools: the read-only look-up of the company's typed records
// (observations, events, declared objects) over the ONE fenced query plane and
// the objects reader the substrate composes (NewSubstrate). It reads under the
// predicate runKernelTool resolves for the caller, so it needs no grant and
// enforces nothing of its own. execution.tools.company_records.enabled (default
// true) turns it off.
func wireCompanyRecords(cfg config.CompanyRecordsConfig, server *subnetwork.Server, substrate *Substrate) {
	if !cfg.Enabled || server == nil || server.ToolExecutor == nil {
		return
	}
	if substrate == nil || substrate.Query == nil {
		slog.Warn("ENG-2117: company_records not served: the knowledge substrate has no query plane")
		return
	}
	deps := records.Deps{Plane: substrate.Query, MaxRows: cfg.MaxRows, MaxEvents: cfg.MaxEvents, MaxAttributes: cfg.MaxAttributes}
	if substrate.Objects != nil {
		deps.Objects = substrate.Objects
	}
	tool, err := records.New(deps)
	if err != nil {
		slog.Error("ENG-2117: company_records not served", "err", err)
		return
	}
	server.ToolExecutor.KernelTools = append(server.ToolExecutor.KernelTools, tool)
	slog.Info("ENG-2117: company_records served", "max_rows", cfg.MaxRows, "max_events", cfg.MaxEvents,
		"objects", deps.Objects != nil)
}
