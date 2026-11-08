package domain

import (
	"reflect"
	"strings"
	"testing"
)

// o3ARSourceObject is the JSON object the finance_aging_report step (step 0) of
// o3-ar-reminders #1 answered with (run R-20261107T182608Z-default, run dir
// 20261107T182608Z_usecases_full, kernel-U3 21:42:28). The log keeps only a 300-char
// preview; the rest is reconstructed from the 26 child queries the line split produced
// ("For the customer \"contact_email\": …, use crm_search_accounts …") and checked
// against the stored payload size (content_store_put data_bytes=1170 = this object,
// a blank line, and o3ARSourceArray). Two array fields, rows keyed by customer_name.
const o3ARSourceObject = `{
  "customers_60plus_overdue": [
    {
      "customer_name": "Wychwood Clinic",
      "overdue_amount_60plus": 20092.11
    },
    {
      "customer_name": "Marlowe Park Pharmacy",
      "overdue_amount_60plus": 25602.43
    },
    {
      "customer_name": "Pentland Row Laboratory",
      "overdue_amount_60plus": 20789.21
    }
  ],
  "follow_up_contacts": [
    {
      "customer_name": "Wychwood Clinic",
      "contact_name": "Brisa Ashcombe",
      "contact_email": "brisa.ashcombe@wychwood-clinic.example",
      "contact_phone": "+44 1632 266264",
      "account_id": "CUS-002"
    },
    {
      "customer_name": "Marlowe Park Pharmacy",
      "contact_name": "Jarek Hallowfield",
      "contact_email": "jarek.hallowfield@marlowe-park-pharmacy.example",
      "contact_phone": "+44 1632 480725",
      "account_id": "CUS-004"
    },
    {
      "customer_name": "Pentland Row Laboratory",
      "contact_name": "Brisa Stallard",
      "contact_email": "brisa.stallard@pentland-row-laboratory.example",
      "contact_phone": "+44 1632 283887",
      "account_id": "CUS-011"
    }
  ]
}`

// o3ARSourceArray is the item array the fanout_source instruction asked for, which the
// worker put on the last line — unfenced.
const o3ARSourceArray = `["Wychwood Clinic", "Marlowe Park Pharmacy", "Pentland Row Laboratory"]`

var o3ARCustomers = []string{"Wychwood Clinic", "Marlowe Park Pharmacy", "Pentland Row Laboratory"}

// o3ARSource is the whole step-0 output.
const o3ARSource = o3ARSourceObject + "\n\n" + o3ARSourceArray

// readFanOutPlan is the o3 #1 plan shape: a read step, then a read-only per-customer
// lookup over it.
func readFanOutPlan() *ExecutionPlan {
	return &ExecutionPlan{Steps: []Step{
		{Query: "Use finance_aging_report to list the customers 60+ days overdue", RequiredEffects: []string{"read"}},
		{Query: "For the customer {item}, use crm_search_accounts then crm_get_account", DependsOn: []int{0}, FanOutOver: iptr(0), RequiredEffects: []string{"read"}},
		{Query: "Draft the reminders", IsThought: true, DependsOn: []int{1}},
	}}
}

func TestO3ARSource_IsTheRecordedPayload(t *testing.T) {
	t.Parallel()
	if len(o3ARSource) != 1170 {
		t.Fatalf("reconstructed source is %d bytes, the stored payload was 1170", len(o3ARSource))
	}
	// The old extractor's line split of this output is exactly the 26 children seen.
	if n := len(listLines(o3ARSource)); n != 26 {
		t.Fatalf("line split gives %d items, the run expanded 26", n)
	}
}

// The bug: the read-only fan-out over the o3 #1 source made 26 children of JSON lines.
// It must expand to the three customers the reply ends with.
func TestResolveFanOut_ReadOnlyOverO3SourceExpandsToTheCustomers(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"recorded (object, blank line, bare array)": o3ARSource,
		"array fenced":                    o3ARSourceObject + "\n\n```json\n" + o3ARSourceArray + "\n```\n",
		"object and array both fenced":    "```json\n" + o3ARSourceObject + "\n```\n\nItems:\n```json\n" + o3ARSourceArray + "\n```",
		"lead-in prose, then both values": "Here are the results.\n\n" + o3ARSource,
	} {
		t.Run(name, func(t *testing.T) {
			plan := readFanOutPlan()
			out, items, how, err := ResolveFanOut(plan, 1, src, 50)
			if err != nil || how != FanOutExpanded {
				t.Fatalf("outcome = %v err %v", how, err)
			}
			if !reflect.DeepEqual(items, o3ARCustomers) {
				t.Fatalf("items = %q, want %q", items, o3ARCustomers)
			}
			if got := out.Steps[3].Query; got != "For the customer Wychwood Clinic, use crm_search_accounts then crm_get_account" {
				t.Errorf("first child = %q", got)
			}
		})
	}
}

// Without the trailing array the object alone is ambiguous (two array fields). JSON
// is never split into lines: the read-only step runs ONCE over it; a write step is
// demoted as before.
func TestResolveFanOut_OverUnrecognisedJSONIsDemotedNotSplit(t *testing.T) {
	t.Parallel()
	truncated := o3ARSourceObject[:400] // a value cut short: not decodable
	for name, src := range map[string]string{
		"ambiguous object":   o3ARSourceObject,
		"truncated object":   truncated,
		"prose then object":  "Found these:\n\n" + o3ARSourceObject + "\n\nLet me know.",
		"scalar-only object": `{"count": 3, "status": "ok"}`,
	} {
		t.Run(name, func(t *testing.T) {
			plan := readFanOutPlan()
			out, items, how, err := ResolveFanOut(plan, 1, src, 50)
			if err != nil || how != FanOutDemotedUnrecognisedJSON {
				t.Fatalf("outcome = %v err %v; want FanOutDemotedUnrecognisedJSON", how, err)
			}
			if items != nil || len(out.Steps) != len(plan.Steps) || out.Steps[1].FanOutOver != nil {
				t.Fatalf("a demoted step appends nothing and is concrete: items %v, %d steps", items, len(out.Steps))
			}
			if ExtractFanOutItems(src) != nil {
				t.Errorf("ExtractFanOutItems must not split JSON into lines: %q", ExtractFanOutItems(src))
			}
			wplan := writeFanOutPlan([]string{"write"}, false)
			if _, _, wh, _ := ResolveFanOut(wplan, 3, src, 50); wh != FanOutDemotedUnstructured {
				t.Errorf("write over %s: outcome %v, want FanOutDemotedUnstructured", name, wh)
			}
		})
	}
}

// Defect 2 of the ticket: rows keyed by a field that is none of id/item/name/path/title
// were DROPPED, so an array of three records expanded to zero children with ok=true.
func TestExtractStructuredFanOutItems_ObjectRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"customer_name rows in a wrapper",
			`{"customers_60plus_overdue":[{"customer_name":"Wychwood Clinic","overdue_amount_60plus":20092.11},{"customer_name":"Marlowe Park Pharmacy","overdue_amount_60plus":25602.43}]}`,
			[]string{"Wychwood Clinic", "Marlowe Park Pharmacy"}},
		{"id suffix when no name", `[{"account_id":"CUS-002","n":1},{"account_id":"CUS-004"}]`, []string{"CUS-002", "CUS-004"}},
		{"known key beats suffix", `[{"customer_name":"A","id":"CUS-1"}]`, []string{"CUS-1"}},
		{"no identifying field: whole row", `[{"amount":5,"due":"2026-09-01"}]`, []string{`{"amount":5,"due":"2026-09-01"}`}},
		{"nested wrapper", `{"data":{"customers":[{"customer_name":"A"},{"customer_name":"B"}]}}`, []string{"A", "B"}},
		{"result wrapper with meta", `{"result":{"rows":["x","y"]},"ok":true}`, []string{"x", "y"}},
		{"floats keep their digits", `[20092.11, 3]`, []string{"20092.11", "3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ExtractStructuredFanOutItems(tt.raw)
			if !ok || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %q, %v; want %q", got, ok, tt.want)
			}
		})
	}
}

func TestExtractStructuredFanOutItems_Placement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want []string
		ok   bool
	}{
		{"array on its own line inside prose", "The customers are:\n[\"A\", \"B\"]\nI will look them up.", []string{"A", "B"}, true},
		{"the reply's last list wins", "```json\n[\"x\"]\n```\n```json\n[\"y\"]\n```", []string{"y"}, true},
		{"two lists, none at the end, is ambiguous", "```json\n[\"x\"]\n```\nand\n```json\n[\"y\"]\n```\nDone.", nil, false},
		{"a citation mid-sentence is not a list", "Revenue grew [1] last year.\n- alpha\n- beta", nil, false},
		{"trailing non-list JSON after the only list", "[\"A\",\"B\"]\n\n{\"count\": 2}", []string{"A", "B"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ExtractStructuredFanOutItems(tt.raw)
			if ok != tt.ok || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// Free text with no JSON keeps the read-only line fallback; a citation bracket does
// not make it "JSON".
func TestResolveFanOut_ReadOnlyFreeTextStillSplitsListLines(t *testing.T) {
	t.Parallel()
	src := "Revenue grew [1] last year. The files:\n- a/b.go\n- c/d.go"
	_, items, how, err := ResolveFanOut(readFanOutPlan(), 1, src, 50)
	if err != nil || how != FanOutExpanded || !reflect.DeepEqual(items, []string{"a/b.go", "c/d.go"}) {
		t.Fatalf("outcome %v items %q err %v", how, items, err)
	}
}

func TestSourceHoldsJSON(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{
		o3ARSourceObject:                    true,
		o3ARSourceObject[:300]:              true, // cut short: fragment lines
		"- alpha\n- beta":                   false,
		"See [1] and {braces} in prose.":    false,
		"":                                  false,
		"Result:\n```json\n{\"a\": 1}\n```": true,
	} {
		if got := SourceHoldsJSON(raw); got != want {
			t.Errorf("SourceHoldsJSON(%q) = %v, want %v", strings.ReplaceAll(raw[:min(len(raw), 40)], "\n", `\n`), got, want)
		}
	}
}
