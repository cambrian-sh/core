package domain

import (
	"testing"
	"time"
)

func TestExplorationBudget_BoundsAndWindow(t *testing.T) {
	now := time.Now()
	b := NewExplorationBudget(2, time.Hour)
	b.now = func() time.Time { return now }

	if !b.Allowed("browser") {
		t.Fatal("fresh budget should allow")
	}
	b.RecordWin("browser")
	if !b.Allowed("browser") {
		t.Fatal("1/2 wins should still allow")
	}
	b.RecordWin("browser")
	if b.Allowed("browser") {
		t.Fatal("2/2 wins should be exhausted")
	}
	// A different capability is independent.
	if !b.Allowed("pdf") {
		t.Fatal("a different capability must not be affected")
	}
	// Roll the window forward past the two wins → allowed again.
	now = now.Add(2 * time.Hour)
	if !b.Allowed("browser") {
		t.Fatal("after the window rolls over, exploration should be allowed again")
	}
}

func TestExplorationBudget_OnExhaustedFiresOnce(t *testing.T) {
	now := time.Now()
	fired := 0
	b := NewExplorationBudget(1, time.Hour)
	b.now = func() time.Time { return now }
	b.OnExhausted = func(string) { fired++ }

	b.RecordWin("x") // 1/1 → exhausted, fires
	b.RecordWin("x") // still exhausted, must NOT fire again within the window
	if fired != 1 {
		t.Fatalf("OnExhausted should fire exactly once per exhaustion, got %d", fired)
	}
}

func TestExplorationBudget_NilAndDisabledAlwaysAllow(t *testing.T) {
	var nilB *ExplorationBudget
	if !nilB.Allowed("x") {
		t.Fatal("nil budget must always allow")
	}
	nilB.RecordWin("x") // no panic

	disabled := NewExplorationBudget(0, time.Hour) // n<=0 disables
	if !disabled.Allowed("x") {
		t.Fatal("disabled budget must always allow")
	}
}
