package domain

import (
	"errors"
	"strings"
	"testing"
)

func validErasure() ErasureRequest {
	return ErasureRequest{Principal: "agent-a", PrincipalKind: ErasureByAgent, Reason: ErasureReasonUserRequest,
		Selector: ErasureSelector{DocumentIDs: []string{"d2", "d1", "d1"}}}
}

// ADR-0137 D6 / AGENTS.md §2.6: the request id is a function of principal + selector only —
// no clock, no order sensitivity, no reason.
func TestErasureRequestID_DeterministicAndOrderFree(t *testing.T) {
	a := validErasure()
	b := validErasure()
	b.Selector.DocumentIDs = []string{"d1", "d2"}
	b.Reason = ErasureReasonCompliance
	if a.ID() != b.ID() {
		t.Fatalf("same principal+selector must give the same id: %s vs %s", a.ID(), b.ID())
	}
	if !strings.HasPrefix(a.ID(), "erase:") || len(a.ID()) != len("erase:")+32 {
		t.Fatalf("id shape: %q", a.ID())
	}
	for name, mut := range map[string]func(*ErasureRequest){
		"principal":      func(r *ErasureRequest) { r.Principal = "agent-b" },
		"principal kind": func(r *ErasureRequest) { r.PrincipalKind = ErasureByOperator },
		"extra id":       func(r *ErasureRequest) { r.Selector.DocumentIDs = append(r.Selector.DocumentIDs, "d3") },
		"selector kind": func(r *ErasureRequest) {
			r.Selector = ErasureSelector{ChunkIDs: []string{"d1", "d2"}}
		},
		// Two ids joined must not equal one id containing the separator-free concatenation.
		"id boundary": func(r *ErasureRequest) { r.Selector.DocumentIDs = []string{"d1d2"} },
	} {
		c := validErasure()
		mut(&c)
		if c.ID() == a.ID() {
			t.Errorf("%s change kept the id %s", name, a.ID())
		}
	}
}

func TestErasureRequest_Validate(t *testing.T) {
	if err := validErasure().Validate(); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	for name, mut := range map[string]func(*ErasureRequest){
		"no principal":   func(r *ErasureRequest) { r.Principal = " " },
		"bad kind":       func(r *ErasureRequest) { r.PrincipalKind = "human" },
		"bad reason":     func(r *ErasureRequest) { r.Reason = "because I said so" },
		"empty selector": func(r *ErasureRequest) { r.Selector = ErasureSelector{} },
		"empty id":       func(r *ErasureRequest) { r.Selector.EntityRefs = []string{""} },
	} {
		r := validErasure()
		mut(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalidErasureRequest) {
			t.Errorf("%s: want ErrInvalidErasureRequest, got %v", name, err)
		}
	}
}

func TestErasureSelector_NormalizedAndKinds(t *testing.T) {
	s := ErasureSelector{EntityRefs: []string{"e"}, DocumentIDs: []string{"b", "a", "b"}, ThreadIDs: nil}
	n := s.Normalized()
	if strings.Join(n.DocumentIDs, ",") != "a,b" || n.ThreadIDs != nil {
		t.Fatalf("normalized = %+v", n)
	}
	if got := strings.Join(s.Kinds(), ","); got != "document,entity" {
		t.Fatalf("kinds = %q", got)
	}
	if s.Count() != 3 || (ErasureSelector{}).Count() != 0 || !(ErasureSelector{}).IsEmpty() {
		t.Fatalf("count/empty wrong")
	}
}

func TestNothingToEraseError_IsSentinel(t *testing.T) {
	var err error = &NothingToEraseError{EarlierRequestID: "erase:x"}
	if !errors.Is(err, ErrNothingToErase) || !strings.Contains(err.Error(), "erase:x") {
		t.Fatalf("err = %v", err)
	}
	if (&NothingToEraseError{}).Error() != ErrNothingToErase.Error() {
		t.Fatal("bare error text")
	}
}
