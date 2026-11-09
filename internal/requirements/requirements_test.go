package requirements

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ── Derivation (D5) ──────────────────────────────────────────────────────────

type scriptedGen struct {
	replies []string
	prompts []string
}

func (g *scriptedGen) Generate(_ context.Context, prompt string) (string, error) {
	g.prompts = append(g.prompts, prompt)
	r := g.replies[0]
	if len(g.replies) > 1 {
		g.replies = g.replies[1:]
	}
	return r, nil
}

// constraintTerms is a constraint vocabulary (execution.tools.constraint_classes).
var constraintTerms = map[string][]domain.ToolEffect{
	"read": {domain.EffectRead}, "write": {domain.EffectWrite}, "send": {domain.EffectEgress},
}

const request = "Fevzipaşa şubesinde vadesi 60 günü geçen müşterileri listele, toplam borcu söyle ve her birine hatırlatma taslağı hazırla ama henüz gönderme."

func TestDerive_QuotesAreCheckedRepairedOnceThenDropped(t *testing.T) {
	first := `{"items":[
	 {"type":"answer","text":"List customers more than 60 days overdue","quote":"vadesi 60 günü geçen müşterileri listele"},
	 {"type":"figure","text":"State the total debt","quote":"toplam borç"},
	 {"type":"document","text":"Draft a reminder for each","quote":"hatırlatma taslağı hazırla"},
	 {"type":"constraint","text":"Do not send yet","quote":"henüz gönderme","forbids":["send","teleport"]},
	 {"type":"wish","text":"bogus type","quote":"listele"}]}`
	// The repair turn fixes the figure's quote but invents one for the document.
	repaired := `{"items":[
	 {"type":"answer","text":"List customers more than 60 days overdue","quote":"vadesi 60 günü geçen müşterileri listele"},
	 {"type":"figure","text":"State the total debt","quote":"toplam borcu söyle"},
	 {"type":"document","text":"Draft a reminder for each","quote":"draft a reminder"},
	 {"type":"constraint","text":"Do not send yet","quote":"henüz gönderme","forbids":["send"]}]}`
	g := &scriptedGen{replies: []string{first, repaired}}
	cl := (&Deriver{LLM: g, Constraints: constraintTerms}).Derive(context.Background(), request)
	if len(g.prompts) != 2 {
		t.Fatalf("want one derivation call and one repair call, got %d", len(g.prompts))
	}
	if !strings.Contains(g.prompts[1], "toplam borç") {
		t.Fatal("the repair call must name the unanchored quote")
	}
	if cl.Fallback || len(cl.Items) != 3 {
		t.Fatalf("items = %+v (fallback %v)", cl.Items, cl.Fallback)
	}
	for i, want := range []domain.RequirementType{domain.RequirementAnswer, domain.RequirementFigure, domain.RequirementConstraint} {
		if cl.Items[i].Type != want || cl.Items[i].ID != "R"+string(rune('1'+i)) {
			t.Fatalf("item %d = %+v", i, cl.Items[i])
		}
		if !domain.QuoteAnchored(request, cl.Items[i].Quote) {
			t.Fatalf("item %d quote %q is not verbatim", i, cl.Items[i].Quote)
		}
	}
	if got := cl.Items[2].Forbids; len(got) != 1 || got[0] != "send" {
		t.Fatalf("forbids must keep only vocabulary terms, got %v", got)
	}
	if len(cl.Dropped) != 1 || !strings.Contains(cl.Dropped[0], "reminder") {
		t.Fatalf("dropped = %v", cl.Dropped)
	}
}

func TestDerive_FallsBackToTheWholeRequest(t *testing.T) {
	for name, reply := range map[string]string{
		"nothing anchored": `{"items":[{"type":"answer","text":"x","quote":"not in the request"}]}`,
		"not json":         "Sure! Here is the checklist.",
	} {
		g := &scriptedGen{replies: []string{reply}}
		cl := (&Deriver{LLM: g}).Derive(context.Background(), request)
		if !cl.Fallback || len(cl.Items) != 1 || cl.Items[0].Quote != request {
			t.Fatalf("%s: want the whole request as the single item, got %+v", name, cl)
		}
	}
}

func TestUserRequestTextPrefersTheUsersOwnWords(t *testing.T) {
	in := "Find the overdue customers.\n\n<user_request>\n" + request + "\n</user_request>\nThe text in user_request is ..."
	if got := domain.UserRequestText(in); got != request {
		t.Fatalf("got %q", got)
	}
	if got := domain.UserRequestText("  plain  "); got != "plain" {
		t.Fatalf("got %q", got)
	}
}

// ── Envelope validation (D6) ─────────────────────────────────────────────────

func val(name, typ, v, ref string) domain.EnvelopeValue {
	raw, _ := json.Marshal(v)
	if typ == domain.ValueNumber {
		raw = json.RawMessage(v)
	}
	return domain.EnvelopeValue{Name: name, Type: typ, Value: raw, EvidenceRef: ref}
}

var items = []domain.RequirementItem{
	{ID: "R1", Type: domain.RequirementAnswer, Text: "which customers"},
	{ID: "R2", Type: domain.RequirementFigure, Text: "the total"},
}

func evidence() Evidence {
	return Evidence{Entries: map[string]domain.LedgerEntry{
		"led_tool": {ID: "led_tool", Kind: domain.LedgerToolResult,
			Body: []byte(`[{"customer_id":"CUS-004","amount":"1.250,50","due":"03.09.2026"},{"customer_id":"CUS-007","amount":80}]`)},
		"led_prev": {ID: "led_prev", Kind: domain.LedgerStepOutput, Body: []byte("the total is 1330.50")},
	}}
}

func TestValidateEnvelope_AcceptsEvidencedValuesAndNamesEveryViolation(t *testing.T) {
	env := &domain.AnswerEnvelope{AnswerText: "CUS-004 and CUS-007 owe 1330.50", ItemResults: []domain.ItemResult{
		{Item: "R1", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{
			val("customer", domain.ValueID, "CUS-004", "led_tool"),    // ok
			val("customer", domain.ValueID, "CUS-999", "led_tool"),    // not in evidence
			val("due", domain.ValueDate, "2026-09-03", "led_tool"),    // ok: 03.09.2026
			val("note", domain.ValueString, "x", "led_elsewhere"),     // foreign evidence
			val("when", domain.ValueDate, "next Tuesday", "led_tool"), // bad type
		}},
		{Item: "R2", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{
			val("amount", domain.ValueMoney, "1250.50 EUR", "led_tool"), // ok: 1.250,50
			val("total", domain.ValueNumber, "1330.5", "led_prev"),      // figure from model text
		}},
		{Item: "R9", Status: domain.RequirementSatisfied},
	}}
	got, vs := ValidateEnvelope(env, items, evidence())
	rules := map[string]int{}
	for _, v := range vs {
		rules[v.Rule]++
	}
	want := map[string]int{domain.ViolationNotInEvidence: 1, domain.ViolationForeignEvidence: 1, domain.ViolationBadType: 1,
		domain.ViolationFigureFromText: 1, domain.ViolationUnknownItem: 1}
	for r, n := range want {
		if rules[r] != n {
			t.Fatalf("rule %s: %d violations, want %d (all: %+v)", r, rules[r], n, vs)
		}
	}
	if len(got) != 2 || len(got[0].Values) != 2 || len(got[1].Values) != 1 {
		t.Fatalf("accepted = %+v", got)
	}
	if _, vs := ValidateEnvelope(nil, items, evidence()); len(vs) != 1 || vs[0].Rule != domain.ViolationNoEnvelope {
		t.Fatalf("a prose answer must be one no_envelope violation, got %+v", vs)
	}
	_, vs = ValidateEnvelope(&domain.AnswerEnvelope{}, items, evidence())
	if len(vs) != 2 || vs[0].Rule != domain.ViolationMissingItem {
		t.Fatalf("missing items: %+v", vs)
	}
}

// ── Coverage (D5) ────────────────────────────────────────────────────────────

func checklist() domain.RequirementChecklist {
	return domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer},
		{ID: "R2", Type: domain.RequirementFigure},
		{ID: "R3", Type: domain.RequirementWrite},
		{ID: "R4", Type: domain.RequirementConstraint, Forbids: []string{"send"}},
	}}
}

func status(c *Coverage, id string) (domain.RequirementStatus, string) {
	for _, it := range c.Snapshot().Items {
		if it.Item.ID == id {
			return it.Status, it.Reason
		}
	}
	return "", ""
}

func TestCoverage_PerTypeRulesAreDecidedFromKernelRecords(t *testing.T) {
	c := NewCoverage(checklist())
	c.ObserveStep(StepObservation{Step: 0, ToolResults: 1, Results: []domain.ItemResult{
		{Item: "R1", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{val("customer", domain.ValueID, "CUS-004", "led_tool")}},
		{Item: "R2", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{val("name", domain.ValueString, "x", "led_tool")}},
		{Item: "R3", Status: domain.RequirementSatisfied},
	}})
	if s, _ := status(c, "R1"); s != domain.RequirementSatisfied {
		t.Fatalf("R1 = %s", s)
	}
	if s, why := status(c, "R2"); s != domain.RequirementOpen || !strings.Contains(why, "number") {
		t.Fatalf("a figure item with no figure must stay open: %s %q", s, why)
	}
	if s, why := status(c, "R3"); s != domain.RequirementOpen || !strings.Contains(why, "action ledger") {
		t.Fatalf("a write claimed without an executed write must stay open: %s %q", s, why)
	}
	c.ObserveStep(StepObservation{Step: 1, Results: []domain.ItemResult{{Item: "R3", Status: domain.RequirementSatisfied}},
		Actions: []domain.PlanAction{{Step: 1, Tool: "mcp:crm/note_add", Status: domain.PlanActionDone}}})
	if s, _ := status(c, "R3"); s != domain.RequirementSatisfied {
		t.Fatalf("R3 = %s", s)
	}
	// A constraint is never satisfied; a forbidden call fails it, a refused one does not.
	effects := func(tool string) []string {
		if strings.Contains(tool, "send") {
			return []string{"send"}
		}
		return []string{"write"}
	}
	c.ObserveActions([]domain.PlanAction{{Step: 2, Tool: "mcp:mail/send", Status: domain.PlanActionRefused}}, effects)
	if s, _ := status(c, "R4"); s != domain.RequirementOpen {
		t.Fatalf("a refused send violates nothing, R4 = %s", s)
	}
	c.ObserveActions([]domain.PlanAction{{Step: 2, Tool: "mcp:mail/send", Status: domain.PlanActionDone}}, effects)
	if s, why := status(c, "R4"); s != domain.RequirementFailed || !strings.Contains(why, "violated") {
		t.Fatalf("R4 = %s %q", s, why)
	}
}

func TestCoverage_NotFoundNeedsRealSearchesAndASecondAttemptAfterErrors(t *testing.T) {
	c := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{{ID: "R1", Type: domain.RequirementAnswer}}})
	nf := []domain.ItemResult{{Item: "R1", Status: domain.RequirementNotFound, Reason: "no such permit"}}
	c.ObserveStep(StepObservation{Step: 0, Results: nf})
	if s, why := status(c, "R1"); s != domain.RequirementOpen || !strings.Contains(why, "without a search") {
		t.Fatalf("not_found with no search accepted: %s %q", s, why)
	}
	c.ObserveStep(StepObservation{Step: 0, Results: nf, ToolResults: 2, ToolErrors: 2})
	st := c.NextStage(2)
	if s, _ := status(c, "R1"); s != domain.RequirementOpen || len(st.Open) != 1 || !strings.Contains(st.Open[0].Retry, "different strategy") {
		t.Fatalf("errored searches must ask for a different strategy first: %s %+v", s, st)
	}
	c.ObserveStep(StepObservation{Step: 0, Results: nf, ToolResults: 1, ToolErrors: 1})
	if s, _ := status(c, "R1"); s != domain.RequirementNotFound {
		t.Fatalf("after the second attempt errored too, not_found is accepted; got %s", s)
	}
	c2 := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{{ID: "R1", Type: domain.RequirementAnswer}}})
	c2.ObserveStep(StepObservation{Step: 0, Results: nf, ToolResults: 3, ToolErrors: 1})
	if s, _ := status(c2, "R1"); s != domain.RequirementNotFound {
		t.Fatalf("real searches back not_found; got %s", s)
	}
	// not_permitted only from the kernel's own refusal records.
	c3 := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{{ID: "R1", Type: domain.RequirementAnswer}}})
	np := []domain.ItemResult{{Item: "R1", Status: domain.RequirementNotPermitted}}
	c3.ObserveStep(StepObservation{Step: 0, Results: np})
	if s, _ := status(c3, "R1"); s != domain.RequirementOpen {
		t.Fatalf("not_permitted without a refusal accepted: %s", s)
	}
	c3.ObserveStep(StepObservation{Step: 0, Results: np, Actions: []domain.PlanAction{{Tool: "hr_get", Status: domain.PlanActionRefused, Detail: "approval denied"}}})
	if s, _ := status(c3, "R1"); s != domain.RequirementNotPermitted {
		t.Fatalf("got %s", s)
	}
}

func TestCoverage_ConflictKeepsTheItemOpen(t *testing.T) {
	c := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{{ID: "R1", Type: domain.RequirementFigure}}})
	c.ObserveStep(StepObservation{Step: 0, Results: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied,
		Values: []domain.EnvelopeValue{val("total", domain.ValueMoney, "1330.50", "led_a")}}}})
	c.ObserveStep(StepObservation{Step: 1, Results: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied,
		Values: []domain.EnvelopeValue{val("total", domain.ValueMoney, "1,330.50", "led_b")}}}})
	if s, _ := status(c, "R1"); s != domain.RequirementSatisfied {
		t.Fatalf("the same amount written differently is no conflict; got %s", s)
	}
	c.ObserveStep(StepObservation{Step: 2, Results: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied,
		Values: []domain.EnvelopeValue{val("total", domain.ValueMoney, "1250.00", "led_c")}}}})
	if s, why := status(c, "R1"); s != domain.RequirementOpen || !strings.Contains(why, "conflict") {
		t.Fatalf("two different totals must keep the item open as a conflict: %s %q", s, why)
	}
}

func TestCoverage_ForEachNeedsOneResultPerMember(t *testing.T) {
	c := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{{ID: "R1", Type: domain.RequirementAnswer}}})
	c.ExpectMembers("R1", []string{"CUS-004", "CUS-007"})
	ok := func(member string) StepObservation {
		return StepObservation{Step: 1, Member: member, ToolResults: 1, Results: []domain.ItemResult{{Item: "R1",
			Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{val("draft", domain.ValueID, "MSG-1", "led_t")}}}}
	}
	c.ObserveStep(ok("CUS-004"))
	if s, why := status(c, "R1"); s != domain.RequirementOpen || !strings.Contains(why, "CUS-007") {
		t.Fatalf("a missing member must keep the item open, naming it: %s %q", s, why)
	}
	if st := c.NextStage(2); len(st.Open) != 1 || len(st.Open[0].Missing) != 1 || st.Open[0].Missing[0] != "CUS-007" {
		t.Fatalf("the next stage must cover exactly the missing member: %+v", st)
	}
	c.ObserveStep(ok("CUS-007"))
	if s, _ := status(c, "R1"); s != domain.RequirementSatisfied {
		t.Fatalf("every member covered: got %s", s)
	}
}

func TestCoverage_AStageThatChangesNothingIsAStall(t *testing.T) {
	c := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer}, {ID: "R2", Type: domain.RequirementAnswer}}})
	c.ObserveStep(StepObservation{Step: 0, Results: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied,
		Values: []domain.EnvelopeValue{val("a", domain.ValueString, "x", "led_1")}}}})
	if !c.EndStage() {
		t.Fatal("a stage that satisfied an item made progress")
	}
	// Stage 2: the worker only repeats a failed claim.
	c.ObserveStep(StepObservation{Step: 0, Results: []domain.ItemResult{{Item: "R2", Status: domain.RequirementFailed, Reason: "tool down"}}})
	if c.EndStage() {
		t.Fatal("a stage that changed no status and added no evidence is a stall")
	}
	c.CloseGaps("stalled")
	if !c.Settled() {
		t.Fatal("after the stall every item is final")
	}
	if s, why := status(c, "R2"); s != domain.RequirementFailed || !strings.Contains(why, "gap") || !strings.Contains(why, "tool down") {
		t.Fatalf("the open item closes as a gap with the worker's reason: %s %q", s, why)
	}
}

// T-20261108-coverage-cannot-accept-not-permitted-for-a: a refused READ is the
// kernel's record of an access refusal, so not_permitted is accepted on it; with
// no refusal of any kind the claim is still refused.
func TestCoverage_NotPermittedIsAcceptedOnARefusedRead(t *testing.T) {
	c := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer}, {ID: "R2", Type: domain.RequirementAnswer}}})
	np := func(id string) []domain.ItemResult {
		return []domain.ItemResult{{Item: id, Status: domain.RequirementNotPermitted, Reason: "hr records are closed to me"}}
	}
	c.ObserveStep(StepObservation{Step: 0, Results: np("R1")})
	if s, why := status(c, "R1"); s != domain.RequirementOpen || !strings.Contains(why, "no refusal") {
		t.Fatalf("not_permitted with no kernel record accepted: %s %q", s, why)
	}
	c.ObserveStep(StepObservation{Step: 1, Results: np("R2"), Denials: []domain.PlanAction{
		{Step: 1, Tool: "mcp:hr/employee_get", Status: domain.PlanActionRefused, Detail: "scope: hr not granted"}}})
	if s, why := status(c, "R2"); s != domain.RequirementNotPermitted || !strings.Contains(why, "scope: hr not granted") {
		t.Fatalf("a refused read must back not_permitted: %s %q", s, why)
	}
}

// o3x #2 (T-20261109-staged-coverage-settles-a-write-item-not): a step saved two
// drafts and had a third refused, and the worker claimed not_permitted. The item
// is satisfied by the executed writes; the refused target is kept per target and
// never turns the item not_permitted.
func TestCoverage_ARefusalDoesNotEraseExecutedWrites(t *testing.T) {
	c := NewCoverage(domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R2", Type: domain.RequirementDocument}, {ID: "R3", Type: domain.RequirementWrite}}})
	acts := []domain.PlanAction{
		{Step: 2, Tool: "mail_draft", Status: domain.PlanActionDone, Target: "mail_draft|to=brisa@wychwood.example", IDs: []string{"id=W-000002"}},
		{Step: 2, Tool: "mail_draft", Status: domain.PlanActionDone, Target: "mail_draft|to=ops@marlowe.example", IDs: []string{"id=W-000003"}},
		{Step: 2, Tool: "mail_draft", Status: domain.PlanActionRefused, Target: "mail_draft|to=brisa@wychwood.example", Detail: "denied"},
		{Step: 2, Tool: "mail_draft", Status: domain.PlanActionRefused, Target: "mail_draft|to=carrow@hollins.example", Detail: "denied"},
	}
	c.ObserveStep(StepObservation{Step: 2, Actions: acts, Results: []domain.ItemResult{
		{Item: "R2", Status: domain.RequirementNotPermitted, Reason: "one draft was refused"},
		{Item: "R3", Status: domain.RequirementNotPermitted}}})
	for _, id := range []string{"R2", "R3"} {
		s, why := status(c, id)
		if s != domain.RequirementSatisfied || !strings.Contains(why, "carrow@hollins.example not_permitted") {
			t.Fatalf("%s = %s %q", id, s, why)
		}
	}
	snap := c.Snapshot()
	var brisa TargetResult
	for _, tr := range snap.Items[0].Targets {
		if tr.Target == "mail_draft|to=brisa@wychwood.example" {
			brisa = tr
		}
	}
	if brisa.Status != domain.RequirementSatisfied || len(snap.Items[0].Targets) != 3 {
		t.Fatalf("targets = %+v", snap.Items[0].Targets)
	}
}
