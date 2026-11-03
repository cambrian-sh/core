package storage

import (
	"fmt"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Concurrent plans and the live-plan registry
// (T-20261103-a-plan-s-content-store-gc-evicts-a-concurrently): one plan ending
// must never evict scratch a still-live plan may need.

func mustPut(t *testing.T, cs *BBoltContentStore, data, typ string) domain.CID {
	t.Helper()
	cid, err := cs.Put(bg, []byte(data), typ, nil, "")
	if err != nil {
		t.Fatalf("Put %q: %v", data, err)
	}
	return cid
}

func mustBegin(t *testing.T, cs *BBoltContentStore) domain.PlanLease {
	t.Helper()
	l, err := cs.BeginPlan(bg)
	if err != nil {
		t.Fatalf("BeginPlan: %v", err)
	}
	return l
}

func mustEnd(t *testing.T, cs *BBoltContentStore, l domain.PlanLease) {
	t.Helper()
	if err := cs.EndPlan(bg, l); err != nil {
		t.Fatalf("EndPlan: %v", err)
	}
}

func wantPresent(t *testing.T, cs *BBoltContentStore, cid domain.CID, present bool, what string) {
	t.Helper()
	ok, err := cs.Has(bg, cid)
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if ok != present {
		t.Errorf("%s: present=%v, want %v", what, ok, present)
	}
}

func TestContentStore_EndPlan_NeverEvictsConcurrentPlansScratch(t *testing.T) {
	cs := newTestContentStore(t)
	a := mustBegin(t, cs)
	aStep := mustPut(t, cs, "A step", domain.NodeTypeStepResult)
	b := mustBegin(t, cs)
	bStep := mustPut(t, cs, "B step", domain.NodeTypeStepResult)
	bTool := mustPut(t, cs, "B tool", domain.NodeTypeToolResult)

	mustEnd(t, cs, a)
	// A's own step was written before B began, so no live plan can need it.
	wantPresent(t, cs, aStep, false, "A's step after A ends")
	wantPresent(t, cs, bStep, true, "B's step_result after A ends")
	wantPresent(t, cs, bTool, true, "B's tool_result after A ends")

	// A legacy GC(keep) call from anywhere must honour live plans too.
	if err := cs.GC(bg, nil); err != nil {
		t.Fatalf("GC: %v", err)
	}
	wantPresent(t, cs, bStep, true, "B's step_result after a bare GC(nil)")

	mustEnd(t, cs, b)
	wantPresent(t, cs, bStep, false, "B's step after the last plan ends")
	wantPresent(t, cs, bTool, false, "B's tool after the last plan ends")
}

// Scratch written by the earlier plan after the later one began is shielded
// conservatively (write-order attribution) — and collected once both end.
func TestContentStore_EndPlan_ConservativeForInterleavedWrites(t *testing.T) {
	cs := newTestContentStore(t)
	a := mustBegin(t, cs)
	b := mustBegin(t, cs)
	aLate := mustPut(t, cs, "A wrote this after B began", domain.NodeTypeStepResult)
	mustEnd(t, cs, a)
	wantPresent(t, cs, aLate, true, "scratch written while B was live")
	mustEnd(t, cs, b)
	wantPresent(t, cs, aLate, false, "after both plans end")
}

func TestContentStore_SharedCID_SurvivesUntilLastPlanEnds(t *testing.T) {
	for _, order := range []string{"A ends first", "B ends first"} {
		t.Run(order, func(t *testing.T) {
			cs := newTestContentStore(t)
			a := mustBegin(t, cs)
			shared := mustPut(t, cs, "shared bytes", domain.NodeTypeStepResult)
			b := mustBegin(t, cs)
			// B writes the same bytes: a dedup hit on a node written before B
			// began. It must be re-stamped, or A's EndPlan would evict it.
			if again := mustPut(t, cs, "shared bytes", domain.NodeTypeToolResult); again != shared {
				t.Fatalf("dedup: %s != %s", again, shared)
			}
			first, last := a, b
			if order == "B ends first" {
				first, last = b, a
			}
			mustEnd(t, cs, first)
			wantPresent(t, cs, shared, true, "shared CID after the first plan ends")
			mustEnd(t, cs, last)
			wantPresent(t, cs, shared, false, "shared CID after the last plan ends")
		})
	}
}

func TestContentStore_EndPlan_NeverEvictsDurableTypes(t *testing.T) {
	cs := newTestContentStore(t)
	a := mustBegin(t, cs)
	ev := mustPut(t, cs, "evidence body", "evidence")
	// Scratch first, then a durable writer of the same bytes: promoted, not re-stamped.
	promoted := mustPut(t, cs, "promoted bytes", domain.NodeTypeStepResult)
	mustPut(t, cs, "promoted bytes", "source_document")
	mustEnd(t, cs, a)
	wantPresent(t, cs, ev, true, "durable evidence after EndPlan")
	wantPresent(t, cs, promoted, true, "promoted node after EndPlan")
	n, err := cs.Get(bg, promoted)
	if err != nil || n.Type != "source_document" {
		t.Fatalf("promoted node: %+v %v", n, err)
	}
}

// Many plans beginning, writing (own and shared bytes) and ending concurrently:
// every plan can read all of its own CIDs right before it ends, and nothing
// scratch is left afterwards. Run with -race.
func TestContentStore_ConcurrentPlans_RaceAndCompleteness(t *testing.T) {
	cs := newTestContentStore(t)
	const plans, writes = 8, 20
	var wg sync.WaitGroup
	errs := make(chan error, plans)
	var all sync.Map
	for p := 0; p < plans; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			lease, err := cs.BeginPlan(bg)
			if err != nil {
				errs <- err
				return
			}
			var mine []domain.CID
			for w := 0; w < writes; w++ {
				data := fmt.Sprintf("plan %d write %d", p, w)
				if w%3 == 0 {
					data = fmt.Sprintf("shared write %d", w) // deduped across plans
				}
				typ := domain.NodeTypeStepResult
				if w%2 == 1 {
					typ = domain.NodeTypeToolResult
				}
				cid, err := cs.Put(bg, []byte(data), typ, nil, "")
				if err != nil {
					errs <- err
					return
				}
				mine = append(mine, cid)
				all.Store(cid, true)
			}
			for _, c := range mine {
				if ok, _ := cs.Has(bg, c); !ok {
					errs <- fmt.Errorf("plan %d: own live CID %s evicted by another plan", p, c)
					return
				}
			}
			if err := cs.EndPlan(bg, lease); err != nil {
				errs <- err
			}
		}(p)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	all.Range(func(k, _ any) bool {
		if ok, _ := cs.Has(bg, k.(domain.CID)); ok {
			t.Errorf("scratch %s left after every plan ended", k)
		}
		return true
	})
}
