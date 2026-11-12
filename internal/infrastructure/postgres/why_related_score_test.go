package postgres

import (
	"math"
	"testing"
	"time"
)

// The ADR-0148 scoring pieces, DB-less (T-20261112-implement-adr-0148-why-
// stored-lineage-default).

func TestHubWeight_IsOneOverLnEPlusDF(t *testing.T) {
	for _, df := range []int{0, 1, 20, 64, 100} {
		want := 1 / math.Log(math.E+float64(df))
		if got := hubWeight(df); math.Abs(got-want) > 1e-12 {
			t.Errorf("df=%d: %v want %v", df, got, want)
		}
	}
	if !(hubWeight(1) > hubWeight(20) && hubWeight(20) > hubWeight(64)) {
		t.Error("weight must fall as a participant gets busier")
	}
	if math.Abs(hubWeight(0)-1) > 1e-12 {
		t.Error("w(0) must be 1")
	}
}

func TestRecencyDecay_TauIsAThirdOfTheWindow(t *testing.T) {
	w := 72 * time.Hour
	if got := recencyDecay(24*time.Hour, w); math.Abs(got-math.Exp(-1)) > 1e-12 {
		t.Errorf("Δt = τ should decay to 1/e, got %v", got)
	}
	if recencyDecay(0, w) != 1 || recencyDecay(-time.Hour, w) != 1 {
		t.Error("no decay for simultaneous events")
	}
	if !(recencyDecay(time.Hour, w) > recencyDecay(10*time.Hour, w)) {
		t.Error("nearer must score higher")
	}
}

func TestCapRelated_OrdersByScoreThenRecencyThenIDAndCaps(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	rows := []relatedRow{
		{fromRef: "a", toRef: "event:3", score: 0.5, occurredAt: t0},
		{fromRef: "a", toRef: "event:1", score: 0.9, occurredAt: t0},
		{fromRef: "a", toRef: "event:2", score: 0.5, occurredAt: t0.Add(time.Minute)},
		{fromRef: "a", toRef: "event:4", score: 0.5, occurredAt: t0},
		{fromRef: "b", toRef: "event:9", score: 0.1, occurredAt: t0},
	}
	got := capRelated(rows, func(h relatedRow) string { return h.fromRef }, 3)
	var order []string
	for _, h := range got["a"] {
		order = append(order, h.toRef)
	}
	if want := "event:1 event:2 event:3"; joinSpace(order) != want {
		t.Fatalf("order %v, want %s", order, want)
	}
	if len(got["b"]) != 1 {
		t.Fatalf("anchor b: %v", got["b"])
	}
}

func joinSpace(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}
