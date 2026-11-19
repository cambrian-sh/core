package replycheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func raws(fs []Finding) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.Raw)
	}
	sort.Strings(out)
	return out
}

func kinds(ms []Mention) map[string][]string {
	out := map[string][]string{}
	for _, m := range ms {
		out[m.Kind] = append(out[m.Kind], m.Raw)
	}
	return out
}

func TestExtractNumbersMoneyAndPercentInEnglishAndTurkish(t *testing.T) {
	cases := []struct {
		text, lang string
		kind       string
		want       float64
	}{
		{"The total is £1.4M.", "en", KindMoney, 1.4e6},
		{"Toplam 1.234,56 TL.", "tr", KindMoney, 1234.56},
		{"Toplam ₺1,2 milyon.", "tr", KindMoney, 1.2e6},
		{"Outstanding: $25,602.43", "en", KindMoney, 25602.43},
		{"Artış %12,5 oldu.", "tr", KindPercent, 12.5},
		{"Growth was 12.5%.", "en", KindPercent, 12.5},
		{"It has 15 iş günü.", "tr", KindNumber, 15},
		{"About 66,484 in total.", "en", KindNumber, 66484},
		{"Toplam 66.484 adet.", "tr", KindNumber, 66484},
	}
	for _, c := range cases {
		ms := Extract(c.text, Options{MinInteger: 10, Lang: c.lang})
		found := false
		for _, m := range ms {
			if m.Kind != c.kind {
				continue
			}
			for _, r := range m.Values {
				if r.V == c.want {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%q: no %s reading %v in %+v", c.text, c.kind, c.want, ms)
		}
	}
}

func TestExtractDatesIDsAndSkipsListPositionsTimesAndSmallCounts(t *testing.T) {
	text := "1. Draft MSG-000173 for CUS-004 sent 2026-11-08 at 09:01:38 UTC.\n" +
		"2. Due 8 Kasım 2026 or 08.11.2026, i.e. November 8, 2026; 3 invoices; the 4. madde."
	got := kinds(Extract(text, Options{MinInteger: 10, Lang: "en"}))
	if !reflect.DeepEqual(got[KindID], []string{"MSG-000173", "CUS-004"}) {
		t.Errorf("ids = %v", got[KindID])
	}
	if len(got[KindDate]) != 4 {
		t.Errorf("dates = %v, want four readings of 8 November 2026", got[KindDate])
	}
	if len(got[KindNumber]) != 0 {
		t.Errorf("numbers = %v; list positions, times, the date digits and small counts are not figures", got[KindNumber])
	}
}

func TestEnglishMayIsNotAMonthWithoutAYearOrCapital(t *testing.T) {
	ms := Extract("You have 20 may apply, or 20 May.", Options{MinInteger: 10})
	got := kinds(ms)
	if len(got[KindDate]) != 1 || got[KindDate][0] != "20 May" {
		t.Errorf("dates = %v", got[KindDate])
	}
}

func TestRoundingIsBackedByTheExactValue(t *testing.T) {
	ev := Evidence{Entries: []Entry{{ID: "led_aaaaaaaa11", Kind: "tool_result", Body: `{"total": 1412345.67, "overdue": 66483.75}`}}}
	for _, ok := range []string{"£1.4M", "£1.41 million", "66,484", "£66.5k", "66.483,75 TL"} {
		if r := Check("It is "+ok+" now.", ev, Options{MinInteger: 10, Lang: "en"}); len(r.Uncited) != 0 {
			t.Errorf("%s should be backed, got %+v", ok, r.Uncited)
		}
	}
	for _, bad := range []string{"£1.5M", "66,480", "£38,662.06"} {
		if r := Check("It is "+bad+" now.", ev, Options{MinInteger: 10, Lang: "en"}); len(r.Uncited) != 1 {
			t.Errorf("%s should be uncited, got %+v", bad, r)
		}
	}
}

func TestMarkersAreStrippedAndMiscitationsReported(t *testing.T) {
	ev := Evidence{Entries: []Entry{
		{ID: "led_aaaaaaaa11", Kind: "tool_result", Body: `{"amount": 20092.11}`},
		{ID: "led_bbbbbbbb22", Kind: "tool_result", Body: `{"amount": 25602.43}`},
		{ID: "led_cccccccc33", Kind: "step_output", Body: `£99.00`},
	}}
	reply := "Wychwood owes £20,092.11 [led_bbbbbbbb] and Marlowe £25,602.43 [led_bbbbbbbb]. Another £99.00 [led_cccccccc]. Gap [R2]."
	r := Check(reply, ev, Options{MinInteger: 10})
	if got := raws(r.Miscited); !reflect.DeepEqual(got, []string{"£20,092.11"}) {
		t.Errorf("miscited = %v", got)
	}
	// A step output is model text: it never backs a figure.
	if got := raws(r.Uncited); !reflect.DeepEqual(got, []string{"£99.00"}) {
		t.Errorf("uncited = %v", got)
	}
	if !reflect.DeepEqual(r.UnknownMarkers, []string{"led_cccccccc"}) {
		t.Errorf("unknown markers = %v", r.UnknownMarkers)
	}
	if got := StripMarkers(reply); got != "Wychwood owes £20,092.11 and Marlowe £25,602.43. Another £99.00. Gap." {
		t.Errorf("stripped = %q", got)
	}
}

func TestChecklistItemsAnsweredOrStatedAsGaps(t *testing.T) {
	items := []Item{
		{ID: "R1", Type: "answer", Status: "satisfied", Values: []Value{{Type: "id", Text: "CUS-004", EvidenceRef: "led_aaaaaaaa11"}}},
		{ID: "R2", Type: "figure", Status: "satisfied", Values: []Value{{Type: "money", Text: "66483.75"}}},
		{ID: "R3", Type: "answer", Status: "not_found", Quote: "who signed it"},
		{ID: "R4", Type: "constraint", Status: "open"},
		{ID: "R5", Type: "figure", Status: "satisfied", Values: []Value{{Type: "date", Text: "2026-11-08"}}},
	}
	ev := Evidence{Items: items}
	r := Check("CUS-004 is overdue; the total is £66,483.75; due 8 Kasım. I could not find who signed it [R3].", ev, Options{MinInteger: 10, Lang: "tr"})
	if len(r.Unanswered) != 0 || len(r.Unverified) != 0 {
		t.Errorf("all answered or stated: %+v", r)
	}
	r = Check("CUS-004 is overdue.", ev, Options{MinInteger: 10})
	var got []string
	for _, g := range r.Unanswered {
		got = append(got, g.Item+":"+g.Reason)
	}
	if !reflect.DeepEqual(got, []string{"R2:unanswered", "R5:unanswered"}) {
		t.Errorf("unanswered = %v", got)
	}
	// No item tag anywhere: whether R3's gap was stated is not decidable.
	if len(r.Unverified) != 1 || r.Unverified[0].Item != "R3" {
		t.Errorf("unverified = %+v", r.Unverified)
	}
	// With item tags in use, an untagged gap is a violation.
	r = Check("CUS-004 is overdue [R1]. Total £66,483.75 [R2], on 2026-11-08.", ev, Options{MinInteger: 10})
	if len(r.Unanswered) != 1 || r.Unanswered[0].Reason != "gap_not_stated" {
		t.Errorf("gap not stated: %+v", r.Unanswered)
	}
}

// ── the recorded replies (fixture: testdata/recorded_replies.json) ──────────

type recordedCase struct {
	Name, Source, Lang, User, Reply, Evidence string
}

func loadRecorded(t *testing.T) map[string]recordedCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "recorded_replies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct{ Cases []recordedCase }
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	out := map[string]recordedCase{}
	for _, c := range f.Cases {
		out[c.Name] = c
	}
	return out
}

// o3xEvidence is the world the o3x replies were about (the graders' figures): the
// aging report, the invoices, the CRM contacts and the three mail_draft results,
// plus the checklist coverage of the request.
func o3xEvidence() Evidence {
	return Evidence{
		Entries: []Entry{
			{ID: "led_0a0a0a0a0a01", Kind: "tool_result", Tool: "finance_aging_report", Body: `{"buckets":{"over_60":[` +
				`{"customer_id":"CUS-002","name":"Wychwood Clinic","overdue":20092.11},` +
				`{"customer_id":"CUS-004","name":"Marlowe Park Pharmacy","overdue":25602.43},` +
				`{"customer_id":"CUS-011","name":"Pentland Row Laboratory","overdue":20789.21}],"over_60_total":66483.75}}`},
			{ID: "led_0a0a0a0a0a02", Kind: "tool_result", Tool: "finance_list_invoices", Body: `[` +
				`{"id":"INV-10015","customer_id":"CUS-002","amount":19973.41,"days_overdue":109},` +
				`{"id":"INV-10032","customer_id":"CUS-002","amount":118.70,"days_overdue":81},` +
				`{"id":"INV-10017","customer_id":"CUS-004","amount":6736.14,"days_overdue":107},` +
				`{"id":"INV-10033","customer_id":"CUS-004","amount":18866.29,"days_overdue":73},` +
				`{"id":"INV-10016","customer_id":"CUS-011","amount":5971.75,"days_overdue":108},` +
				`{"id":"INV-10035","customer_id":"CUS-011","amount":14817.46,"days_overdue":72}]`},
			{ID: "led_0a0a0a0a0a03", Kind: "tool_result", Tool: "mail_draft", Body: `{"id":"MSG-000173","to":["brisa.stallard@pentland-row-laboratory.example"]}`},
			{ID: "led_0a0a0a0a0a04", Kind: "tool_result", Tool: "mail_draft", Body: `{"id":"MSG-000174","to":["jarek.hallowfield@marlowe-park-pharmacy.example"]}`},
			{ID: "led_0a0a0a0a0a05", Kind: "tool_result", Tool: "mail_draft", Body: `{"id":"MSG-000175","to":["brisa.ashcombe@wychwood-clinic.example"]}`},
			// The model's own step summary: never evidence for a figure.
			{ID: "led_0a0a0a0a0a06", Kind: "step_output", Body: `Total outstanding £38,662.06`},
		},
		Items: []Item{
			{ID: "R1", Type: "answer", Status: "satisfied", Quote: "Which customers are more than 60 days overdue?",
				Values:   []Value{{Type: "id", Text: "CUS-002"}, {Type: "string", Text: "Wychwood Clinic"}, {Type: "id", Text: "CUS-004"}, {Type: "id", Text: "CUS-011"}},
				Evidence: []string{"led_0a0a0a0a0a01"}},
			{ID: "R2", Type: "figure", Status: "satisfied", Quote: "more than 60 days overdue",
				Values: []Value{{Name: "over_60_total", Type: "money", Text: "66483.75", EvidenceRef: "led_0a0a0a0a0a01"}}},
			{ID: "R3", Type: "document", Status: "satisfied", Quote: "Draft a polite payment reminder to each of them",
				Values: []Value{{Type: "id", Text: "MSG-000173"}, {Type: "id", Text: "MSG-000174"}, {Type: "id", Text: "MSG-000175"}}},
			{ID: "R4", Type: "constraint", Status: "open", Quote: "don't send anything yet"},
		},
	}
}

func TestRecordedO3xRepliesFlagTheInventedTotalAndTheMissingFigure(t *testing.T) {
	cases := loadRecorded(t)
	ev := o3xEvidence()
	want := map[string][]string{
		// Attempt 1 stated a Wychwood total of £38,662.06: the sum of two invoices it
		// mixed up, in no tool result (the grader: "figure ... not stated").
		"o3x-attempt-1-turn-1": {"£38,662.06"},
		"o3x-attempt-2-turn-1": {},
		"o3x-attempt-3-turn-1": {},
	}
	for name, uncited := range want {
		c := cases[name]
		ev := ev
		ev.Context = []string{c.User}
		r := Check(c.Reply, ev, Options{MinInteger: 10, Lang: c.Lang})
		if got := raws(r.Uncited); !reflect.DeepEqual(got, uncited) {
			t.Errorf("%s: uncited = %v, want %v", name, got, uncited)
		}
		// None of the three replies states the over-60 total the request asked
		// for (R2): the graders failed all three on it.
		found := false
		for _, g := range r.Unanswered {
			if g.Item == "R2" && g.Reason == "unanswered" {
				found = true
			}
			if g.Item == "R1" || g.Item == "R3" {
				t.Errorf("%s: %s is answered in the reply, got %+v", name, g.Item, g)
			}
		}
		if !found {
			t.Errorf("%s: R2 (the total) should be unanswered: %+v", name, r.Unanswered)
		}
	}
}

const enProcedure = "Harrowmere District Council - Freedom of Information and Environmental Information Procedure (Code: INF). " +
	"4.1 We respond to a request within 20 working days of receipt, as section 10 of the Freedom of Information Act 2000 requires. " +
	"4.2 Where the public interest test must be considered, we may extend this by a further 20 working days and tell the requester."

const trYonerge = "Karaçam Belediyesi Bilgi Edinme Hakkı Uygulama Yönergesi. MADDE 4 – Cevap Süresi (1) Başvurulara 15 iş günü içinde cevap verilir. " +
	"4982 sayılı Bilgi Edinme Hakkı Kanunu'nun 11. maddesi uyarınca, istenen bilgi veya belge başka bir birimden temin edilecekse cevap süresi 30 iş günüdür; " +
	"uzatma ve gerekçesi 15 iş günlük süre içinde başvurucuya bildirilir. (2) Ret kararına karşı tebliğden itibaren 15 gün içinde Bilgi Edinme Değerlendirme Kurulu'na itiraz edilebilir."

func TestRecordedDeadlineRepliesEnglish(t *testing.T) {
	cases := loadRecorded(t)
	withDoc := Evidence{Entries: []Entry{{ID: "led_1b1b1b1b1b01", Kind: "tool_result", Tool: "docs_search", Body: enProcedure}}}
	none := Evidence{}

	// Attempts 1 and 3 answered from general knowledge (no delegation, nothing
	// read): every figure they state is uncited.
	for _, name := range []string{"en-deadline-attempt-1", "en-deadline-attempt-3"} {
		c := cases[name]
		r := Check(c.Reply, Evidence{Context: []string{c.User}}, Options{MinInteger: 10, Lang: "en"})
		if len(r.Uncited) == 0 {
			t.Errorf("%s: an answer from general knowledge must be flagged: %+v", name, r)
		}
	}
	// Attempt 2 delegated and quotes the procedure: only the 40 it added up itself
	// (20 + 20, model arithmetic, ADR-0145 D4) is not in the source.
	c := cases["en-deadline-attempt-2"]
	r := Check(c.Reply, withDoc, Options{MinInteger: 10, Lang: "en"})
	if got := raws(r.Uncited); !reflect.DeepEqual(got, []string{"40"}) {
		t.Errorf("attempt 2 uncited = %v, want [40]", got)
	}
	if r := Check(c.Reply, none, Options{MinInteger: 10, Lang: "en"}); len(r.Uncited) < 3 {
		t.Errorf("attempt 2 without evidence: %v", raws(r.Uncited))
	}
}

func TestRecordedDeadlineRepliesTurkish(t *testing.T) {
	cases := loadRecorded(t)
	withDoc := Evidence{Entries: []Entry{{ID: "led_2c2c2c2c2c01", Kind: "tool_result", Tool: "docs_search", Body: trYonerge}}}
	for _, name := range []string{"tr-deadline-attempt-1", "tr-deadline-attempt-3"} {
		c := cases[name]
		r := Check(c.Reply, withDoc, Options{MinInteger: 10, Lang: "tr"})
		if len(r.Uncited) != 0 {
			t.Errorf("%s: every figure is in the Yönerge, got uncited %v", name, raws(r.Uncited))
		}
		if r.Checked < 5 {
			t.Errorf("%s: checked only %d values", name, r.Checked)
		}
	}
	// Attempt 2 answered from general knowledge: 4982 and 15 rest on nothing read.
	c := cases["tr-deadline-attempt-2"]
	r := Check(c.Reply, Evidence{Context: []string{c.User}}, Options{MinInteger: 10, Lang: "tr"})
	if got := raws(r.Uncited); !reflect.DeepEqual(got, []string{"15", "4982"}) {
		t.Errorf("attempt 2 uncited = %v", got)
	}
}

func TestTheUsersOwnWordsAndTodayBackAValue(t *testing.T) {
	ev := Evidence{Context: []string{"Please check invoice INV-7781 for £1,250.", "Today is 2026-11-08 (Sunday)."}}
	r := Check("Invoice INV-7781 (£1,250) is due today, 8 November 2026.", ev, Options{MinInteger: 10})
	if len(r.Uncited) != 0 {
		t.Errorf("uncited = %v", raws(r.Uncited))
	}
}

// ── recalled organisation rules (T-20261109-chat-agent-answers-a-published-rule) ──
//
// Replies and recalled chunks copied from the citizen gate run
// cambrian-benchmarks/runs/20261109T0320Z_gate2_citizen_live (transcripts/*, and the
// <recalled_memory> of traces/agent-chat_agent-*.jsonl). The recall is now a ledger
// entry (tool memory_recall), so the figures are backed; a reply that drops the
// procedure's section / madde is reported as missing its source.

func recallEntry(t *testing.T, id string, hits ...[3]string) Entry {
	t.Helper()
	var b recallBody
	for _, h := range hits {
		b.Hits = append(b.Hits, struct {
			Text     string `json:"text"`
			Document string `json:"document"`
			Section  string `json:"section"`
		}{Text: h[0], Document: h[1], Section: h[2]})
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return Entry{ID: id, Kind: "tool_result", Tool: "memory_recall", Body: string(raw)}
}

const (
	enFOIChunk        = "**4. Time for compliance**\nThe council responds promptly and in any event within 20 working days (s.10 FOIA). Where the public interest test must be considered the council may extend by up to a further 20 working days and will tell the requester (s.10(3)).\n**5. Redaction**\nWhere exempt information can be separated, the remainder of a record is released with the exempt parts redacted; third parties' personal data is redacted under s.40(2)."
	enComplaintsChunk = "**4. Stage 1**\nStage 1 complaints receive a full response within 10 working days.\n**5. Stage 2**\nA complainant who is dissatisfied may escalate to stage 2, answered within 20 working days; thereafter they may contact the Local Government and Social Care Ombudsman."
	trFOIChunk        = "**MADDE 3 – Başvuru**\n(1) Başvurular dilekçe, elektronik posta, e-Belediye veya CİMER aracılığıyla yapılabilir.\n**MADDE 4 – Cevap süresi**\n(1) Başvurulara 4982 sayılı Kanunun 11 inci maddesi uyarınca 15 iş günü içinde cevap verilir. (2) Bilgi veya belgenin başka bir birimden sağlanması gerekirse süre 30 iş gününe uzatılır; uzatma ve gerekçesi süre içinde başvurana bildirilir.\n**MADDE 5 – Ayrılabilir belgeler**"
	trPurposeChunk    = "**MADDE 1 – Amaç**\n(1) Bu Yönergenin amacı, 4982 sayılı Bilgi Edinme Hakkı Kanunu kapsamındaki başvuruların Belediyede ele alınış usulünü belirlemektir."
)

func TestRecalledRuleFiguresAreBackedButMustNameTheirSource(t *testing.T) {
	en := Evidence{Entries: []Entry{recallEntry(t, "led_recall_en",
		[3]string{enFOIChunk, "", ""}, [3]string{enComplaintsChunk, "", ""})},
		Context: []string{"How many working days does the council have to answer my FOI request?"}}
	// The recorded EN final reply (g-en-citizen-records-deadline attempt 1).
	recorded := "The council has 20 working days to answer your FOI request. If the public interest test must be considered, the council may extend by up to a further 20 working days and will tell you."
	rep := Check(recorded, en, Options{MinInteger: 10, Lang: "en"})
	if len(rep.Uncited) != 0 {
		t.Fatalf("memory-grounded figures counted as uncited: %+v", rep.Uncited)
	}
	if len(rep.MissingSource) != 1 || rep.MissingSource[0].Entry != "led_recall_en" || rep.Violations() != 1 {
		t.Fatalf("the reply drops section 4 and s.10; want one missing source, got %+v", rep.MissingSource)
	}
	fixed := "Under section 4 (Time for compliance) of the council's Freedom of Information procedure, the council answers within 20 working days [led_recall_en] (s.10 FOIA)."
	if rep := Check(fixed, en, Options{MinInteger: 10, Lang: "en"}); len(rep.MissingSource) != 0 || rep.Violations() != 0 {
		t.Fatalf("a reply naming the section was flagged: %+v", rep)
	}

	tr := Evidence{Entries: []Entry{recallEntry(t, "led_recall_tr",
		[3]string{trFOIChunk, "", ""}, [3]string{trPurposeChunk, "", ""})},
		Context: []string{"Bilgi edinme başvuruma kaç iş günü içinde cevap verilmesi gerekir?"}}
	recordedTR := "Bilgi edinme başvurunuza **15 iş günü** içinde cevap verilmesi gerekir. Ancak, istediğiniz bilgi veya belgenin başka bir birimden sağlanması gerekirse, bu süre **30 iş gününe uzatılır**. Eğer süre uzatılması söz konusuysa, uzatma kararı ve gerekçesi bu süre içinde size bildirilmelidir.\n\nBu hüküm, 4982 sayılı Bilgi Edinme Hakkı Kanununa dayanılarak belirlenmiştir."
	rep = Check(recordedTR, tr, Options{MinInteger: 10, Lang: "tr"})
	if len(rep.Uncited) != 0 || len(rep.MissingSource) == 0 {
		t.Fatalf("TR: uncited %+v, missing source %+v", rep.Uncited, rep.MissingSource)
	}
	fixedTR := "Bilgi Edinme Yönergesi MADDE 4 (Cevap süresi) uyarınca başvurunuza 15 iş günü içinde cevap verilir [led_recall_tr]; başka bir birimden bilgi gerekirse süre 30 iş gününe uzar. Dayanak: 4982 sayılı Kanunun 11 inci maddesi."
	if rep := Check(fixedTR, tr, Options{MinInteger: 10, Lang: "tr"}); len(rep.MissingSource) != 0 {
		t.Fatalf("TR: a reply naming Madde 4 was flagged: %+v", rep.MissingSource)
	}
	// A figure a tool result backs needs no document source.
	tool := Evidence{Entries: []Entry{{ID: "led_tool", Kind: "tool_result", Tool: "cases_get", Body: `{"due_in_working_days": 20}`},
		recallEntry(t, "led_recall_en", [3]string{enFOIChunk, "", ""})}}
	if rep := Check("You will hear back within 20 working days.", tool, Options{MinInteger: 10}); len(rep.MissingSource) != 0 {
		t.Fatalf("a tool-backed figure was asked for a document source: %+v", rep.MissingSource)
	}
}

func TestSourceLabelsFromSectionPathDocumentAndHeadings(t *testing.T) {
	got := sourceLabels("Freedom of Information procedure", "FOI procedure > 4. Time for compliance", "**MADDE 4 – Cevap süresi**\n(1) text")
	want := []string{"Freedom of Information procedure", "FOI procedure", "4. Time for compliance", "Time for compliance", "MADDE 4 – Cevap süresi", "MADDE 4", "Cevap süresi"}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("label %q missing from %q", w, got)
		}
	}
}

// ENG-2165: SharesValue is the chat lane's content test for source inheritance.
func TestSharesValue(t *testing.T) {
	opt := Options{MinInteger: 1000}
	for _, tc := range []struct {
		reply, given string
		want         bool
	}{
		{"As I said: HERON-77.", "Agreement HERON-77 is signed.", true},
		{"The total is about £1.4M.", "Invoice total £1,412,000.", true},
		{"We meet on 8 November 2026.", "Visit: 08.11.2026", true},
		{"Glad to help, anything else?", "Agreement HERON-77 is signed.", false},
		{"There are 3 invoices.", "3 invoices were paid.", false}, // below MinInteger: a count, not a value
		{"", "Agreement HERON-77", false},
		{"HERON-77", "", false},
	} {
		if got := SharesValue(tc.reply, tc.given, opt); got != tc.want {
			t.Errorf("SharesValue(%q, %q) = %v, want %v", tc.reply, tc.given, got, tc.want)
		}
	}
}
