package prompts

import (
	"errors"
	"testing"
)

func TestPairLoadsHashesAndRenders(t *testing.T) {
	p, err := LoadPair("router", "router_input")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Schema()) == 0 {
		t.Fatal("router carries its response schema")
	}
	msg, err := p.Message(map[string]string{"input": "hello"})
	if err != nil || msg != "<input>\nhello\n</input>\n\nClassify the input above." {
		t.Fatalf("msg = %q, err = %v", msg, err)
	}
	if _, err := p.Message(map[string]string{}); !errors.Is(err, ErrSlots) {
		t.Fatalf("a missing slot must be ErrSlots, got %v", err)
	}
	other := MustPair("planner", "planner_input")
	if p.StaticHash() == other.StaticHash() || len(p.StaticHash()) != 64 {
		t.Fatal("pair hashes must be distinct sha256 hex")
	}
	// The pair hash covers the input template too, not only the system block.
	swapped := Pair{System: p.System, Input: other.Input}
	if swapped.StaticHash() == p.StaticHash() {
		t.Fatal("the pair hash ignores the input template")
	}
}

func TestPairRejectsASlottedSystemPrompt(t *testing.T) {
	// router_input has a slot, so it cannot be a cached system block.
	if _, err := LoadPair("router_input", "router_input"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}
