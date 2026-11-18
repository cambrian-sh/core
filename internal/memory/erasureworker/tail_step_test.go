package erasureworker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// recordingTail is a plugin tail step that records what it was handed.
type recordingTail struct {
	name string
	got  []domain.ErasedRecords
	err  error
}

func (r *recordingTail) Step() string { return r.name }
func (r *recordingTail) RunErasureStep(_ context.Context, e domain.ErasedRecords) (int, error) {
	r.got = append(r.got, e)
	return len(e.DocumentIDs), r.err
}

// ENG-1966 (ADR-0137 D7): a plugin tail step runs by its outbox name with what the
// request erased and is marked done; a failing one is retried, never marked done; a
// step whose plugin is absent stays pending as unknown_step; a plugin cannot claim a
// core step name.
func TestRunOnce_PluginTailSteps(t *testing.T) {
	ob := newOutbox(
		domain.ErasureOutboxRow{ID: 1, RequestID: "erase:a", Step: "reactive_dead_letters", Ref: "erase:a"},
		domain.ErasureOutboxRow{ID: 2, RequestID: "erase:a", Step: "flaky", Ref: "erase:a"},
		domain.ErasureOutboxRow{ID: 3, RequestID: "erase:a", Step: "absent_plugin", Ref: "erase:a"},
	)
	ob.erased = map[string]domain.ErasedRecords{"erase:a": {RequestID: "erase:a", DocumentIDs: []string{"doc-1"}, CIDs: []string{"cid-1"}}}
	ok := &recordingTail{name: "reactive_dead_letters"}
	flaky := &recordingTail{name: "flaky", err: errors.New("bbolt busy")}
	hijack := &recordingTail{name: domain.ErasureStepCASDelete}
	w := New(ob, &fakeCAS{}, Config{Poll: time.Hour, MaxAttempts: 2, TailSteps: []domain.ErasureTailStep{ok, flaky, hijack, nil}}, nil)
	for i := 0; i < 3; i++ {
		w.RunOnce(context.Background())
	}
	if !ob.done[1] || len(ok.got) != 1 || ok.got[0].DocumentIDs[0] != "doc-1" || ok.got[0].CIDs[0] != "cid-1" {
		t.Fatalf("tail step: done=%v got=%+v", ob.done[1], ok.got)
	}
	if ob.done[2] || ob.failed[2] != "tail_step_failed" || len(flaky.got) != 2 {
		t.Fatalf("failing tail step: done=%v class=%q runs=%d (want retried MaxAttempts times, never done)", ob.done[2], ob.failed[2], len(flaky.got))
	}
	if ob.done[3] || ob.failed[3] != "unknown_step" {
		t.Fatalf("absent plugin step: done=%v class=%q", ob.done[3], ob.failed[3])
	}
	if _, claimed := w.tails[domain.ErasureStepCASDelete]; claimed {
		t.Fatal("a plugin claimed a core step name")
	}
}

// ENG-1968 (ADR-0137 D7): a tail step whose records live in an external service the
// kernel cannot delete from (ErrErasureNotErasableExternal, wrapped or not) is marked
// processed with the not_erasable_external outcome — once, never retried, never a
// failure.
func TestRunOnce_NotErasableExternalIsTerminal(t *testing.T) {
	ob := newOutbox(domain.ErasureOutboxRow{ID: 1, RequestID: "erase:a", Step: "langfuse_traces", Ref: "erase:a"})
	ext := &recordingTail{name: "langfuse_traces", err: fmt.Errorf("langfuse: no delete API: %w", domain.ErrErasureNotErasableExternal)}
	w := New(ob, &fakeCAS{}, Config{Poll: time.Hour, MaxAttempts: 3, TailSteps: []domain.ErasureTailStep{ext}}, nil)
	st := w.RunOnce(context.Background())
	w.RunOnce(context.Background())
	if st.NotErasable != 1 || st.Failed != 0 || st.Done != 0 {
		t.Fatalf("stats = %+v, want one not-erasable step, no failure", st)
	}
	if !ob.done[1] || ob.outcome[1] != domain.ErasureOutcomeNotErasableExternal || ob.failed[1] != "" {
		t.Fatalf("row: done=%v outcome=%q failed=%q", ob.done[1], ob.outcome[1], ob.failed[1])
	}
	if len(ext.got) != 1 {
		t.Fatalf("step ran %d times, want once (terminal, not retried)", len(ext.got))
	}
}
