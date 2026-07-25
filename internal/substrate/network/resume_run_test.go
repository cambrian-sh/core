package network

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// memRunStore is an in-memory domain.RunStore.
type memRunStore struct {
	mu   sync.Mutex
	runs map[domain.RunID]domain.Run
}

func newMemRunStore() *memRunStore {
	return &memRunStore{runs: map[domain.RunID]domain.Run{}}
}

func (s *memRunStore) SaveRun(r domain.Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[r.ID] = r
	return nil
}

func (s *memRunStore) GetRun(id domain.RunID) (*domain.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func (s *memRunStore) ListRunsForSession(sid domain.SessionID) ([]domain.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Run
	for _, r := range s.runs {
		if r.SessionID == sid {
			out = append(out, r)
		}
	}
	return out, nil
}

// memCheckpoints is an in-memory, run-keyed checkpoint store that returns checkpoints in
// ascending step order (the guarantee the zero-padded bbolt key provides).
type memCheckpoints struct {
	saved map[domain.RunID]map[int]map[string]string
}

func newMemCheckpoints() *memCheckpoints {
	return &memCheckpoints{saved: map[domain.RunID]map[int]map[string]string{}}
}

func (m *memCheckpoints) SaveCheckpoint(run domain.RunID, step int, ctx map[string]string) error {
	if m.saved[run] == nil {
		m.saved[run] = map[int]map[string]string{}
	}
	m.saved[run][step] = ctx
	return nil
}

func (m *memCheckpoints) LoadCheckpoint(run domain.RunID, step int) (map[string]string, error) {
	return m.saved[run][step], nil
}

func (m *memCheckpoints) ListCheckpoints(run domain.RunID) ([]executer.CheckpointMeta, error) {
	steps := make([]int, 0, len(m.saved[run]))
	for s := range m.saved[run] {
		steps = append(steps, s)
	}
	for i := 1; i < len(steps); i++ { // insertion sort — ascending, like the real key order
		for j := i; j > 0 && steps[j] < steps[j-1]; j-- {
			steps[j], steps[j-1] = steps[j-1], steps[j]
		}
	}
	out := make([]executer.CheckpointMeta, len(steps))
	for i, s := range steps {
		out[i] = executer.CheckpointMeta{RunID: run, StepIndex: s}
	}
	return out, nil
}

func resumeServer(t *testing.T) (*Server, *memRunStore, *memCheckpoints) {
	t.Helper()
	s := minimalServer(t)
	runs := newMemRunStore()
	cps := newMemCheckpoints()
	s.Runs = runs
	s.Checkpoints = cps
	return s, runs, cps
}

func threeStepRun(id domain.RunID, session domain.SessionID) domain.Run {
	return domain.Run{
		ID:        id,
		SessionID: session,
		Status:    domain.RunRunning,
		StartedAt: time.Now(),
		Plan: &domain.ExecutionPlan{
			Subject: "s",
			Steps:   []domain.Step{{Query: "a"}, {Query: "b"}, {Query: "c"}},
		},
	}
}

// Resume replays the run's OWN plan and continues after its last checkpoint. This is the
// property defect 06 lacked: the step index and the plan now come from the same run.
func TestResumeRun_ContinuesFromLatestCheckpointOfItsOwnPlan(t *testing.T) {
	s, runs, cps := resumeServer(t)
	_ = runs.SaveRun(threeStepRun("run-1", "sess-1"))
	_ = cps.SaveCheckpoint("run-1", 0, map[string]string{"step_0": "done"})
	_ = cps.SaveCheckpoint("run-1", 1, map[string]string{"step_0": "done", "step_1": "done"})

	got, err := s.ResumeRun(context.Background(), "run-1", "sess-1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got.StartFrom != 2 {
		t.Errorf("StartFrom = %d, want 2 (after the step-1 checkpoint)", got.StartFrom)
	}
	if got.Plan == nil || len(got.Plan.Steps) != 3 {
		t.Fatalf("resume must replay the run's own plan, got %+v", got.Plan)
	}
	if got.Context["step_1"] != "done" {
		t.Errorf("checkpointed context not restored: %v", got.Context)
	}
}

// No checkpoints yet ⇒ start at the beginning, not at some other run's position.
func TestResumeRun_NoCheckpointsStartsAtZero(t *testing.T) {
	s, runs, _ := resumeServer(t)
	_ = runs.SaveRun(threeStepRun("run-1", "sess-1"))

	got, err := s.ResumeRun(context.Background(), "run-1", "sess-1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got.StartFrom != 0 {
		t.Errorf("StartFrom = %d, want 0", got.StartFrom)
	}
}

// A run cannot be resumed into a different session — that would splice one session's work
// into another's history.
func TestResumeRun_RejectsCrossSessionResume(t *testing.T) {
	s, runs, _ := resumeServer(t)
	_ = runs.SaveRun(threeStepRun("run-1", "sess-1"))

	if _, err := s.ResumeRun(context.Background(), "run-1", "sess-OTHER"); err == nil {
		t.Fatal("resuming a run from another session must fail")
	}
}

func TestResumeRun_RejectsUnknownRun(t *testing.T) {
	s, _, _ := resumeServer(t)
	if _, err := s.ResumeRun(context.Background(), "nope", "sess-1"); err == nil {
		t.Fatal("resuming an unknown run must fail, not silently start a fresh one")
	}
}

// A finished run is terminal.
func TestResumeRun_RejectsCompletedRun(t *testing.T) {
	s, runs, _ := resumeServer(t)
	run := threeStepRun("run-1", "sess-1")
	run.Status = domain.RunCompleted
	_ = runs.SaveRun(run)

	if _, err := s.ResumeRun(context.Background(), "run-1", "sess-1"); err == nil {
		t.Fatal("resuming a completed run must fail")
	}
}

// A run persisted without its plan cannot be resumed — a step index with no steps to index
// into is exactly the unsound state this phase removes.
func TestResumeRun_RejectsRunWithoutPlan(t *testing.T) {
	s, runs, _ := resumeServer(t)
	_ = runs.SaveRun(domain.Run{ID: "run-1", SessionID: "sess-1", Status: domain.RunRunning})

	if _, err := s.ResumeRun(context.Background(), "run-1", "sess-1"); err == nil {
		t.Fatal("a run with no persisted plan must not be resumable")
	}
}

// StartFrom is clamped to the plan length, so a checkpoint at the final step cannot produce
// an out-of-range start.
func TestResumeRun_ClampsStartToPlanLength(t *testing.T) {
	s, runs, cps := resumeServer(t)
	_ = runs.SaveRun(threeStepRun("run-1", "sess-1"))
	_ = cps.SaveCheckpoint("run-1", 2, map[string]string{"done": "all"})

	got, err := s.ResumeRun(context.Background(), "run-1", "sess-1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got.StartFrom != 3 {
		t.Errorf("StartFrom = %d, want 3 (clamped to plan length)", got.StartFrom)
	}
}
