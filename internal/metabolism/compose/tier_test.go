package compose

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
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

// ENG-2096 (owner decision 2026-11-18): the single-agent lane's request
// composition leaves third-party professions out, so one installed third-party
// profile no longer moves every request onto the third-party runtime.
func TestComposeRequest_LeavesThirdPartyProfessionsOut(t *testing.T) {
	logs := captureLogs(t)
	c := tierFixture(t)
	// The outsider alone allows the sandbox tool, so its absence shows the
	// profession's allowlist stayed out (the researcher allows web and fs).
	c.Agents.(fakeAgents).manifests["outsider"] = &domain.AgentManifest{Skills: []string{"summarise"}, ToolAllowlist: []string{"mcp:sandbox/*"}}
	c.Fleet = fakeFleet{profileAgent("researcher"), thirdPartyProfile("outsider")}
	in := &domain.Handoff{Context: map[string]string{}}
	runtime, comp, err := c.ComposeRequest(context.Background(), "lease-req", "x", in)
	if err != nil {
		t.Fatal(err)
	}
	if runtime != "composed_worker" || comp.Runtime != runtime {
		t.Fatalf("runtime = %q (composition %q), want the shared runtime", runtime, comp.Runtime)
	}
	if !reflect.DeepEqual(comp.Professions, []string{"researcher"}) {
		t.Fatalf("professions = %v, want the third-party profession left out", comp.Professions)
	}
	for _, tool := range comp.Tools {
		if tool == "mcp:sandbox/run" {
			t.Fatalf("the left-out profession's allowlist reached the composition: %v", comp.Tools)
		}
	}
	if !strings.Contains(logs.String(), "third-party professions left out") || !strings.Contains(logs.String(), "outsider") {
		t.Fatalf("the left-out professions must be logged by name; logs: %s", logs.String())
	}
	// Only third-party professions: nothing to compose a request from, and the
	// refusal names them.
	c.Fleet = fakeFleet{thirdPartyProfile("outsider")}
	_, _, err = c.ComposeRequest(context.Background(), "lease-req2", "x", nil)
	if !errors.Is(err, ErrNoProfessions) || !strings.Contains(err.Error(), "outsider") {
		t.Fatalf("only third-party professions: err = %v, want ErrNoProfessions naming outsider", err)
	}
	if _, bound := c.Leases.For("lease-req2"); bound {
		t.Fatal("a refused request composition was bound to the lease")
	}
}

// The exclusion does not depend on a third-party runtime being configured: a
// request composition never needs it.
func TestComposeRequest_NeedsNoThirdPartyRuntime(t *testing.T) {
	c := tierFixture(t)
	c.ThirdPartyRuntimeID = ""
	c.Fleet = fakeFleet{profileAgent("researcher"), thirdPartyProfile("outsider")}
	if runtime, _, err := c.ComposeRequest(context.Background(), "lease-req3", "x", nil); err != nil || runtime != "composed_worker" {
		t.Fatalf("runtime=%q err=%v, want the shared runtime", runtime, err)
	}
}

// A pinned plan step is still the way a third-party profession runs: the same
// composer, with the request lane excluding it, puts a pinned third-party step on
// the third-party runtime.
func TestComposeRequest_PinnedThirdPartyStepStillRunsOnTheThirdPartyRuntime(t *testing.T) {
	c := tierFixture(t)
	c.Fleet = fakeFleet{profileAgent("researcher"), thirdPartyProfile("outsider")}
	if runtime, _, err := c.ComposeRequest(context.Background(), "lease-lane", "x", nil); err != nil || runtime != "composed_worker" {
		t.Fatalf("request: runtime=%q err=%v", runtime, err)
	}
	pinned := domain.ScoredCandidate{Agent: thirdPartyProfile("outsider"), Score: 1}
	rt, comp, ok, err := c.Compose(context.Background(), &domain.DispatchTask{ID: "t5", Description: "x", PreferredAgent: "outsider", AgentPin: domain.PinHard},
		pinned, []domain.ScoredCandidate{pinned}, stepHandoff("lease-pin"))
	if err != nil || !ok || rt.Agent.ID != "composed_worker_third_party" || comp.Runtime != "composed_worker_third_party" {
		t.Fatalf("pinned step: rt=%q comp=%q ok=%v err=%v, want the third-party runtime", rt.Agent.ID, comp.Runtime, ok, err)
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
	if _, err := c.ComposeTurnRequest(context.Background(), "lease-turn2", "x"); !errors.Is(err, ErrNoProfessions) {
		t.Fatalf("only third-party professions: err = %v, want ErrNoProfessions", err)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLogs routes slog to a buffer for the test.
func captureLogs(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}
