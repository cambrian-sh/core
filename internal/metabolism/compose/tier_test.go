package compose

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2083 (owner decision 2026-11-18, option a): a composition holding a
// third-party profession runs on the SEPARATE third-party runtime, never on the
// shared system-tier one; the lowest tier among the professions decides.

func tierFixture(t *testing.T) *Composer {
	t.Helper()
	c, _ := fixture(t)
	a := c.Agents.(fakeAgents)
	a.defs["composed_worker_third_party"] = domain.AgentDefinition{ID: "composed_worker_third_party", System: true, TrustTier: "third-party"}
	a.manifests["outsider"] = &domain.AgentManifest{Capabilities: []string{"research"}, Skills: []string{"summarise"},
		ToolAllowlist: []string{"mcp:web/*"}}
	c.ThirdPartyRuntimeID = "composed_worker_third_party"
	return c
}

func thirdPartyProfile(id string) domain.AgentDefinition {
	d := profileAgent(id)
	d.TrustTier = "third-party"
	return d
}

func TestCompose_ThirdPartyWinnerRunsOnTheThirdPartyRuntime(t *testing.T) {
	c := tierFixture(t)
	// A hard-pinned step: the gatekeeper's slate is the pinned profession alone.
	pinned := domain.ScoredCandidate{Agent: thirdPartyProfile("outsider"), Score: 1}
	rt, comp, ok, err := c.Compose(context.Background(), &domain.DispatchTask{ID: "t1", Description: "x"},
		pinned, []domain.ScoredCandidate{pinned}, stepHandoff("lease-tp"))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if rt.Agent.ID != "composed_worker_third_party" || comp.Runtime != "composed_worker_third_party" {
		t.Fatalf("runtime = %q / composition runtime %q, want the third-party runtime", rt.Agent.ID, comp.Runtime)
	}
	// A retry on the same lease stays on the composition's runtime.
	rt2, _, ok2, err2 := c.Compose(context.Background(), &domain.DispatchTask{ID: "t1"}, pinned, nil, stepHandoff("lease-tp"))
	if err2 != nil || !ok2 || rt2.Agent.ID != "composed_worker_third_party" {
		t.Fatalf("retry: rt=%q ok=%v err=%v", rt2.Agent.ID, ok2, err2)
	}
}

func TestCompose_MixedCompositionRunsAtTheLowestTier(t *testing.T) {
	c := tierFixture(t)
	task := &domain.DispatchTask{ID: "t2", Description: "x", RequiredCapabilities: []string{"research"}}
	scored := []domain.ScoredCandidate{{Agent: profileAgent("researcher"), Score: 0.9},
		{Agent: thirdPartyProfile("outsider"), Score: 0.5}}
	rt, comp, ok, err := c.Compose(context.Background(), task, scored[0], scored, stepHandoff("lease-mix"))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(comp.Professions, []string{"outsider", "researcher"}) {
		t.Fatalf("professions = %v", comp.Professions)
	}
	if rt.Agent.ID != "composed_worker_third_party" {
		t.Fatalf("a composition with one third-party profession ran on %q", rt.Agent.ID)
	}
}

func TestCompose_FirstPartyStaysOnTheSharedRuntime(t *testing.T) {
	c := tierFixture(t)
	w := domain.ScoredCandidate{Agent: profileAgent("researcher"), Score: 1}
	rt, _, ok, err := c.Compose(context.Background(), &domain.DispatchTask{ID: "t3"}, w, []domain.ScoredCandidate{w}, stepHandoff("lease-fp"))
	if err != nil || !ok || rt.Agent.ID != "composed_worker" {
		t.Fatalf("rt=%q ok=%v err=%v, want the shared runtime", rt.Agent.ID, ok, err)
	}
}

// Fail closed: no third-party runtime configured or registered ⇒ the dispatch
// is refused, never composed onto the shared runtime nor handed to the winner.
func TestCompose_ThirdPartyWithoutItsRuntimeIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*Composer){
		"unconfigured": func(c *Composer) { c.ThirdPartyRuntimeID = "" },
		"unregistered": func(c *Composer) { delete(c.Agents.(fakeAgents).defs, "composed_worker_third_party") },
	} {
		t.Run(name, func(t *testing.T) {
			c := tierFixture(t)
			mutate(c)
			w := domain.ScoredCandidate{Agent: thirdPartyProfile("outsider"), Score: 1}
			rt, _, ok, err := c.Compose(context.Background(), &domain.DispatchTask{ID: "t4"}, w, []domain.ScoredCandidate{w}, stepHandoff("lease-x"))
			if err == nil || ok || !strings.Contains(err.Error(), "third-party") {
				t.Fatalf("want a refusal naming third-party, got rt=%q ok=%v err=%v", rt.Agent.ID, ok, err)
			}
			if _, bound := c.Leases.For("lease-x"); bound {
				t.Fatal("a refused composition was bound to the lease")
			}
		})
	}
}

func TestComposeRequest_ThirdPartyProfessionMovesTheRequest(t *testing.T) {
	c := tierFixture(t)
	c.Fleet = fakeFleet{profileAgent("researcher"), thirdPartyProfile("outsider")}
	runtime, comp, err := c.ComposeRequest(context.Background(), "lease-req", "x", &domain.Handoff{Context: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if runtime != "composed_worker_third_party" || comp.Runtime != runtime {
		t.Fatalf("runtime = %q (composition %q), want the third-party runtime", runtime, comp.Runtime)
	}
	c.Fleet = fakeFleet{profileAgent("researcher")}
	if runtime, _, err := c.ComposeRequest(context.Background(), "lease-req2", "x", nil); err != nil || runtime != "composed_worker" {
		t.Fatalf("first-party only: runtime=%q err=%v", runtime, err)
	}
}

// The chat turn's merged agent cannot move to the third-party runtime: its
// composition leaves third-party professions out.
func TestComposeTurnRequest_LeavesThirdPartyProfessionsOut(t *testing.T) {
	c := tierFixture(t)
	c.Fleet = fakeFleet{profileAgent("researcher"), thirdPartyProfile("outsider")}
	comp, err := c.ComposeTurnRequest(context.Background(), "lease-turn", "x")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(comp.Professions, []string{"researcher"}) || comp.Runtime != "composed_worker" {
		t.Fatalf("turn composition = %+v, want researcher only on the shared runtime", comp)
	}
	c.Fleet = fakeFleet{thirdPartyProfile("outsider")}
	if _, err := c.ComposeTurnRequest(context.Background(), "lease-turn2", "x"); err != ErrNoProfessions {
		t.Fatalf("only third-party professions: err = %v, want ErrNoProfessions", err)
	}
}
