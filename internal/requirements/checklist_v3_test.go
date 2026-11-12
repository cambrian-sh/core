package requirements

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// requirement_checklist v3 (owner decision 2026-11-12 "explicit targets +
// expects"; T-20261112-write-target-coverage-reads-title-pdf-from-a,
// T-20261112-checklist-types-the-source-page-for-each).

// p05Request is the recorded p05 request (trace cambrian-kernel-20261112T132754Z-1300,
// run 20261112T_personal_adr0145_fixes), with the sandbox path shortened.
const p05Request = `In my Downloads folder (C:/Users/demo/home/Downloads) find the PDF files from last month (October 2026). ` +
	`Rename each one to the date and title printed on the receipt, in the form "YYYY-MM-DD Title.pdf" ` +
	`(for example "2026-01-15 Acme Widget Invoice.pdf"), and move them into C:/Users/demo/home/Documents/Receipts ` +
	`(create that folder). Do not touch any other file.`

func TestDerive_V3TeachesTargetsAndExpects(t *testing.T) {
	if checklistPrompts.System.Version < 3 {
		t.Fatalf("the deriver runs requirement_checklist v%d; v3 declares targets and expects", checklistPrompts.System.Version)
	}
	for _, rule := range []string{"match \"exact\"", "match \"glob\"", "Placeholders\n  and examples are not real names",
		"where the result ends up", "expects lists", "[\"any\"]", "a link, web page\n  or source document is a citation"} {
		if !strings.Contains(checklistPrompts.System.Body, rule) {
			t.Fatalf("v3 system block lacks %q", rule)
		}
	}
	schema := string(checklistPrompts.Schema())
	for _, field := range []string{`"targets"`, `"expects"`, `"glob"`, `"recipient"`, `"citation"`} {
		if !strings.Contains(schema, field) {
			t.Fatalf("v3 schema lacks %s", field)
		}
	}
}

// The deriver keeps a target code can stand behind and drops the rest: an exact
// value not verbatim in the request (the old 'Title.pdf' guess), a glob that does
// not compile, a "glob" with no wildcard that is not in the request, an unknown
// kind. A glob for the template is kept.
func TestDerive_TargetsAreAnchoredOrDropped(t *testing.T) {
	g := &scriptedGen{replies: []string{`{"items":[
	 {"type":"write","text":"Move the October receipts into the Receipts folder","quote":"move them into C:/Users/demo/home/Documents/Receipts",
	  "targets":[{"kind":"folder","value":"C:/Users/demo/home/Documents/Receipts","match":"exact"},
	             {"kind":"file","value":"C:/Users/demo/home/Documents/Receipts/2026-10-03 Northwind.pdf","match":"exact"}]},
	 {"type":"write","text":"Rename each to YYYY-MM-DD Title.pdf","quote":"in the form \"YYYY-MM-DD Title.pdf\"",
	  "targets":[{"kind":"file","value":"C:/Users/demo/home/Documents/Receipts/????-??-?? *.pdf","match":"glob"},
	             {"kind":"file","value":"Title.pdf","match":"exact"},
	             {"kind":"file","value":"2026-01-15 Acme Widget Invoice.pdf","match":"exact"},
	             {"kind":"file","value":"Receipts/[2026-*.pdf","match":"glob"},
	             {"kind":"file","value":"Invoice Title.pdf","match":"glob"},
	             {"kind":"planet","value":"Receipts","match":"exact"}]},
	 {"type":"constraint","text":"Do not touch any other file","quote":"Do not touch any other file","forbids":["write"]}]}`}}
	cl := (&Deriver{LLM: g, Constraints: constraintTerms}).Derive(context.Background(), p05Request)
	if len(cl.Items) != 3 {
		t.Fatalf("items = %+v", cl.Items)
	}
	move, rename := cl.Items[0].Targets, cl.Items[1].Targets
	if len(move) != 1 || move[0].Kind != domain.TargetKindFolder || move[0].Match != domain.TargetMatchExact {
		t.Fatalf("move targets = %+v (an invented file name is not in the request)", move)
	}
	// "Title.pdf" and the example "2026-01-15 Acme Widget Invoice.pdf" ARE verbatim
	// spans of the request, so the anchoring check cannot refuse them: telling a
	// placeholder or an example from a real name is the prompt's job (v3
	// "Placeholders and examples are not real names"). Targets are alternatives,
	// so the glob beside them still settles the item.
	if len(rename) != 3 || rename[0].Match != domain.TargetMatchGlob || !strings.HasSuffix(rename[0].Value, "????-??-?? *.pdf") ||
		rename[1].Value != "Title.pdf" || rename[2].Value != "2026-01-15 Acme Widget Invoice.pdf" {
		t.Fatalf("rename targets = %+v (the glob and the two verbatim spans survive)", rename)
	}
	if cl.Items[2].Targets != nil {
		t.Fatalf("a constraint carries no targets: %+v", cl.Items[2])
	}
}

func TestDerive_ExpectsKeepTheValueVocabulary(t *testing.T) {
	req := "Give me 3 options with the unit price, the lead time and the source page for each one."
	g := &scriptedGen{replies: []string{`{"items":[
	 {"type":"figure","text":"Report the unit price","quote":"the unit price","expects":["money","number"]},
	 {"type":"answer","text":"Report the source page","quote":"the source page for each one","expects":["citation","string","url"]},
	 {"type":"answer","text":"Report the lead time","quote":"the lead time","expects":["duration"]},
	 {"type":"figure","text":"Say how many options","quote":"3 options","expects":["number","any"]}]}`}}
	cl := (&Deriver{LLM: g}).Derive(context.Background(), req)
	want := [][]string{{"money", "number"}, {"citation", "string"}, nil, {"any"}}
	for i, w := range want {
		if !equalStrs(cl.Items[i].Expects, w) {
			t.Fatalf("item %d expects = %v, want %v", i, cl.Items[i].Expects, w)
		}
	}
}

// p03: "the source page for each one" answered by a cited URL settles an item that
// expects a citation or string; a number never answers an item expecting a
// citation; a figure item declaring no expects keeps the figure types.
func TestCoverage_ExpectsDecidesWhichValuesAnswerTheItem(t *testing.T) {
	url := val("Sterilux GmbH source page URL", domain.ValueString, "http://127.0.0.1:8777/suppliers/sterilux.html", "led_page")
	cite := val("source", domain.ValueCitation, "http://127.0.0.1:8777/suppliers/sterilux.html", "led_page")
	num := val("price", domain.ValueNumber, "12.5", "led_page")
	c := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementFigure, Expects: []string{domain.ValueCitation, domain.ValueString}},
		{ID: "R2", Type: domain.RequirementAnswer, Expects: []string{domain.ValueCitation, domain.ValueString}},
		{ID: "R3", Type: domain.RequirementAnswer, Expects: []string{domain.ValueCitation}},
		{ID: "R4", Type: domain.RequirementFigure},
		{ID: "R5", Type: domain.RequirementAnswer, Expects: []string{domain.ExpectAny}},
	}})
	sat := func(id string, vs ...domain.EnvelopeValue) domain.ItemResult {
		return domain.ItemResult{Item: id, Status: domain.RequirementSatisfied, Values: vs}
	}
	c.ObserveStep(StepObservation{Step: 0, ToolResults: 1, Results: []domain.ItemResult{
		sat("R1", url), sat("R2", cite), sat("R3", num), sat("R4", url), sat("R5", num)}})
	for id, want := range map[string]domain.RequirementStatus{"R1": domain.RequirementSatisfied, "R2": domain.RequirementSatisfied,
		"R3": domain.RequirementOpen, "R4": domain.RequirementOpen, "R5": domain.RequirementSatisfied} {
		if s, why := status(c, id); s != want {
			t.Fatalf("%s = %s %q, want %s", id, s, why, want)
		}
	}
	if _, why := status(c, "R3"); !strings.Contains(why, "expects (citation)") || !strings.Contains(why, "price (number)") {
		t.Fatalf("R3 reason must name the expected and the sent types: %q", why)
	}
	if _, why := status(c, "R4"); !strings.Contains(why, "number, money amount or date") {
		t.Fatalf("R4 keeps the figure rule: %q", why)
	}
}

// B4: a value the judge refused is no progress. The worker re-sends the refused
// URLs under new names every stage (p03 #2 ran to stage 7 this way); the stage is
// a stall, and the values are kept in the report marked not accepted.
func TestCoverage_RefusedValuesWithNewNamesAreNotProgress(t *testing.T) {
	c := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{{ID: "R4", Type: domain.RequirementFigure}}})
	send := func(name string) {
		c.ObserveStep(StepObservation{Step: 0, ToolResults: 1, Results: []domain.ItemResult{{Item: "R4", Status: domain.RequirementSatisfied,
			Values: []domain.EnvelopeValue{val(name, domain.ValueString, "http://127.0.0.1:8777/suppliers/sterilux.html", "led_page")}}}})
	}
	send("Sterilux source")
	if c.EndStage() {
		t.Fatal("a stage whose only change is a refused value made progress")
	}
	send("Sterilux source page URL")
	if c.EndStage() {
		t.Fatal("the same refused value under a new name is not progress")
	}
	snap := c.Snapshot().Items[0]
	if len(snap.Values) != 2 || !snap.Values[0].Refused || !snap.Values[1].Refused {
		t.Fatalf("refused values are kept, marked: %+v", snap.Values)
	}
	// An accepted value is progress, and accepting it clears its mark.
	c.ObserveStep(StepObservation{Step: 0, ToolResults: 1, Results: []domain.ItemResult{{Item: "R4", Status: domain.RequirementSatisfied,
		Values: []domain.EnvelopeValue{val("price", domain.ValueNumber, "12.5", "led_page")}}}})
	if !c.EndStage() {
		t.Fatal("an accepted value is progress")
	}
}

// A refused value never contests an accepted one as a conflict.
func TestCoverage_ARefusedValueIsNoConflict(t *testing.T) {
	c := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{{ID: "R1", Type: domain.RequirementAnswer,
		Expects: []string{domain.ValueMoney}}}})
	c.ObserveStep(StepObservation{Step: 0, Results: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied,
		Values: []domain.EnvelopeValue{val("total", domain.ValueString, "about a thousand", "led_a")}}}})
	c.ObserveStep(StepObservation{Step: 1, Results: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied,
		Values: []domain.EnvelopeValue{val("total", domain.ValueMoney, "1012.40", "led_b")}}}})
	if s, why := status(c, "R1"); s != domain.RequirementSatisfied {
		t.Fatalf("R1 = %s %q", s, why)
	}
}
