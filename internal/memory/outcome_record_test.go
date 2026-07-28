package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// fakeExperienceStore captures the episode parent rows the agent mints.
type fakeExperienceStore struct {
	saved []domain.Experience
	err   error
}

func (f *fakeExperienceStore) LinkDerivation(_ context.Context, _ string, _ []string) error {
	return nil
}

func (f *fakeExperienceStore) SaveExperience(_ context.Context, e domain.Experience) error {
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, e)
	return nil
}

// TestBornTags_NeverEmpty is ADR-0095 D4's first clause as a test: there is no input
// that yields an untagged experience. An untagged row has no tags for any predicate to
// act on, which is how a boundary becomes inexpressible rather than merely open.
func TestBornTags_NeverEmpty(t *testing.T) {
	for _, surface := range []domain.SurfaceRef{
		{},
		{Kind: domain.SurfaceChat},
		{Kind: domain.SurfaceChat, ID: "airline"},
		{ID: "airline"},
	} {
		tags := domain.BornTags(surface)
		if len(tags) == 0 {
			t.Fatalf("surface %+v produced no tags", surface)
		}
		var hasInternal bool
		for _, tag := range tags {
			if tag == domain.TagInternal {
				hasInternal = true
			}
		}
		if !hasInternal {
			t.Errorf("surface %+v: expected %q among %v", surface, domain.TagInternal, tags)
		}
	}
}

// The surface must reach the tags, or a customer-surface episode is indistinguishable
// from an internal one and ADR-0087's RequiredTags clamp has nothing to clamp on.
func TestBornTags_CarriesSurfaceAndIngress(t *testing.T) {
	tags := domain.BornTags(domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "airline"})
	want := map[string]bool{"internal": false, "surface:chat": false, "ingress:airline": false}
	for _, tag := range tags {
		if _, ok := want[tag]; ok {
			want[tag] = true
		}
	}
	for tag, seen := range want {
		if !seen {
			t.Errorf("missing tag %q in %v", tag, tags)
		}
	}
}

// The arm is default-off, and off must mean NOTHING is written — not "written but
// filtered later". This is the post-2026-07-18 baseline and the control arm of every
// A/B that measures the outcome record.
func TestWritePlanScene_DefaultOffWritesNothing(t *testing.T) {
	store := &fakeExperienceStore{}
	agent := &Agent{ExperienceStore: store} // RecordOutcomes defaults false
	if err := agent.WritePlanScene(context.Background(), domain.PlanRecord{PlanID: "plan-1", Goal: "do a thing", Success: true, Surprise: -1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.saved) != 0 {
		t.Errorf("arm is off; no experience should be minted, got %+v", store.saved)
	}
}

// A nil Manager must not panic — the gate is checked before any dereference.
func TestWritePlanScene_NilManagerIsSafe(t *testing.T) {
	agent := &Agent{RecordOutcomes: true}
	if err := agent.WritePlanScene(context.Background(), domain.PlanRecord{PlanID: "plan-1", Goal: "goal", Success: true, Surprise: -1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// An empty plan id is not an episode.
func TestWritePlanScene_EmptyPlanIDWritesNothing(t *testing.T) {
	store := &fakeExperienceStore{}
	agent := &Agent{RecordOutcomes: true, ExperienceStore: store}
	if err := agent.WritePlanScene(context.Background(), domain.PlanRecord{PlanID: "", Goal: "goal", Success: true, Surprise: -1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.saved) != 0 {
		t.Errorf("empty plan id must mint nothing, got %+v", store.saved)
	}
}

// TestRecordToolOutput_ArmsAreIndependent pins the split that A2.2 needed and the
// original single-flag gating broke.
//
// The failure it guards against is invisible in isolation: with everything behind
// RecordExperiential, turning ON the outcome-record arm produced NOTHING, because the
// engaged-entity accretion that feeds a plan scene never ran, so every scene hit the
// contentless-scene skip. Both flags were set in every existing test, so nothing caught
// it until a live plan execution wrote zero rows.
func TestRecordToolOutput_ArmsAreIndependent(t *testing.T) {
	mutation := domain.ToolOutputRecord{
		ToolName:   "write_file",
		ArgsJSON:   []byte(`{"path":"/tmp/a.md"}`),
		Output:     []byte(`{"ok":1}`),
		IsMutation: true,
		TaskID:     "step-0-plan1",
	}

	t.Run("both off writes nothing", func(t *testing.T) {
		store := &captureSaveStore{}
		agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.7, 5, 3, 64, 0, 0, 0)
		_ = agent.RecordToolOutput(context.Background(), mutation)
		if got := agent.engagedCountForPlan("plan1"); got != 0 {
			t.Errorf("both arms off: expected no engagement, got %d", got)
		}
	})

	t.Run("outcomes arm alone accretes the plan scope", func(t *testing.T) {
		store := &captureSaveStore{}
		agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.7, 5, 3, 64, 0, 0, 0)
		agent.RecordOutcomes = true // RecordExperiential stays FALSE
		_ = agent.RecordToolOutput(context.Background(), mutation)
		if got := agent.engagedCountForPlan("plan1"); got == 0 {
			t.Error("outcome arm on: the plan scene scope must accrete, or WritePlanScene " +
				"skips as contentless and A2.2 silently writes nothing")
		}
	})
}

// multiCaptureStore keeps EVERY saved doc. The shared captureSaveStore keeps only the
// last, which cannot answer "did this plan write a precedent IN ADDITION to its outcome
// record" — the exact question A2.3 turns on.
type multiCaptureStore struct {
	fakeVectorStore
	saved []domain.Document
}

func (m *multiCaptureStore) Save(_ context.Context, doc *domain.Document) error {
	m.saved = append(m.saved, *doc)
	return nil
}

// TestWritePlanScene_SurpriseGatesTheFailurePrecedent covers ADR-0049 A2.3's core
// distinction. An UNSURPRISING failure gets the ordinary outcome record and nothing
// more; a surprising one additionally earns a failure precedent in the negative-edge
// lane. A gate that fired on every failure would be a failure gate, which is exactly
// what A2.3 rejects.
func TestWritePlanScene_SurpriseGatesTheFailurePrecedent(t *testing.T) {
	newAgent := func() (*multiCaptureStore, *Agent) {
		store := &multiCaptureStore{}
		a := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.7, 5, 3, 64, 0, 0, 0)
		a.RecordOutcomes = true
		a.SurpriseFloor = 0.5
		return store, a
	}
	negEdges := func(s *multiCaptureStore) int {
		n := 0
		for _, d := range s.saved {
			if d.DocumentType == domain.DocTypeNegativeEdge {
				n++
			}
		}
		return n
	}
	engage := func(a *Agent, plan string) {
		_ = a.RecordToolOutput(context.Background(), domain.ToolOutputRecord{
			ToolName: "write_file", ArgsJSON: []byte(`{"path":"/tmp/x.md"}`),
			Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "step-0-" + plan,
		})
	}

	t.Run("unsurprising failure writes no precedent", func(t *testing.T) {
		store, a := newAgent()
		engage(a, "p1")
		_ = a.WritePlanScene(context.Background(), domain.PlanRecord{PlanID: "p1", Goal: "routine", Success: false, Surprise: 0.1})
		if n := negEdges(store); n != 0 {
			t.Errorf("a predicted failure must not earn a precedent, got %d", n)
		}
	})

	t.Run("surprising failure writes a precedent", func(t *testing.T) {
		store, a := newAgent()
		engage(a, "p2")
		_ = a.WritePlanScene(context.Background(), domain.PlanRecord{PlanID: "p2", Goal: "shock", Success: false, Surprise: 0.9})
		if n := negEdges(store); n != 1 {
			t.Errorf("an unexpected failure must earn exactly one precedent, got %d", n)
		}
	})

	t.Run("surprising SUCCESS writes no precedent", func(t *testing.T) {
		store, a := newAgent()
		engage(a, "p3")
		_ = a.WritePlanScene(context.Background(), domain.PlanRecord{PlanID: "p3", Goal: "pleasant", Success: true, Surprise: 0.9})
		if n := negEdges(store); n != 0 {
			t.Errorf("a success is not a negative edge however surprising, got %d", n)
		}
	})
}

// Surprise must reach the record as a FIELD and lift its activation, since A2.3 gates
// retrievability rather than existence. Unknown surprise (-1) must take the baseline
// and never be read as "predicted perfectly".
func TestWritePlanScene_SurpriseStampedAndScalesActivation(t *testing.T) {
	run := func(surprise float64) domain.Document {
		store := &multiCaptureStore{}
		a := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.7, 5, 3, 64, 0, 0, 0)
		a.RecordOutcomes = true
		_ = a.RecordToolOutput(context.Background(), domain.ToolOutputRecord{
			ToolName: "write_file", ArgsJSON: []byte(`{"path":"/tmp/y.md"}`),
			Output: []byte(`{"ok":1}`), IsMutation: true, TaskID: "step-0-ps",
		})
		_ = a.WritePlanScene(context.Background(), domain.PlanRecord{PlanID: "ps", Goal: "goal", Success: true, Surprise: surprise})
		for _, d := range store.saved {
			if d.DocumentType == domain.DocTypeMnemonicScene {
				return d
			}
		}
		t.Fatal("no outcome record written")
		return domain.Document{}
	}

	high := run(1.0)
	if got, _ := high.Metadata["surprise"].(float64); got != 1.0 {
		t.Errorf("surprise must be stamped on the record, got %v", high.Metadata["surprise"])
	}
	low := run(0.0)
	unknown := run(-1)
	if !(high.ActivationStrength > low.ActivationStrength) {
		t.Errorf("a surprising episode must start more retrievable: %v vs %v",
			high.ActivationStrength, low.ActivationStrength)
	}
	if unknown.ActivationStrength != low.ActivationStrength {
		t.Errorf("unknown surprise must take the baseline, not be penalised: %v vs %v",
			unknown.ActivationStrength, low.ActivationStrength)
	}
}
