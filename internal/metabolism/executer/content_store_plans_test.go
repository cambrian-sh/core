package executer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261103-a-plan-s-content-store-gc-evicts-a-concurrently: plan A finishing
// must not evict concurrently running plan B's step_result / tool_result
// scratch; after both finish, all of it is gone.
func TestDAGExecutor_ConcurrentPlans_GCKeepsLivePlansScratch(t *testing.T) {
	cs := newTestStore(t)

	bStarted := make(chan struct{})
	aDone := make(chan struct{})
	bChecked := make(chan error, 1)

	var bStep0CID, bToolCID, sharedCID domain.CID

	// Plan A: one step, starts after B is live, writes its own bytes plus bytes
	// B also writes (a CID shared through dedup), and a tool_result.
	planA := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "a0"}}}
	stepA := StepFunc(func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		<-bStarted
		if _, err := cs.Put(ctx, []byte("A tool output"), domain.NodeTypeToolResult, nil, ""); err != nil {
			return nil, err
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("shared result")}}, nil
	})

	// Plan B: step 0 writes its result, a tool_result and the shared bytes; step 1
	// (depends on step 0) waits until A has ended, then checks B's scratch.
	planB := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "b0"}, {Query: "b1", DependsOn: []int{0}}}}
	stepB := StepFunc(func(ctx context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
		if i == 0 {
			var err error
			if bToolCID, err = cs.Put(ctx, []byte("B tool output"), domain.NodeTypeToolResult, nil, ""); err != nil {
				return nil, err
			}
			if sharedCID, err = cs.Put(ctx, []byte("shared result"), domain.NodeTypeToolResult, nil, ""); err != nil {
				return nil, err
			}
			bStep0CID = domain.CID(cidOf("B step 0 result"))
			close(bStarted)
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("B step 0 result")}}, nil
		}
		<-aDone
		var missing []string
		for name, c := range map[string]domain.CID{"B step_0 result": bStep0CID, "B tool_result": bToolCID, "shared CID": sharedCID} {
			if ok, _ := cs.Has(ctx, c); !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			bChecked <- fmt.Errorf("plan A's GC evicted plan B's live scratch: %v", missing)
		} else {
			bChecked <- nil
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("B step 1 result")}}, nil
	})

	bErr := make(chan error, 1)
	go func() {
		_, err := (&DAGExecutor{ContentStore: cs}).Execute(t.Context(), planB, nil, stepB)
		bErr <- err
	}()
	<-bStarted // B is registered (its lease began before step 0 ran)
	if _, err := (&DAGExecutor{ContentStore: cs}).Execute(t.Context(), planA, nil, stepA); err != nil {
		t.Fatalf("plan A: %v", err)
	}
	close(aDone)
	if err := <-bErr; err != nil {
		t.Fatalf("plan B: %v", err)
	}
	if err := <-bChecked; err != nil {
		t.Error(err)
	}

	// After both ended, every scratch node is gone — including the plans' own
	// step results, which GC(keep = planCIDs) used to retain until the next plan.
	for _, data := range []string{"A tool output", "B tool output", "shared result", "B step 0 result", "B step 1 result"} {
		if ok, _ := cs.Has(bg, domain.CID(cidOf(data))); ok {
			t.Errorf("scratch %q left after both plans ended", data)
		}
	}
}

// A resumed plan re-Puts its completed steps' results, so a dependent step's
// ContextRef CID resolves even after the earlier execution's GC evicted it.
func TestDAGExecutor_Resume_RePutsCompletedStepResults(t *testing.T) {
	cs := newTestStore(t)
	cid := cidOf("step 0 output") // never stored: as if an earlier GC evicted it
	if ok, _ := cs.Has(bg, domain.CID(cid)); ok {
		t.Fatal("precondition: cid must be absent")
	}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "s0"}, {Query: "s1", DependsOn: []int{0}}}}
	initial := map[string]string{"step_0_result": "step 0 output", "step_0_cid": cid}
	var resolved atomic.Bool
	stepFn := StepFunc(func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		ok, _ := cs.Has(ctx, domain.CID(cid))
		resolved.Store(ok)
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("step 1 output")}}, nil
	})
	if _, err := (&DAGExecutor{ContentStore: cs}).ExecuteFrom(t.Context(), plan, initial, stepFn, 1); err != nil {
		t.Fatalf("ExecuteFrom: %v", err)
	}
	if !resolved.Load() {
		t.Error("resumed step 0's cid must resolve while the resumed plan runs")
	}
}

// cidOf is the CID the content store assigns to data (hex SHA-256, ADR-0022).
func cidOf(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}
