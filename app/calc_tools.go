package app

import (
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/calc"
	"github.com/cambrian-sh/core/internal/config"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// wireCalcTools serves the ADR-0145 D4 calculation tools (execution.tools.calc_tools,
// default off; T-20261108-adr-0145-d4-kernel-calculation-tools-working) as kernel
// tools on the ToolExecutor, beside ledger_get / ledger_find: one ExecuteTool path
// for every agent, needing no grant. Called after wireLedger, so
// with the run ledger on the tools read references through its ENFORCED reader and
// record every result as a computed entry through its classified writer; with the
// ledger off they still compute (dates, inline values) but cannot read references
// or record. The working-day calendar is the organisation's (organisation.weekend,
// organisation.holidays); "today" is the kernel's one calendar.
func wireCalcTools(cfg *config.Config, server *subnetwork.Server, calendar *domain.Calendar) {
	if !cfg.Execution.Tools.CalcTools || server == nil || server.ToolExecutor == nil {
		return
	}
	weekend, err := cfg.Organisation.WeekendDays()
	if err != nil {
		slog.Error("ADR-0145 D4: organisation.weekend invalid; working days use Saturday and Sunday", "err", err)
		weekend = nil
	}
	work, err := calc.NewWorkCalendar(weekend, cfg.Organisation.Holidays)
	if err != nil {
		// Load validates the holidays first, so this is a config that skipped
		// validation: loud, and the tools still serve without the holidays rather
		// than not at all.
		slog.Error("ADR-0145 D4: organisation.holidays invalid; working days ignore holidays", "err", err)
		work, _ = calc.NewWorkCalendar(weekend, nil)
	}
	deps := calc.Deps{Calendar: calendar, Work: work}
	if server.Ledger != nil {
		deps.Reader, deps.Writer = server.Ledger.Reader, server.Ledger.Writer
	} else {
		slog.Warn("ADR-0145 D4: calculation tools on without the run ledger: references are refused and results are not recorded")
	}
	server.ToolExecutor.KernelTools = append(server.ToolExecutor.KernelTools, calc.Tools(deps)...)
	slog.Info("ADR-0145 D4: calculation tools served", "tools", calc.Names, "holidays", len(cfg.Organisation.Holidays),
		"ledger", server.Ledger != nil)
}
