package executer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// PlanStepsKey is the master-context key under which ExecuteFrom leaves the step
// index of the FINAL plan (after replans and fan-out expansion) as JSON
// []domain.ExecuteStepRef, for Server.Execute's structured result (ADR-0145 D8).
// Like finalResultKey it is an internal convention: the server consumes and
// deletes it before the context leaves the kernel.
const PlanStepsKey = "_dag_plan_steps"

// stepRefPreviewChars bounds a step's preview in the structured result.
const stepRefPreviewChars = 300

// planStepRefs lists every step of plan with its outcome as recorded in master:
// completed (with its ledger reference when the ledger recorded it), empty, or not
// run. An expanded fan-out node is listed too; its children carry its results.
func planStepRefs(plan *domain.ExecutionPlan, master map[string]string) []domain.ExecuteStepRef {
	if plan == nil {
		return nil
	}
	out := make([]domain.ExecuteStepRef, 0, len(plan.Steps))
	for i, st := range plan.Steps {
		ref := domain.ExecuteStepRef{Step: i, Query: st.Query, FanOutItem: st.FanOutItem, Thought: st.IsThought}
		res, ok := master[fmt.Sprintf("step_%d_result", i)]
		switch {
		case !ok:
			ref.Status = domain.ExecuteStepNotRun
		case strings.TrimSpace(res) == "":
			ref.Status = domain.ExecuteStepEmpty
		default:
			ref.Status = domain.ExecuteStepCompleted
			ref.Bytes = len(res)
			ref.Preview = domain.LedgerPreview([]byte(res), stepRefPreviewChars)
		}
		if cid := master[fmt.Sprintf("step_%d_cid", i)]; domain.IsLedgerID(cid) && ok {
			ref.Ref = cid
		}
		out = append(out, ref)
	}
	return out
}

// PlanStepRefsFrom decodes the PlanStepsKey value; nil when absent or malformed.
func PlanStepRefsFrom(master map[string]string) []domain.ExecuteStepRef {
	raw := master[PlanStepsKey]
	if raw == "" {
		return nil
	}
	var refs []domain.ExecuteStepRef
	if json.Unmarshal([]byte(raw), &refs) != nil {
		return nil
	}
	return refs
}

func encodeStepRefs(refs []domain.ExecuteStepRef) string {
	b, err := json.Marshal(refs)
	if err != nil {
		return ""
	}
	return string(b)
}
