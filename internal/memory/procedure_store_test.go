package memory

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func proc(status domain.ProcedureStatus, samples int) domain.Procedure {
	return domain.Procedure{
		ID:      "proc-1",
		Trigger: "goal: ship a release | engages: 1 repo",
		Steps: []domain.ProcedureStep{
			{RequiredCapabilities: []string{"build"}, Intent: "compile"},
			{RequiredCapabilities: []string{"deploy"}, Intent: "roll out"},
		},
		SourceExperiences: []string{"exp-1", "exp-2"},
		Tags:              []string{"internal"},
		SampleCount:       samples,
		Status:            status,
	}
}

// The stored doc must embed the TRIGGER, not the steps: you retrieve by situation and
// act on steps. Embedding the steps would match routines to each other rather than to
// the problem in front of the planner.
func TestProcedureDoc_EmbeddingSubjectIsTheTrigger(t *testing.T) {
	p := proc(domain.ProcedureActive, 2)
	d := procedureDoc(p, []float32{0.1, 0.2})
	if d.Summary != "procedure: "+p.Trigger {
		t.Errorf("summary must be the trigger, got %q", d.Summary)
	}
	if d.DocumentType != domain.DocTypeMnemonicProcedure {
		t.Errorf("wrong doc type %q", d.DocumentType)
	}
	// Steps belong in the reconstruction face and metadata, not the retrieval key.
	if !strings.Contains(d.Text, "[build]") || !strings.Contains(d.Text, "[deploy]") {
		t.Errorf("text must carry the capability-tagged steps, got %q", d.Text)
	}
	var steps []domain.ProcedureStep
	raw, _ := d.Metadata["steps"].(string)
	if err := json.Unmarshal([]byte(raw), &steps); err != nil || len(steps) != 2 {
		t.Fatalf("steps must round-trip through metadata: %v %q", err, raw)
	}
	if steps[0].RequiredCapabilities[0] != "build" {
		t.Errorf("capabilities must survive persistence, got %+v", steps[0])
	}
}

// Deprecation must remove a routine from CONTENTION without removing the record. A
// routine that stopped working is evidence, the same way a rejected arm is.
func TestProcedureActivation_DeprecatedSinksButSurvives(t *testing.T) {
	active := procedureActivation(proc(domain.ProcedureActive, 3))
	deprecated := procedureActivation(proc(domain.ProcedureDeprecated, 3))
	superseded := procedureActivation(proc(domain.ProcedureSuperseded, 3))
	if !(deprecated < active) || !(superseded < active) {
		t.Errorf("non-active routines must sink below active: %v/%v vs %v",
			deprecated, superseded, active)
	}
	if deprecated <= 0 {
		t.Error("a deprecated routine must still EXIST (non-zero activation), not be erased")
	}
	// Corroboration raises standing, but capped so volume alone cannot dominate.
	if procedureActivation(proc(domain.ProcedureActive, 100)) > 0.8 {
		t.Error("activation must be capped so a much-repeated routine cannot crowd everything")
	}
}

// ADR-0094 D8: confidence must resist single runs, or continual adaptation erodes what
// already worked (the capability-erosion failure).
func TestApplyOutcome_ResistsSingleRuns(t *testing.T) {
	p := proc(domain.ProcedureActive, 0)
	p = ApplyOutcome(p, true, 0.3) // first observation seeds
	for i := 0; i < 5; i++ {
		p = ApplyOutcome(p, true, 0.3)
	}
	established := p.Confidence

	afterOneFailure := ApplyOutcome(p, false, 0.3)
	if afterOneFailure.Status != domain.ProcedureActive {
		t.Error("one bad run must not deprecate an established routine")
	}
	if drop := established - afterOneFailure.Confidence; drop > 0.2 {
		t.Errorf("a single failure moved confidence by %.2f — too plastic to be stable", drop)
	}
}

// Sustained failure SHOULD deprecate — the guard is against haste, not against learning.
func TestApplyOutcome_SustainedFailureDeprecates(t *testing.T) {
	p := proc(domain.ProcedureActive, 0)
	for i := 0; i < 25; i++ {
		p = ApplyOutcome(p, false, 0.3)
	}
	if p.Status != domain.ProcedureDeprecated {
		t.Errorf("a routine that keeps failing must be deprecated, got %s (conf %.2f)",
			p.Status, p.Confidence)
	}
}

// A single early failure is noise, not a verdict.
func TestApplyOutcome_NoSnapJudgementOnFirstFailure(t *testing.T) {
	p := ApplyOutcome(proc(domain.ProcedureActive, 0), false, 0.3)
	if p.Status != domain.ProcedureActive {
		t.Errorf("first observation must not deprecate, got %s", p.Status)
	}
}

func TestSupersede_RetainsTheOriginal(t *testing.T) {
	old := Supersede(proc(domain.ProcedureActive, 4), "proc-2")
	if old.Status != domain.ProcedureSuperseded || old.SupersededBy != "proc-2" {
		t.Errorf("supersession must link, not delete: %+v", old)
	}
	if old.Trigger == "" || len(old.Steps) == 0 {
		t.Error("the superseded routine must retain its content as evidence")
	}
}
