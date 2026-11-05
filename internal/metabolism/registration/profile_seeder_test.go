package registration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type fakeManifests map[string]*domain.AgentManifest

func (m fakeManifests) GetManifest(_ context.Context, id string) (*domain.AgentManifest, error) {
	if man, ok := m[id]; ok {
		return man, nil
	}
	return &domain.AgentManifest{}, nil
}

type savedProfile struct {
	agentID, hash string
	embedding     []float32
	profile       domain.AgentProfile
}

type fakeProfiles struct {
	mu       sync.Mutex
	existing map[string]*domain.AgentProfile // agentID:hash → profile
	saved    []savedProfile
}

func (p *fakeProfiles) SaveProfile(_ context.Context, agentID, hash string, emb []float32, prof domain.AgentProfile) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saved = append(p.saved, savedProfile{agentID, hash, emb, prof})
	return nil
}

func (p *fakeProfiles) GetProfile(_ context.Context, agentID, hash string) (*domain.AgentProfile, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.existing[agentID+":"+hash], nil
}

type fakeUpdater struct {
	mu    sync.Mutex
	calls map[string]bool
}

func (u *fakeUpdater) SetProvisional(id string, prov bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.calls == nil {
		u.calls = map[string]bool{}
	}
	u.calls[id] = prov
	return nil
}

type fakeEmbedder struct {
	mu    sync.Mutex
	texts []string
	err   error
}

func (e *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.texts = append(e.texts, text)
	if e.err != nil {
		return nil, e.err
	}
	return []float32{0.1, 0.2, 0.3}, nil
}

type captureBus struct {
	mu     sync.Mutex
	events []domain.DomainEvent
}

func (b *captureBus) Subscribe(string, domain.EventHandler) {}
func (b *captureBus) Publish(e domain.DomainEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e)
	return nil
}

type fakeCards struct{ card *domain.AgentCard }

func (f fakeCards) FetchCard(context.Context, string) (*domain.AgentCard, error) { return f.card, nil }

func newSeeder(man fakeManifests) (*ProfileSeeder, *fakeEmbedder, *fakeProfiles, *fakeUpdater, *captureBus) {
	emb, prof, upd, bus := &fakeEmbedder{}, &fakeProfiles{existing: map[string]*domain.AgentProfile{}}, &fakeUpdater{}, &captureBus{}
	s := NewProfileSeeder(man, emb, prof, upd)
	s.EventBus = bus
	return s, emb, prof, upd, bus
}

// ── tests ────────────────────────────────────────────────────────────────────

// A new cognitive agent becomes routable on the NEUTRAL prior with a vector built
// from its own description — no LLM, no question, no agent boot (the seeder has no
// generator or agent caller to call).
func TestSeed_NewCognitiveAgentGetsNeutralPriorAndDescriptionVector(t *testing.T) {
	agent := domain.AgentDefinition{ID: "calc", SourceHash: "h1", Description: "Evaluates arithmetic expressions.", Provisional: true}
	s, emb, prof, upd, bus := newSeeder(fakeManifests{"calc": {Capabilities: []string{"math"}}})

	if err := s.Seed(t.Context(), agent); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if len(emb.texts) != 1 || emb.texts[0] != agent.Description {
		t.Fatalf("embedded %q, want exactly the description", emb.texts)
	}
	if len(prof.saved) != 1 {
		t.Fatalf("saved %d profiles, want 1", len(prof.saved))
	}
	got := prof.saved[0]
	if got.hash != "h1" || got.profile.SuccessRate != NeutralPrior || got.profile.TrustScore != NeutralPrior || got.profile.Provisional {
		t.Errorf("profile = %+v under hash %q, want neutral 0.5/0.5, not provisional, hash h1", got.profile, got.hash)
	}
	if len(got.embedding) == 0 {
		t.Error("profile saved with no vector; L2 could never see the agent")
	}
	if prov, ok := upd.calls["calc"]; !ok || prov {
		t.Errorf("SetProvisional calls = %v, want calc=false", upd.calls)
	}
	if len(bus.events) != 1 {
		t.Fatalf("events = %d, want one AgentReadyEvent", len(bus.events))
	}
	ev := bus.events[0].(domain.AgentReadyEvent)
	if ev.AgentID != "calc" || ev.TrustScore != NeutralPrior || len(ev.Capabilities) != 1 || ev.Capabilities[0] != "math" {
		t.Errorf("ready event = %+v", ev)
	}
}

// A changed agent (new source hash) starts again from the neutral prior: nothing
// is inherited from the previous version.
func TestSeed_ChangedAgentStartsNeutral(t *testing.T) {
	s, _, prof, _, _ := newSeeder(fakeManifests{})
	prof.existing["calc:old"] = &domain.AgentProfile{SuccessRate: 0.95, TrustScore: 0.9}

	if err := s.Seed(t.Context(), domain.AgentDefinition{ID: "calc", SourceHash: "new", Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if p := prof.saved[0].profile; p.SuccessRate != NeutralPrior || p.TrustScore != NeutralPrior {
		t.Errorf("changed agent profile = %+v, want the neutral prior", p)
	}
}

// Re-seeding the SAME version (boot backfill, missing-vector self-heal) refreshes
// the vector and keeps the outcomes the version already earned.
func TestSeed_SameVersionKeepsEarnedOutcomes(t *testing.T) {
	s, _, prof, _, _ := newSeeder(fakeManifests{})
	prof.existing["calc:h1"] = &domain.AgentProfile{AgentID: "calc", SourceHash: "h1", SuccessRate: 0.8, TrustScore: 0.7, NetworkLatencyMedianMs: 120}

	if err := s.Seed(t.Context(), domain.AgentDefinition{ID: "calc", SourceHash: "h1", Description: "d", Provisional: true}); err != nil {
		t.Fatal(err)
	}
	p := prof.saved[0].profile
	if p.SuccessRate != 0.8 || p.TrustScore != 0.7 || p.NetworkLatencyMedianMs != 120 || p.Provisional {
		t.Errorf("re-seeded profile = %+v, want the earned 0.8/0.7/120ms kept", p)
	}
	if len(prof.saved[0].embedding) == 0 {
		t.Error("re-seed did not refresh the vector")
	}
}

// Tool agents are verified by default (ADR-0033/0051) — unchanged by the deletion.
func TestSeed_ToolAgentVerifiedByDefault(t *testing.T) {
	s, _, prof, _, _ := newSeeder(fakeManifests{})
	if err := s.Seed(t.Context(), domain.AgentDefinition{ID: "fs", SourceHash: "h", Trait: domain.TraitTool, Description: "files"}); err != nil {
		t.Fatal(err)
	}
	if p := prof.saved[0].profile; p.SuccessRate != 1.0 || p.TrustScore != 1.0 {
		t.Errorf("tool profile = %+v, want 1.0/1.0", p)
	}
}

// Model agents get no profile and no vector; they are only marked ready.
func TestSeed_ModelAgentHasNoProfile(t *testing.T) {
	s, emb, prof, upd, _ := newSeeder(fakeManifests{})
	if err := s.Seed(t.Context(), domain.AgentDefinition{ID: "llm:haiku", Trait: domain.TraitModel, Provisional: true}); err != nil {
		t.Fatal(err)
	}
	if len(emb.texts) != 0 || len(prof.saved) != 0 {
		t.Errorf("model agent embedded %v / saved %d profiles, want none", emb.texts, len(prof.saved))
	}
	if prov, ok := upd.calls["llm:haiku"]; !ok || prov {
		t.Errorf("model agent provisional not cleared: %v", upd.calls)
	}
}

// With the embedder down nothing is persisted and the agent stays Provisional
// (still routable on the neutral prior through the Gatekeeper's L2 bypass).
func TestSeed_EmbedderDownLeavesAgentProvisional(t *testing.T) {
	s, emb, prof, upd, bus := newSeeder(fakeManifests{})
	emb.err = errors.New("embedder down")
	if err := s.Seed(t.Context(), domain.AgentDefinition{ID: "calc", SourceHash: "h", Description: "d", Provisional: true}); err == nil {
		t.Fatal("Seed succeeded with the embedder down")
	}
	if len(prof.saved) != 0 || len(upd.calls) != 0 || len(bus.events) != 0 {
		t.Errorf("embedder-down seed wrote state: saved=%d updates=%v events=%d", len(prof.saved), upd.calls, len(bus.events))
	}
}

// An A2A agent's vector is built from its Agent Card (description + skills): card
// ingestion, not an interview.
func TestSeed_A2AAgentEmbedsCard(t *testing.T) {
	s, emb, prof, _, _ := newSeeder(fakeManifests{})
	s.CardFetcher = fakeCards{card: &domain.AgentCard{Description: "Remote translator.", Skills: []domain.A2ASkill{{Description: "French"}, {Description: "German"}}}}
	agent := domain.AgentDefinition{ID: "tr", SourceHash: "h", Runtime: domain.RuntimeA2A, A2AEndpoint: "http://x", Description: "short"}
	if err := s.Seed(t.Context(), agent); err != nil {
		t.Fatal(err)
	}
	if want := "Remote translator.\nFrench\nGerman"; emb.texts[0] != want {
		t.Errorf("embedded %q, want %q", emb.texts[0], want)
	}
	if p := prof.saved[0].profile; p.TrustScore != NeutralPrior || p.Provisional {
		t.Errorf("A2A profile = %+v, want neutral and not provisional", p)
	}
}

// An agent with no description still gets a non-empty vector text.
func TestSeed_NoDescriptionFallsBackToCapabilities(t *testing.T) {
	s, emb, _, _, _ := newSeeder(fakeManifests{"x": {Capabilities: []string{"search", "summarise"}}})
	if err := s.Seed(t.Context(), domain.AgentDefinition{ID: "x", SourceHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if emb.texts[0] != "search, summarise" {
		t.Errorf("embedded %q, want the capabilities", emb.texts[0])
	}
}

// The pool drains the queue and every goroutine returns on cancel (goleak in TestMain).
func TestStart_SeedsQueuedAgentsAndStops(t *testing.T) {
	s, _, prof, _, _ := newSeeder(fakeManifests{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	ids := []string{"a1", "a2", "a3"}
	for _, id := range ids {
		s.Enqueue(domain.AgentDefinition{ID: id, SourceHash: "h", Description: "agent " + id, Provisional: true})
	}
	if err := WaitForAgentReadiness(s, ids, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned %v", err)
	}
	prof.mu.Lock()
	defer prof.mu.Unlock()
	if len(prof.saved) != 3 {
		t.Errorf("saved %d profiles, want 3", len(prof.saved))
	}
}

func TestWaitForAgentReadiness_TimesOutNamingMissing(t *testing.T) {
	s := NewProfileSeeder(fakeManifests{}, &fakeEmbedder{}, &fakeProfiles{}, &fakeUpdater{})
	err := WaitForAgentReadiness(s, []string{"never"}, 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}
