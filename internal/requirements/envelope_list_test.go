package requirements

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261112-d6-rejects-a-list-value-sent-as-a-json-array.

// A list sent as a string holding a JSON array (what Haiku sends: the worker tool's
// value field has no JSON type) is accepted as that array.
func TestValidateEnvelope_ListSentAsJSONArrayStringIsAccepted(t *testing.T) {
	list := `[{"SKU": "VMS-PCH-100", "price": 12.5}, {"SKU": "VMS-PCH-200", "price": 14}]`
	env := &domain.AnswerEnvelope{ItemResults: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied,
		Values: []domain.EnvelopeValue{val("prices", domain.ValueList, list, "led_tool")}}}}
	got, vs := ValidateEnvelope(env, items[:1], evidence())
	if len(vs) != 0 {
		t.Fatalf("violations: %+v", vs)
	}
	if len(got) != 1 || len(got[0].Values) != 1 {
		t.Fatalf("results = %+v", got)
	}
	var arr []map[string]any
	if err := json.Unmarshal(got[0].Values[0].Value, &arr); err != nil || len(arr) != 2 || arr[0]["SKU"] != "VMS-PCH-100" {
		t.Fatalf("the accepted value must be the parsed array, got %s (%v)", got[0].Values[0].Value, err)
	}
}

// Only a list is ever parsed from a string: a text value that looks like JSON
// stays text, and a list string that is not a JSON array stays refused.
func TestNormaliseListValue_OnlyListsAndOnlyJSONArrays(t *testing.T) {
	for _, tc := range []struct {
		typ, value string
		changed    bool
	}{
		{domain.ValueList, `["a","b"]`, true},
		{domain.ValueList, `  [1, 2] `, true},
		{domain.ValueList, `a, b, c`, false},
		{domain.ValueList, `{"a":1}`, false},
		{domain.ValueList, `[not json`, false},
		{domain.ValueString, `["a","b"]`, false},
		{domain.ValueObject, `{"a":1}`, false},
		{domain.ValueCitation, `[1]`, false},
	} {
		v := val("x", tc.typ, tc.value, "led_tool")
		nv, changed := domain.NormaliseListValue(v)
		if changed != tc.changed {
			t.Fatalf("%s %q: changed=%v want %v", tc.typ, tc.value, changed, tc.changed)
		}
		if !changed && string(nv.Value) != string(v.Value) {
			t.Fatalf("%s %q: value changed without a normalisation: %s", tc.typ, tc.value, nv.Value)
		}
	}
	// An array already sent as an array is untouched (and valid).
	v := domain.EnvelopeValue{Name: "x", Type: domain.ValueList, Value: json.RawMessage(`["a"]`)}
	if _, changed := domain.NormaliseListValue(v); changed || domain.CheckValueType(v) != "" {
		t.Fatal("a real array must pass as it is")
	}
}

// The repair feedback for a type error names the field's path, the expected type,
// what was received, and a literal corrected example of that field.
func TestValidateEnvelope_TypeViolationIsSpecificAndCarriesAnExample(t *testing.T) {
	env := &domain.AnswerEnvelope{ItemResults: []domain.ItemResult{{Item: "R1", Status: domain.RequirementSatisfied,
		Values: []domain.EnvelopeValue{
			val("customer", domain.ValueID, "CUS-004", "led_tool"),
			val("suppliers", domain.ValueList, "Acme Medical, Borealis GmbH", "led_tool"),
		}}}}
	_, vs := ValidateEnvelope(env, items[:1], evidence())
	if len(vs) != 1 || vs[0].Rule != domain.ViolationBadType {
		t.Fatalf("violations = %+v", vs)
	}
	d := vs[0].Detail
	for _, want := range []string{
		"item_results[0].values[1].value", `"suppliers"`, "type list", "a list must be a JSON array",
		`You sent: "Acme Medical, Borealis GmbH"`, `Send it as: "value": ["Acme Medical","Borealis GmbH"]`,
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("feedback lacks %q:\n%s", want, d)
		}
	}
}

func TestCorrectedExample_PerType(t *testing.T) {
	for _, tc := range []struct {
		v    domain.EnvelopeValue
		want string
	}{
		{val("n", domain.ValueMoney, "about EUR 1.250,50", ""), "1250.5"},
		{domain.EnvelopeValue{Type: domain.ValueList, Value: json.RawMessage(`{"a":1}`)}, `[{"a":1}]`},
		{domain.EnvelopeValue{Type: domain.ValueString, Value: json.RawMessage(`42`)}, `"42"`},
		{val("d", domain.ValueDate, "next Tuesday", ""), `"2026-01-31"`},
	} {
		if got := correctedExample(tc.v); got != tc.want {
			t.Fatalf("%s %s: got %s want %s", tc.v.Type, tc.v.Value, got, tc.want)
		}
	}
	long := strings.Repeat("x", 500)
	if d := badTypeFeedback("p", val("s", domain.ValueList, long, ""), "a list must be a JSON array"); len(d) > 1300 || !strings.Contains(d, "…") {
		t.Fatalf("the received value must be truncated: %d bytes", len(d))
	}
}
