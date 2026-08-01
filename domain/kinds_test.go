package domain

import (
	"strings"
	"testing"
	"time"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func f(v float64) *float64 { return &v }

func sensorSpec() KindSpec {
	return KindSpec{
		Kind: "sensor_reading",
		Predicates: map[string]ValueSpec{
			"temperature_c": {Type: "number", Min: f(-50), Max: f(150)},
			"sensor":        {Type: "entity"},
		},
	}
}

func TestRegistry_DeclaredKindValidates(t *testing.T) {
	r, err := NewKindRegistry([]KindSpec{sensorSpec()}, nil)
	if err != nil {
		t.Fatalf("NewKindRegistry: %v", err)
	}
	ok := []StatementValue{{Predicate: "temperature_c", Type: "number", Number: 22.5}}
	if err := r.ValidateValues("sensor_reading", ok); err != nil {
		t.Fatalf("in-range value refused: %v", err)
	}
	cases := map[string][]StatementValue{
		"above max":            {{Predicate: "temperature_c", Type: "number", Number: 999}},
		"below min":            {{Predicate: "temperature_c", Type: "number", Number: -80}},
		"wrong type":           {{Predicate: "temperature_c", Type: "text", Text: "warm"}},
		"undeclared predicate": {{Predicate: "humidity", Type: "number", Number: 40}},
	}
	for name, vals := range cases {
		if err := r.ValidateValues("sensor_reading", vals); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
	// The refusal NAMES the constraint — an unexplained refusal is unappealable.
	err = r.ValidateValues("sensor_reading",
		[]StatementValue{{Predicate: "temperature_c", Type: "number", Number: 999}})
	if err == nil || !strings.Contains(err.Error(), "150") {
		t.Fatalf("refusal must name the declared maximum, got %v", err)
	}
}

// ADR-0110 D2: adoption is monotonic — an undeclared kind passes untouched.
func TestRegistry_UndeclaredKindPasses(t *testing.T) {
	r, _ := NewKindRegistry([]KindSpec{sensorSpec()}, nil)
	if err := r.ValidateValues("commitment",
		[]StatementValue{{Predicate: "anything", Type: "text", Text: "x"}}); err != nil {
		t.Fatalf("undeclared kind must pass: %v", err)
	}
	var nilReg *KindRegistry
	if err := nilReg.ValidateValues("any", nil); err != nil {
		t.Fatal("nil registry must pass everything")
	}
}

func TestRegistry_BootRefusals(t *testing.T) {
	if _, err := NewKindRegistry([]KindSpec{sensorSpec(), sensorSpec()}, nil); err == nil {
		t.Fatal("duplicate kind must fail the boot")
	}
	bad := sensorSpec()
	bad.Predicates["broken"] = ValueSpec{Type: "vector"}
	if _, err := NewKindRegistry([]KindSpec{bad}, nil); err == nil {
		t.Fatal("unknown value type must fail the boot")
	}
	orphan := sensorSpec()
	orphan.Policy = "quorum"
	if _, err := NewKindRegistry([]KindSpec{orphan}, nil); err == nil {
		t.Fatal("a policy nobody registered must fail the boot, never fall back silently")
	}
}

// The extracted interface resolves identically to the function it was
// extracted from — the definition of a refactor.
func TestLatestAssertionAuthority_MatchesTheFunction(t *testing.T) {
	r, _ := NewKindRegistry([]KindSpec{sensorSpec()}, nil)
	a, ok := r.Authority(ResolutionPolicyLatestAssertion)
	if !ok {
		t.Fatal("the default registry must carry latest_assertion")
	}
	items := []KnowledgeItem{
		{Kind: "k", EntityID: "e", AssertedBy: "s", SourceRef: "m1", AssertedAt: mustTime("2026-08-01T09:00:00Z")},
		{Kind: "k", EntityID: "e", AssertedBy: "s", SourceRef: "m2", AssertedAt: mustTime("2026-08-01T10:00:00Z")},
	}
	w1, r1 := a.Resolve(items)
	w2, r2 := ResolveLatestAssertion(items)
	if w1.SourceRef != w2.SourceRef || r1 != r2 {
		t.Fatalf("authority diverges from the function it wraps: %v/%v vs %v/%v", w1, r1, w2, r2)
	}
}
