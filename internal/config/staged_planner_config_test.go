package config

import (
	"strings"
	"testing"
	"time"
)

// ADR-0145 D11 / D5a / D4 config (T-20261108-adr-0145-d11-staged-planner-returns-next-stage,
// T-20261108-adr-0145-d4-kernel-calculation-tools-working): defaults are the very
// high runaway guards of the no-budget decision, the staged planner needs the
// checklist, ask-back needs the staged planner, and the holiday calendar is
// validated at load.
func TestStagedPlannerAndCalcConfig(t *testing.T) {
	var p PlanConfig
	if p.StageReadRounds() != 25 || p.StageIndexEntries() != 200 || p.AskBackRounds() != 5 || p.AskBackReportsOnOtherPaths() {
		t.Fatalf("defaults: reads %d index %d rounds %d report %v", p.StageReadRounds(), p.StageIndexEntries(), p.AskBackRounds(), p.AskBackReportsOnOtherPaths())
	}
	p.AskBackOtherPaths = "Report"
	if !p.AskBackReportsOnOtherPaths() {
		t.Fatal("report is case-insensitive")
	}

	c := ExecutionConfig{}
	c.Plan.StagedPlanner = true
	c.Plan.AskBackEnabled = true
	c.Plan.AskBackOtherPaths = "ask"
	err := c.Validate()
	if err == nil {
		t.Fatal("staged without checklist must be refused")
	}
	for _, want := range []string{"staged_planner requires execution.plan.checklist_enabled", "ask_back_other_paths"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation lacks %q: %v", want, err)
		}
	}
	c2 := ExecutionConfig{}
	c2.Plan.AskBackEnabled = true
	if err := c2.Validate(); err == nil || !strings.Contains(err.Error(), "ask_back_enabled requires execution.plan.staged_planner") {
		t.Fatalf("ask-back without the staged planner: %v", err)
	}

	o := OrganisationConfig{Holidays: []string{"2026-10-29", "01-01", "29.10.2026"}, Weekend: []string{"Friday", "saturday"}}
	errs := o.validationErrors()
	if len(errs) != 1 || !strings.Contains(errs[0], "29.10.2026") {
		t.Fatalf("holiday validation = %v", errs)
	}
	days, werr := o.WeekendDays()
	if werr != nil || len(days) != 2 || days[0] != time.Friday {
		t.Fatalf("weekend = %v %v", days, werr)
	}
	if _, werr := (OrganisationConfig{Weekend: []string{"funday"}}).WeekendDays(); werr == nil {
		t.Fatal("an unknown weekday must be refused")
	}
	if d, _ := (OrganisationConfig{}).WeekendDays(); len(d) != 2 || d[0] != time.Saturday {
		t.Fatalf("default weekend = %v", d)
	}
}
