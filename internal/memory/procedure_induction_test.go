package memory

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func ep(id, trigger string, caps []string, ok bool, tags ...string) EpisodeShape {
	return EpisodeShape{ExperienceID: id, Trigger: trigger, Capabilities: caps, Succeeded: ok, Tags: tags}
}

// Recurrence is the whole promotion rule: one occurrence is a coincidence, not a routine.
func TestInduceCandidates_RequiresRecurrence(t *testing.T) {
	caps := []string{"read_document", "summarise"}
	one := InduceCandidates([]EpisodeShape{ep("e1", "summarise a report", caps, true, "internal")}, 2)
	if len(one) != 0 {
		t.Errorf("a single episode must not become a procedure, got %+v", one)
	}
	two := InduceCandidates([]EpisodeShape{
		ep("e1", "summarise a report", caps, true, "internal"),
		ep("e2", "summarise a report", caps, true, "internal"),
	}, 2)
	if len(two) != 1 || two[0].SampleCount != 2 {
		t.Fatalf("two matching episodes must induce one candidate, got %+v", two)
	}
}

// Only successes are inducible: a procedure describes what works. Failures are already
// precedents, and inducing from them would produce a reusable recipe for failing.
func TestInduceCandidates_IgnoresFailures(t *testing.T) {
	caps := []string{"deploy"}
	got := InduceCandidates([]EpisodeShape{
		ep("e1", "deploy the service", caps, false, "internal"),
		ep("e2", "deploy the service", caps, false, "internal"),
	}, 2)
	if len(got) != 0 {
		t.Errorf("failures must not induce a procedure, got %+v", got)
	}
}

// Either key alone over-groups, so both must participate.
func TestInduceCandidates_NeedsBothShapeAndSituation(t *testing.T) {
	sameShapeDifferentSituation := InduceCandidates([]EpisodeShape{
		ep("e1", "summarise a report", []string{"read_document"}, true, "internal"),
		ep("e2", "audit a contract", []string{"read_document"}, true, "internal"),
	}, 2)
	if len(sameShapeDifferentSituation) != 0 {
		t.Errorf("identical capabilities in unrelated situations are not one routine, got %+v",
			sameShapeDifferentSituation)
	}
	sameSituationDifferentShape := InduceCandidates([]EpisodeShape{
		ep("e1", "summarise a report", []string{"read_document"}, true, "internal"),
		ep("e2", "summarise a report", []string{"web_search", "summarise"}, true, "internal"),
	}, 2)
	if len(sameSituationDifferentShape) != 0 {
		t.Errorf("the same situation solved differently is not one routine, got %+v",
			sameSituationDifferentShape)
	}
}

// ADR-0095 D9: derivation across a classification boundary is REFUSED, not unioned.
func TestInduceCandidates_RefusesCrossBoundaryInduction(t *testing.T) {
	caps := []string{"read_document"}
	got := InduceCandidates([]EpisodeShape{
		ep("e1", "summarise a report", caps, true, "internal"),
		ep("e2", "summarise a report", caps, true, "internal", "airline"),
	}, 2)
	if len(got) != 0 {
		t.Errorf("induction must refuse a mixed-classification cluster rather than "+
			"laundering the restricted member, got %+v", got)
	}
}

// The load-bearing ADR-0094 D2 invariant: a routine names capabilities, never agents.
// A procedure that named agents would be a learned hardcoded routing table.
func TestToProcedure_NamesCapabilitiesNotAgents(t *testing.T) {
	c := InduceCandidates([]EpisodeShape{
		ep("e1", "ship a release", []string{"build", "deploy"}, true, "internal"),
		ep("e2", "ship a release", []string{"build", "deploy"}, true, "internal"),
	}, 2)[0]
	p := c.ToProcedure("proc-1", []string{"compile the artefact", "roll it out"})

	knownAgents := map[string]bool{"code_generator_agent": true, "deployer_agent": true}
	if p.NamesAnyAgent(knownAgents) {
		t.Error("a procedure step must never name an agent — that is a routing table")
	}
	if len(p.Steps) != 2 || p.Steps[0].RequiredCapabilities[0] != "build" {
		t.Errorf("steps must carry the capability sequence, got %+v", p.Steps)
	}
	if p.Status != domain.ProcedureActive || p.SampleCount != 2 {
		t.Errorf("induced procedure should be active with its sample count, got %+v", p)
	}
	if len(p.SourceExperiences) != 2 {
		t.Errorf("provenance must be retained so cross-boundary checks stay queryable, got %+v",
			p.SourceExperiences)
	}
}

// A batch pass that produced different procedures each night would be unusable.
func TestInduceCandidates_IsDeterministic(t *testing.T) {
	eps := []EpisodeShape{
		ep("e1", "b situation", []string{"x"}, true, "internal"),
		ep("e2", "b situation", []string{"x"}, true, "internal"),
		ep("e3", "a situation", []string{"y"}, true, "internal"),
		ep("e4", "a situation", []string{"y"}, true, "internal"),
		ep("e5", "a situation", []string{"y"}, true, "internal"),
	}
	first := InduceCandidates(eps, 2)
	for i := 0; i < 20; i++ {
		again := InduceCandidates(eps, 2)
		if len(again) != len(first) {
			t.Fatalf("candidate count varies between runs: %d vs %d", len(again), len(first))
		}
		for j := range first {
			if again[j].Signature != first[j].Signature || again[j].Trigger != first[j].Trigger {
				t.Fatalf("candidate order varies between runs at %d", j)
			}
		}
	}
	if first[0].SampleCount < first[len(first)-1].SampleCount {
		t.Error("candidates should be ordered by corroboration, strongest first")
	}
}

// TestToProcedure_SeedsConfidenceFromEvidence guards a bug the mock-data lifecycle
// scenario exposed and no unit test had.
//
// Induction clusters only SUCCESSES, so a routine promoted from N episodes has an
// observed record of N-for-N. Leaving Confidence at Go's zero value threw that away: the
// routine's very first SUCCESSFUL use nudged confidence to 0.15, tripped the 0.3
// deprecation floor, and retired a routine that had never once failed.
//
// The lesson is about seams, not arithmetic — induction and the lifecycle each behaved
// correctly in isolation, and the defect lived in what one handed the other.
func TestToProcedure_SeedsConfidenceFromEvidence(t *testing.T) {
	c := InduceCandidates([]EpisodeShape{
		ep("e1", "ship a release", []string{"build", "deploy"}, true, "internal"),
		ep("e2", "ship a release", []string{"build", "deploy"}, true, "internal"),
		ep("e3", "ship a release", []string{"build", "deploy"}, true, "internal"),
	}, 2)[0]
	p := c.ToProcedure("proc-1", nil)

	if p.Confidence <= 0 {
		t.Fatalf("an induced routine must inherit its induction evidence, got %v", p.Confidence)
	}
	// The property that actually matters: surviving its own first success.
	after := ApplyOutcome(p, true, 0.3)
	if after.Status != domain.ProcedureActive {
		t.Errorf("a routine induced from %d successes must not be retired by ANOTHER success "+
			"(confidence %.3f)", p.SampleCount, after.Confidence)
	}
}

// TestInduceCandidates_RerunsDoNotInflateTheCount is the counting half of ADR-0049 A2.9.
//
// A routine seen with four DIFFERENT targets is real evidence of a generalisable shape.
// The same target four times is ONE observation, and counting it four times promotes a
// routine on the strength of a loop repeating itself. Measured on the live store: one
// situation appeared 8 times, which alone would clear min_samples four times over.
func TestInduceCandidates_RerunsDoNotInflateTheCount(t *testing.T) {
	caps := []string{"build", "deploy"}
	rerun := func(id string) EpisodeShape {
		return EpisodeShape{
			ExperienceID: id,
			Trigger:      "goal: ship the billing service | engages: 1 file",
			// Identical identity: same shape, same entity — a rerun.
			RawTrigger:   "goal: ship the billing service | engages: 1 file | on: file:/srv/billing/main.go",
			Capabilities: caps,
			Succeeded:    true,
		}
	}

	got := InduceCandidates([]EpisodeShape{rerun("e1"), rerun("e2"), rerun("e3")}, 2)
	if len(got) != 0 {
		t.Fatalf("three reruns of ONE situation must not promote a routine, got %d: %+v",
			len(got), got)
	}

	// The same shape on DIFFERENT entities is the evidence that does count.
	variant := func(id, path string) EpisodeShape {
		e := rerun(id)
		e.RawTrigger = "goal: ship the billing service | engages: 1 file | on: file:" + path
		return e
	}
	got = InduceCandidates([]EpisodeShape{
		variant("e1", "/srv/billing/main.go"),
		variant("e2", "/srv/billing/api.go"),
		variant("e3", "/srv/billing/main.go"), // a rerun of e1 — provenance, not evidence
	}, 2)
	if len(got) != 1 {
		t.Fatalf("two distinct situations must promote exactly one routine, got %d", len(got))
	}
	if got[0].SampleCount != 2 {
		t.Errorf("SampleCount must count DISTINCT situations (2), got %d", got[0].SampleCount)
	}
	// Provenance still points at every episode, including the rerun.
	if len(got[0].ExperienceIDs) != 3 {
		t.Errorf("every contributing episode must remain as provenance, got %v",
			got[0].ExperienceIDs)
	}
}
