package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// End-to-end behaviour of the experiential memory loop, driven by MOCK PLAN DATA.
//
// Why this exists: everything in ADR-0049 §A2 and ADR-0094 is implemented and
// default-off, and none of it can be validated on the real store yet — there are ~4435
// corpus chunks against 2 experiential rows, so no benchmark can move. The loop is
// nevertheless fully exercisable with synthetic plans, and that is what this does: it
// runs plausible planner output through the REAL write path, the REAL induction pass and
// the REAL feedback cycle, and asserts what the system ends up remembering.
//
// It is deliberately narrative. Run it with -v and it reads as a description of how the
// system memorises, which is the thing a reader actually wants to know.

// ---------------------------------------------------------------------------
// A store that behaves enough like pgvector for the loop to run.
// ---------------------------------------------------------------------------

type lifecycleStore struct {
	fakeVectorStore
	docs map[string]domain.Document
	// experiences records what the episode-parent writer was asked to persist.
	experiences map[string]domain.Experience
	links       map[string][]string
}

func newLifecycleStore() *lifecycleStore {
	return &lifecycleStore{
		docs:        map[string]domain.Document{},
		experiences: map[string]domain.Experience{},
		links:       map[string][]string{},
	}
}

func (l *lifecycleStore) Save(_ context.Context, doc *domain.Document) error {
	l.docs[doc.ID] = *doc
	return nil
}

func (l *lifecycleStore) GetByID(_ context.Context, id string) (*domain.Document, error) {
	d, ok := l.docs[id]
	if !ok {
		return nil, nil
	}
	return &d, nil
}

// QueryByMetadata is the read the induction scheduler uses. Containment semantics,
// matching the adapter's `@>`.
func (l *lifecycleStore) QueryByMetadata(_ context.Context, filter map[string]string, limit int) ([]domain.Document, error) {
	var out []domain.Document
	for _, d := range l.docs {
		match := true
		for k, v := range filter {
			if got, _ := d.Metadata[k].(string); got != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, d)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (l *lifecycleStore) SaveExperience(_ context.Context, e domain.Experience) error {
	l.experiences[e.ID] = e
	return nil
}

func (l *lifecycleStore) LinkDerivation(_ context.Context, derived string, exps []string) error {
	l.links[derived] = append(l.links[derived], exps...)
	return nil
}

func (l *lifecycleStore) countType(t string) int {
	n := 0
	for _, d := range l.docs {
		if d.DocumentType == t {
			n++
		}
	}
	return n
}

func (l *lifecycleStore) ofType(t string) []domain.Document {
	var out []domain.Document
	for _, d := range l.docs {
		if d.DocumentType == t {
			out = append(out, d)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Mock planner output
// ---------------------------------------------------------------------------

// mockPlan is what a planner would have produced, reduced to what memory sees.
type mockPlan struct {
	id           string
	goal         string
	capabilities []string // the ordered capability contract of its steps
	touches      []string // file paths the plan's tools engaged
	success      bool
	surprise     float64 // |merit expectation - actual|, as A2.3 computes it
	// followedProcedures are the ADR-0094 routines that informed this plan — the
	// provenance that closes the co-evolution loop (D8).
	followedProcedures []string
}

// runPlan drives one mock plan through the REAL write path: tool engagements accrete the
// world model, then the plan completes and writes its outcome record.
func runPlan(t *testing.T, a *Agent, p mockPlan) {
	t.Helper()
	ctx := context.Background()
	for i, path := range p.touches {
		_ = a.RecordToolOutput(ctx, domain.ToolOutputRecord{
			ToolName:     "write_file",
			ArgsJSON:     []byte(fmt.Sprintf(`{"path":%q}`, path)),
			Output:       []byte(`{"ok":1}`),
			IsMutation:   true,
			TaskID:       fmt.Sprintf("step-%d-%s", i, p.id),
			FactEligible: true,
		})
	}
	_ = a.WritePlanScene(ctx, domain.PlanRecord{
		PlanID:             p.id,
		Goal:               p.goal,
		Success:            p.success,
		Surprise:           p.surprise,
		Capabilities:       p.capabilities,
		FollowedProcedures: p.followedProcedures,
	})
}

// textRecordingEmbedder records every text it is asked to embed, so a test can assert not
// just how many embeddings a path made but WHICH ones — several legitimate embeds happen
// during one plan, and a bare call count cannot tell them apart.
type textRecordingEmbedder struct {
	mu    sync.Mutex
	texts []string
}

func (c *textRecordingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.texts = append(c.texts, text)
	return []float32{1, 0, 0}, nil
}

func (c *textRecordingEmbedder) count(text string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.texts {
		if t == text {
			n++
		}
	}
	return n
}

func lifecycleAgent(store *lifecycleStore) *Agent {
	return lifecycleAgentWith(store, &recordingEmbedder{})
}

func lifecycleAgentWith(store *lifecycleStore, emb domain.Embedder) *Agent {
	a := NewAgent(NewMemoryManager(store, emb), nil, 0.7, 5, 3, 64, 0, 0, 0)
	a.RecordOutcomes = true // ADR-0049 A2.2 arm ON for this test
	a.SurpriseFloor = 0.5   // A2.3 failure-precedent gate
	a.ProcedureDeprecateBelow = 0.3
	a.ExperienceStore = store
	return a
}

// ---------------------------------------------------------------------------
// The scenario
// ---------------------------------------------------------------------------

// TestMemoryLifecycle_WhatTheSystemRemembers runs a plausible week of planner activity
// and asserts what survives in memory.
func TestMemoryLifecycle_WhatTheSystemRemembers(t *testing.T) {
	store := newLifecycleStore()
	agent := lifecycleAgent(store)

	// Three occurrences of the SAME routine (build then deploy), plus unrelated work,
	// plus two failures: one expected, one shocking.
	plans := []mockPlan{
		{id: "p1", goal: "ship the billing service", capabilities: []string{"build", "deploy"},
			touches: []string{"/srv/billing/main.go"}, success: true, surprise: 0.1},
		{id: "p2", goal: "ship the billing service", capabilities: []string{"build", "deploy"},
			touches: []string{"/srv/billing/api.go"}, success: true, surprise: 0.05},
		{id: "p3", goal: "ship the billing service", capabilities: []string{"build", "deploy"},
			touches: []string{"/srv/billing/db.go"}, success: true, surprise: 0.2},
		{id: "p4", goal: "summarise the quarterly report", capabilities: []string{"read_document", "summarise"},
			touches: []string{"/docs/q3.md"}, success: true, surprise: 0.1},
		// A failure nobody is surprised by: merit already expected this to go badly.
		{id: "p5", goal: "migrate the legacy exporter", capabilities: []string{"migrate"},
			touches: []string{"/srv/legacy/export.go"}, success: false, surprise: 0.1},
		// A failure that contradicts a confident expectation — the informative one.
		{id: "p6", goal: "ship the billing service", capabilities: []string{"build", "deploy"},
			touches: []string{"/srv/billing/cache.go"}, success: false, surprise: 0.9},
	}
	for _, p := range plans {
		runPlan(t, agent, p)
	}

	t.Log("--- after 6 plans ---")
	t.Logf("episodes (experience parents): %d", len(store.experiences))
	t.Logf("outcome records (scenes):      %d", store.countType(domain.DocTypeMnemonicScene))
	t.Logf("action records:                %d", store.countType(domain.DocTypeMnemonicAction))
	t.Logf("failure precedents:            %d", store.countType(domain.DocTypeNegativeEdge))

	// Every plan that engaged something becomes exactly one EPISODE — the episode is
	// the per-plan fact and is never deduplicated.
	if len(store.experiences) != len(plans) {
		t.Errorf("expected one episode per plan, got %d", len(store.experiences))
	}
	// ADR-0049 A2.9: one outcome record per distinct SITUATION. Every plan here
	// engages a DIFFERENT file, so all six are variants of shapes rather than reruns
	// — six situations, six records. Deduplication must not merge them: variants are
	// exactly the evidence a routine is generalised from. The rerun case is covered by
	// TestSceneDedup_RerunReinforcesRatherThanDuplicates.
	if got := store.countType(domain.DocTypeMnemonicScene); got != len(plans) {
		t.Errorf("expected one outcome record per distinct situation (%d), got %d",
			len(plans), got)
	}

	// A2.3: only the SURPRISING failure earns a precedent. The expected one does not —
	// that is the difference between a surprise gate and a failure gate.
	if got := store.countType(domain.DocTypeNegativeEdge); got != 1 {
		t.Errorf("expected exactly 1 failure precedent (the surprising one), got %d", got)
	}

	// ADR-0095 D4: every episode is born tagged, and none can be born untagged.
	for id, e := range store.experiences {
		if len(e.Tags) == 0 {
			t.Errorf("episode %s was born untagged — the boundary would be inexpressible", id)
		}
		var internal bool
		for _, tag := range e.Tags {
			if tag == domain.TagInternal {
				internal = true
			}
		}
		if !internal {
			t.Errorf("episode %s missing the internal classification: %v", id, e.Tags)
		}
	}

	// A2.3 through A2.9: surprise still scales retrievability AFTER deduplication.
	// p6 was a shocking failure in a situation already seen three times, so it
	// reinforces the existing billing scene rather than creating a fourth — and must
	// raise that scene's activation anyway. If dedup swallowed the surprise, a
	// situation that turned dangerous would stay as unretrievable as when it was
	// routine, which is the opposite of what the gate is for.
	var shocking, routine float64
	for _, d := range store.ofType(domain.DocTypeMnemonicScene) {
		switch d.ID {
		case "scene-p6":
			shocking = d.ActivationStrength
		case "scene-p2":
			routine = d.ActivationStrength
		}
	}
	t.Logf("activation — surprising failure %.2f vs routine success %.2f", shocking, routine)
	if !(shocking > routine) {
		t.Errorf("a surprising episode must start more retrievable: %.2f vs %.2f", shocking, routine)
	}
}

// TestMemoryLifecycle_InducesAndRefinesARoutine continues the story: the repeated shape
// becomes a procedure, and the procedure then learns from how it goes.
func TestMemoryLifecycle_InducesAndRefinesARoutine(t *testing.T) {
	ctx := context.Background()
	store := newLifecycleStore()
	agent := lifecycleAgent(store)

	for i := 1; i <= 3; i++ {
		runPlan(t, agent, mockPlan{
			id: fmt.Sprintf("s%d", i), goal: "ship the billing service",
			capabilities: []string{"build", "deploy"},
			touches:      []string{fmt.Sprintf("/srv/billing/f%d.go", i)},
			success:      true, surprise: 0.1,
		})
	}
	// One-off work that must NOT become a routine.
	runPlan(t, agent, mockPlan{
		id: "one", goal: "rotate the signing key", capabilities: []string{"rotate_secret"},
		touches: []string{"/etc/keys/signing.pem"}, success: true, surprise: 0.1,
	})

	inducer := &ProcedureInducer{Store: store, Embedder: &recordingEmbedder{}, Experience: store, MinSamples: 2}
	docs, _ := store.QueryByMetadata(ctx, map[string]string{"outcome": "success"}, 100)
	episodes := EpisodesFromScenes(docs)
	t.Logf("inducible episodes: %d", len(episodes))

	written, err := inducer.Induce(ctx, episodes)
	if err != nil {
		t.Fatalf("induction failed: %v", err)
	}
	t.Logf("procedures induced: %d", written)

	procs := store.ofType(domain.DocTypeMnemonicProcedure)
	if len(procs) != 1 {
		t.Fatalf("expected exactly 1 routine (the repeated shape), got %d: %+v", len(procs), procs)
	}
	p, _ := procedureFromDoc(procs[0])
	t.Logf("routine learned: %q  shape=%s  from %d episodes",
		p.Trigger, p.CapabilitySignature(), p.SampleCount)

	// D2: the routine must name capabilities, never the agents that ran them.
	if p.CapabilitySignature() != "build>deploy" {
		t.Errorf("routine should capture the capability shape, got %q", p.CapabilitySignature())
	}
	if strings.Contains(procs[0].Text, "_agent") {
		t.Errorf("a routine must not name agents — that is a learned routing table: %q", procs[0].Text)
	}
	// D5/ADR-0095: provenance is recorded, so the D9 boundary check stays a query.
	if len(store.links[p.ID]) != 3 {
		t.Errorf("expected provenance links to all 3 source episodes, got %v", store.links[p.ID])
	}

	// --- the loop closes: the routine now learns from how following it goes ---
	emb := &recordingEmbedder{}
	FeedProcedureOutcome(ctx, store, emb, []string{p.ID}, true, 0.3)
	after, _ := procedureFromDoc(store.docs[p.ID])
	t.Logf("after one success:  confidence=%.3f samples=%d status=%s",
		after.Confidence, after.SampleCount, after.Status)
	if after.SampleCount <= p.SampleCount {
		t.Error("following a routine must be counted against it")
	}

	for i := 0; i < 30; i++ {
		FeedProcedureOutcome(ctx, store, emb, []string{p.ID}, false, 0.3)
	}
	final, _ := procedureFromDoc(store.docs[p.ID])
	t.Logf("after 30 failures:  confidence=%.3f samples=%d status=%s",
		final.Confidence, final.SampleCount, final.Status)
	if final.Status != domain.ProcedureDeprecated {
		t.Errorf("a routine that stops working must retire, got %s", final.Status)
	}
	// Retired, not erased: the record survives as evidence.
	if _, still := store.docs[p.ID]; !still {
		t.Error("a deprecated routine must remain as evidence, not be deleted")
	}
}

// TestMemoryLifecycle_ArmOffRemembersNothing is the control. With the default
// configuration the entire loop must be inert — this is the post-2026-07-18 baseline and
// the arm every A/B measures against.
func TestMemoryLifecycle_ArmOffRemembersNothing(t *testing.T) {
	store := newLifecycleStore()
	agent := NewAgent(NewMemoryManager(store, &recordingEmbedder{}), nil, 0.7, 5, 3, 64, 0, 0, 0)
	agent.ExperienceStore = store // wired, but both arms default false

	runPlan(t, agent, mockPlan{
		id: "p1", goal: "ship the billing service", capabilities: []string{"build", "deploy"},
		touches: []string{"/srv/billing/main.go"}, success: true, surprise: 0.9,
	})

	t.Logf("arm off — episodes:%d scenes:%d actions:%d",
		len(store.experiences),
		store.countType(domain.DocTypeMnemonicScene),
		store.countType(domain.DocTypeMnemonicAction))

	if len(store.experiences) != 0 || len(store.docs) != 0 {
		t.Errorf("with the arm off the system must remember NOTHING, got %d episodes / %d docs",
			len(store.experiences), len(store.docs))
	}
}

// TestSceneDedup_RerunReinforcesRatherThanDuplicates covers what the lifecycle fixture
// deliberately does not: the SAME work done twice.
//
// Measured on the live store before this existed: 141 scenes carried 100 distinct
// projections, one situation appearing 8 times. Each was separately embedded, and —
// the part that actually mattered — induction counted rows, so eight reruns of one
// task read as eight independent confirmations of a routine.
func TestSceneDedup_RerunReinforcesRatherThanDuplicates(t *testing.T) {
	store := newLifecycleStore()
	agent := lifecycleAgent(store)

	// Same goal, same file, three times: one situation, lived three times.
	rerun := mockPlan{
		id: "r1", goal: "ship the billing service", capabilities: []string{"build", "deploy"},
		touches: []string{"/srv/billing/main.go"}, success: true, surprise: 0.1,
	}
	for i, id := range []string{"r1", "r2", "r3"} {
		p := rerun
		p.id = id
		// The third occurrence is the shock: the routine situation goes wrong.
		if i == 2 {
			p.success, p.surprise = false, 0.9
		}
		runPlan(t, agent, p)
	}

	scenes := store.ofType(domain.DocTypeMnemonicScene)
	if len(scenes) != 1 {
		t.Fatalf("three reruns of one situation must leave ONE scene, got %d", len(scenes))
	}

	seen, _ := scenes[0].Metadata["seen_count"].(int)
	if seen != 3 {
		t.Errorf("seen_count must count every occurrence, got %d", seen)
	}
	// A surprising occurrence in a familiar situation is the most decision-relevant
	// thing here. Deduplication must not swallow it, or a situation that turned
	// dangerous stays as unretrievable as when it was routine.
	if got := scenes[0].ActivationStrength; got <= 0.1 {
		t.Errorf("the shocking third occurrence must raise activation, got %.2f", got)
	}
	// Every episode is still its own fact — dedup applies to the RECORD, not the event.
	if len(store.experiences) != 3 {
		t.Errorf("expected one episode per plan even when deduplicated, got %d",
			len(store.experiences))
	}
	// And the failure precedent still fires from a deduplicated occurrence.
	if got := store.countType(domain.DocTypeNegativeEdge); got != 1 {
		t.Errorf("a surprising failure must record a precedent even on a rerun, got %d", got)
	}
}

// TestSceneDedup_RerunDoesNotReembedTheProjection is the COST half of dedup.
//
// Not storing the row twice is only half the saving; the projection embed is the
// expensive call, and it used to run BEFORE the dedup check consumed its result — so
// every rerun of a known situation paid an embedder round-trip for a vector that was
// then discarded. The assertion is deliberately on the projection TEXT rather than a
// total call count: a plan legitimately embeds other things, and a count alone would
// pass for the wrong reason.
func TestSceneDedup_RerunDoesNotReembedTheProjection(t *testing.T) {
	store := newLifecycleStore()
	emb := &textRecordingEmbedder{}
	agent := lifecycleAgentWith(store, emb)

	rerun := mockPlan{
		goal: "ship the billing service", capabilities: []string{"build", "deploy"},
		touches: []string{"/srv/billing/main.go"}, success: true, surprise: 0.1,
	}
	for _, id := range []string{"e1", "e2"} {
		p := rerun
		p.id = id
		runPlan(t, agent, p)
	}

	scenes := store.ofType(domain.DocTypeMnemonicScene)
	if len(scenes) != 1 {
		t.Fatalf("two reruns of one situation must leave ONE scene, got %d", len(scenes))
	}
	// Confirms the second run really took the reinforced path — otherwise the embed
	// assertion below would hold for the uninteresting reason.
	if seen, _ := scenes[0].Metadata["seen_count"].(int); seen != 2 {
		t.Fatalf("second run must have reinforced the scene, seen_count = %d", seen)
	}

	projection, _ := scenes[0].Metadata["projection"].(string)
	if projection == "" {
		t.Fatal("scene carries no projection to assert on")
	}
	if got := emb.count(projection); got != 1 {
		t.Errorf("the scene projection must be embedded exactly once across both runs, got %d", got)
	}
}

// TestProcedureFeedbackLoop_ClosesEndToEnd is the ADR-0094 D8 co-evolution loop.
//
// Before this, PlanRecord.FollowedProcedures was DECLARED in domain and READ here in
// the memory agent — and written by nobody. So `len(rec.FollowedProcedures) > 0` was
// never true, FeedProcedureOutcome never ran, and no routine's confidence ever moved.
// The tier could influence a plan and never learn whether it helped, which is the
// difference between a memory system and a cache with extra steps.
//
// The chain under test: enrichment.Procedures -> plan.FollowedProcedures (planner) ->
// PlanRecord.FollowedProcedures (executor) -> FeedProcedureOutcome (memory agent).
func TestProcedureFeedbackLoop_ClosesEndToEnd(t *testing.T) {
	store := newLifecycleStore()
	agent := lifecycleAgent(store)

	// A routine that has worked so far, with enough evidence to be demotable.
	routine := domain.Procedure{
		ID:          "proc-under-test",
		Trigger:     "goal: ship the billing service | engages: 1 file",
		Steps:       []domain.ProcedureStep{{RequiredCapabilities: []string{"build"}, Intent: "perform: build"}},
		Confidence:  0.6,
		SampleCount: 3,
		Status:      domain.ProcedureActive,
	}
	if err := SaveProcedure(t.Context(), store, &recordingEmbedder{}, routine); err != nil {
		t.Fatalf("seed routine: %v", err)
	}

	// A plan that the routine informed, which then FAILED.
	p := mockPlan{
		id: "pf1", goal: "ship the billing service", capabilities: []string{"build"},
		touches: []string{"/srv/billing/main.go"}, success: false, surprise: 0.2,
	}
	p.followedProcedures = []string{routine.ID}
	runPlan(t, agent, p)

	doc, err := store.GetByID(t.Context(), routine.ID)
	if err != nil || doc == nil {
		t.Fatalf("routine vanished: %v", err)
	}
	got, ok := procedureFromDoc(*doc)
	if !ok {
		t.Fatalf("routine no longer parses: %+v", doc)
	}

	// A failure must LOWER confidence — the whole point of the loop.
	if !(got.Confidence < routine.Confidence) {
		t.Errorf("a failed plan must lower the routine's confidence: %.3f -> %.3f",
			routine.Confidence, got.Confidence)
	}
	if got.SampleCount != routine.SampleCount+1 {
		t.Errorf("the occurrence must be counted: %d -> %d", routine.SampleCount, got.SampleCount)
	}
	t.Logf("confidence %.3f -> %.3f, samples %d -> %d",
		routine.Confidence, got.Confidence, routine.SampleCount, got.SampleCount)
}
