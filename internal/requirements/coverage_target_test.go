package requirements

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261112-coverage-settles-a-write-item-on-any-done: a write item is satisfied
// only by a done write to the item's own target (the per-target effect record in
// the plan action ledger), never by another tool succeeding and never by the
// worker's claim. Since requirement_checklist v3 the target is the one the item
// DECLARES (T-20261112-write-target-coverage-reads-title-pdf-from-a); the item's
// text is never parsed for one.

const csvItemText = `Save the price table as a CSV file at C:\Users\demo\out\pricing.csv`

func exact(kind, value string) []domain.ItemTarget {
	return []domain.ItemTarget{{Kind: kind, Value: value, Match: domain.TargetMatchExact}}
}

func csvChecklist() domain.RequirementChecklist {
	return domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementWrite, Text: csvItemText, Quote: "save it as pricing.csv",
			Targets: exact(domain.TargetKindFile, `C:\Users\demo\out\pricing.csv`)}}}
}

// targets as domain.WriteTargets.Key records them (the default rules: path).
func fsAction(tool, path, status, detail string) domain.PlanAction {
	wt := &domain.WriteTargets{DefaultKeys: []string{"path"}}
	return domain.PlanAction{Step: 2, Tool: "mcp:filesystem/" + tool, Status: status, Detail: detail,
		Args: map[string]string{"path": path}, Target: wt.Key("mcp:filesystem/"+tool, []byte(`{"path":`+jsonStr(path)+`}`))}
}

func jsonStr(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }

func TestCoverage_WriteItemNotSettledByAnotherToolsSuccess(t *testing.T) {
	for _, claim := range []domain.RequirementStatus{domain.RequirementSatisfied, domain.RequirementFailed} {
		c := NewCoverage(csvChecklist())
		c.ObserveStep(StepObservation{Step: 2, Results: []domain.ItemResult{{Item: "R1", Status: claim}},
			Actions: []domain.PlanAction{
				fsAction("create_directory", `C:\Users\demo\out`, domain.PlanActionDone, ""),
				fsAction("write_file", `C:\Users\demo\out\pricing.csv`, domain.PlanActionFailed, "was replaced while it was being opened"),
				fsAction("create_directory", `C:\Users\demo\out`, domain.PlanActionDone, ""),
			}})
		s, why := status(c, "R1")
		if s == domain.RequirementSatisfied {
			t.Fatalf("claim %s: create_directory done + write_file failed settled the write: %q", claim, why)
		}
		if claim == domain.RequirementSatisfied && (!strings.Contains(why, "pricing.csv") || !strings.Contains(why, "write_file failed")) {
			t.Fatalf("claim %s: the reason must name the target and the failed write: %q", claim, why)
		}
	}
}

func TestCoverage_WriteItemSatisfiedByADoneWriteToItsTarget(t *testing.T) {
	c := NewCoverage(csvChecklist())
	c.ObserveStep(StepObservation{Step: 2, Results: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied}},
		Actions: []domain.PlanAction{
			fsAction("create_directory", `C:\Users\demo\out`, domain.PlanActionDone, ""),
			fsAction("write_file", `C:\Users\demo\out\pricing.csv`, domain.PlanActionFailed, "busy"),
			fsAction("write_file", `C:/Users/demo/out/pricing.csv`, domain.PlanActionDone, ""),
		}})
	if s, why := status(c, "R1"); s != domain.RequirementSatisfied || !strings.Contains(why, "write_file") {
		t.Fatalf("R1 = %s %q", s, why)
	}
}

func TestCoverage_WriteToADifferentFileDoesNotSettleTheItem(t *testing.T) {
	c := NewCoverage(csvChecklist())
	c.ObserveStep(StepObservation{Step: 2, Results: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied}},
		Actions: []domain.PlanAction{fsAction("write_file", `C:\Users\demo\out\prices_draft.csv`, domain.PlanActionDone, "")}})
	if s, why := status(c, "R1"); s == domain.RequirementSatisfied {
		t.Fatalf("a write to another file settled the item: %q", why)
	}
}

// A relative path in the call names the same file as the item's full path, and a
// bare file name in the item names the call's full path.
func TestCoverage_WriteTargetMatchesAcrossPathForms(t *testing.T) {
	c := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementWrite, Text: "Save the summary as summary.md in the reports folder",
			Targets: exact(domain.TargetKindFile, "summary.md")}}})
	c.ObserveStep(StepObservation{Step: 0, Results: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied}}, Actions: []domain.PlanAction{
		fsAction("write_file", `/srv/work/reports/summary.md`, domain.PlanActionDone, "")}})
	if s, why := status(c, "R1"); s != domain.RequirementSatisfied {
		t.Fatalf("R1 = %s %q", s, why)
	}
}

// An item that declares no target keeps the earlier rule: a done write-class call
// of the step satisfies it. Its text naming a file is not a target any more.
func TestCoverage_WriteItemWithNoDeclaredTargetTakesAnyDoneWrite(t *testing.T) {
	for _, text := range []string{"Record the outcome in the case system", "Rename each one in the form YYYY-MM-DD Title.pdf"} {
		c := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{
			{ID: "R1", Type: domain.RequirementWrite, Text: text}}})
		c.ObserveStep(StepObservation{Step: 0, Results: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied}}, Actions: []domain.PlanAction{
			{Step: 0, Tool: "cases_update", Status: domain.PlanActionDone, Target: "cases_update|case_id=c-77"}}})
		if s, why := status(c, "R1"); s != domain.RequirementSatisfied {
			t.Fatalf("%q: R1 = %s %q", text, s, why)
		}
	}
}

// An item declaring a record is held to it: the done update of another case does
// not settle it, the refused update of its own case is reported as not_permitted.
func TestCoverage_WriteItemDeclaringARecordIsHeldToIt(t *testing.T) {
	c := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementWrite, Text: "Close case CASE-1042 with the outcome",
			Targets: exact(domain.TargetKindRecord, "CASE-1042")}}})
	c.ObserveStep(StepObservation{Step: 0, Results: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied}},
		Actions: []domain.PlanAction{
			{Step: 0, Tool: "cases_update", Status: domain.PlanActionDone, Target: "cases_update|case_id=case-0999"},
			{Step: 0, Tool: "cases_update", Status: domain.PlanActionRefused, Target: "cases_update|case_id=case-1042", Detail: "approval denied"},
		}})
	if s, why := status(c, "R1"); s != domain.RequirementNotPermitted || !strings.Contains(why, "approval denied") {
		t.Fatalf("R1 = %s %q", s, why)
	}
}

// A folder named beside a file is where the file goes, not the file: a declared
// file target is not matched by its folder.
func TestTargetMatches_AFolderDoesNotNameTheFile(t *testing.T) {
	file := exact(domain.TargetKindFile, "pricing.csv")
	if valueMatches(`c:/out/2026`, file) {
		t.Fatal("the folder must not name the file target")
	}
	folder := exact(domain.TargetKindFolder, `Documents\Receipts`)
	if !valueMatches(`c:/users/demo/documents/receipts/2026-10-03 x.pdf`, folder) {
		t.Fatal("a file written inside the declared folder reaches it")
	}
	if valueMatches(`c:/users/demo/downloads/scan_0041.pdf`, folder) {
		t.Fatal("a file outside the declared folder does not reach it")
	}
}

// p05 (run 20261112T132056Z): "Rename each one to the date and title printed on
// the receipt, in the form "YYYY-MM-DD Title.pdf"". v3 declares the template as a
// glob; the four done move_file calls record their destinations (DefaultWriteTargets
// move rule), which match it, so the item settles and every member is covered.
const receiptsGlob = `C:\Users\demo\Documents\Receipts\????-??-?? *.pdf`

var moveTargets = &domain.WriteTargets{Rules: []domain.WriteTargetRule{
	{Tools: []string{"*move*", "*rename*", "*copy*"}, Keys: []string{"destination"}}}, DefaultKeys: []string{"path"}}

func moveAction(step int, src, dst, status string) domain.PlanAction {
	args := `{"source":` + jsonStr(src) + `,"destination":` + jsonStr(dst) + `}`
	return domain.PlanAction{Step: step, Tool: "mcp:filesystem/move_file", Status: status,
		Args: map[string]string{"source": src, "destination": dst}, Target: moveTargets.Key("mcp:filesystem/move_file", []byte(args))}
}

func renameChecklist() domain.RequirementChecklist {
	return domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R2", Type: domain.RequirementWrite, Text: "Rename each October 2026 PDF file to the format YYYY-MM-DD Title.pdf",
			Quote:   `in the form "YYYY-MM-DD Title.pdf"`,
			Targets: []domain.ItemTarget{{Kind: domain.TargetKindFile, Value: receiptsGlob, Match: domain.TargetMatchGlob}}}}}
}

var receiptSet = []string{"receipt(2).pdf", "scan_0041.pdf", "scan_0057.pdf", "download.pdf"}

func TestCoverage_RenameToTemplateSatisfiedByMoveDestinations(t *testing.T) {
	c := NewCoverage(renameChecklist())
	dst := map[string]string{
		"receipt(2).pdf": "2026-10-12 Contoso Cloud Subscription.pdf",
		"scan_0041.pdf":  "2026-10-03 Northwind Office Supplies Invoice.pdf",
		"scan_0057.pdf":  "2026-10-21 Fabrikam Travel Receipt.pdf",
		"download.pdf":   "2026-10-28 Adatum Software Licence.pdf",
	}
	var acts []domain.PlanAction
	for _, m := range receiptSet {
		acts = append(acts, moveAction(3, `C:\Users\demo\Downloads\`+m, `C:\Users\demo\Documents\Receipts\`+dst[m], domain.PlanActionDone))
	}
	c.ObserveStep(StepObservation{Step: 3, SetMembers: receiptSet, Actions: acts,
		Results: []domain.ItemResult{{Item: "R2", Status: domain.RequirementSatisfied}}})
	snap := c.Snapshot().Items[0]
	if snap.Status != domain.RequirementSatisfied || strings.Contains(snap.Reason, "title.pdf") {
		t.Fatalf("R2 = %s %q", snap.Status, snap.Reason)
	}
	if len(snap.Members) != 4 || len(snap.missing()) != 0 {
		t.Fatalf("every member is covered by its move: members %v covered %v", snap.Members, snap.Covered)
	}
}

func TestCoverage_GlobTargetNotSatisfiedByMoveElsewhere(t *testing.T) {
	for name, dst := range map[string]string{
		"wrong folder":   `C:\Users\demo\Documents\Archive\2026-10-03 Northwind Office Supplies Invoice.pdf`,
		"name not dated": `C:\Users\demo\Documents\Receipts\Northwind Office Supplies Invoice.pdf`,
	} {
		c := NewCoverage(renameChecklist())
		c.ObserveStep(StepObservation{Step: 3, Actions: []domain.PlanAction{moveAction(3, `C:\Users\demo\Downloads\scan_0041.pdf`, dst, domain.PlanActionDone)},
			Results: []domain.ItemResult{{Item: "R2", Status: domain.RequirementSatisfied}}})
		s, why := status(c, "R2")
		if s == domain.RequirementSatisfied {
			t.Fatalf("%s: a move outside the declared pattern settled the item: %q", name, why)
		}
		// The reason names the declared pattern and what the call wrote instead.
		if !strings.Contains(why, "????-??-?? *.pdf (pattern)") || !strings.Contains(why, "wrote ") {
			t.Fatalf("%s: reason %q", name, why)
		}
	}
}

// A relative pattern matches the trailing segments of an absolute destination,
// with Windows separators normalised on both sides.
func TestTargetMatches_RelativeGlobMatchesTheEndOfAPath(t *testing.T) {
	g := []domain.ItemTarget{{Kind: domain.TargetKindFile, Value: `Receipts\????-??-?? *.pdf`, Match: domain.TargetMatchGlob}}
	if !valueMatches(`c:\users\demo\documents\receipts\2026-10-03 northwind.pdf`, g) {
		t.Fatal("relative glob must match the end of the destination")
	}
	if valueMatches(`c:/users/demo/documents/receipts/sub/2026-10-03 northwind.pdf`, g) {
		t.Fatal("* never crosses a folder")
	}
}
