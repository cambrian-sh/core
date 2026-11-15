package memory

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// The two-phase ordered backfill (T-20261103-fact-backfill-processes-each-principal-s)
// against the serial ordered run on the same corpus and the same (deterministic) model:
// the final items, assessments (ids, inputs, verdicts), derived chains, predicate
// profiles and markers must be IDENTICAL.
//
// The corpus is one principal's classification group, built to exercise every way the
// order matters:
//   - home_city changes value in every early document; the judge calls each pair
//     contradictory, so the predicate's learned cardinality flips to single on the
//     THIRD vote (d4) â€” after that the serial run plans no exact-key pairs for it, so
//     the two-phase run's speculation for d5 is wrong and must be redone (a wave);
//   - likes accumulates; the judge calls it consistent, so it flips to multi at d6;
//   - d6 and d7 share a session time (a tie, ordered by the lister);
//   - d8 has two chunks that pack into two windows of ONE document (one asserted_at:
//     the before-rule alone would admit the second window's items to the first's
//     candidates);
//   - fuzzy_candidates is 2, so the top-k of a same-subject read would change if a later
//     document's items were visible;
//   - d9 (the latest) was extracted by an EARLIER run: a stored, out-of-order document;
//   - the lister returns the documents shuffled.

var stagedChunkText = map[string]string{
	"c1":  "I live in Paris and I like jazz.",
	"c2":  "I live in London now and I like rock.",
	"c3":  "I live in Berlin.",
	"c4":  "I live in Rome.",
	"c5":  "I live in Madrid and I like rock.",
	"c6":  "I like pop.",
	"c7":  "I work at Acme.",
	"c8a": "I work at Globex.",
	"c8b": "My job title is engineer.",
	"c9":  "I live in Oslo.",
}

// stagedFacts are the model's extraction answers per chunk: attribute, value, quote.
var stagedFacts = map[string][][3]string{
	"c1":  {{"home_city", "Paris", "I live in Paris"}, {"likes", "jazz", "I like jazz"}},
	"c2":  {{"home_city", "London", "I live in London"}, {"likes", "rock", "I like rock"}},
	"c3":  {{"home_city", "Berlin", "I live in Berlin"}},
	"c4":  {{"home_city", "Rome", "I live in Rome"}},
	"c5":  {{"home_city", "Madrid", "I live in Madrid"}, {"likes", "rock", "I like rock"}},
	"c6":  {{"likes", "pop", "I like pop"}},
	"c7":  {{"employer", "Acme", "I work at Acme"}},
	"c8a": {{"employer", "Globex", "I work at Globex"}},
	"c8b": {{"job_title", "engineer", "My job title is engineer"}},
	"c9":  {{"home_city", "Oslo", "I live in Oslo"}},
}

var (
	assessNewRe  = regexp.MustCompile(`(?m)^N(\d+): [^|]+\| ([^|]+) \|`)
	assessCandRe = regexp.MustCompile(`(?m)^  C(\d+)\.(\d+): [^|]+\| ([^|]+) \|`)
)

// stagedModel is a deterministic model: extraction by the chunk text in the prompt; the
// judge by attribute (same home_city or employer: contradictory; same likes: consistent;
// employer vs job_title: consistent; otherwise uncertain).
func stagedModel(prompt string) (string, error) {
	if isAssessmentPrompt(prompt) {
		newAttr := map[string]string{}
		for _, m := range assessNewRe.FindAllStringSubmatch(prompt, -1) {
			newAttr[m[1]] = strings.TrimSpace(m[2])
		}
		var vs []string
		for _, m := range assessCandRe.FindAllStringSubmatch(prompt, -1) {
			a, b := newAttr[m[1]], strings.TrimSpace(m[3])
			v := "uncertain"
			switch {
			case a == b && (a == "home_city" || a == "employer"):
				v = "contradictory"
			case a == b && a == "likes":
				v = "consistent"
			case (a == "employer" && b == "job_title") || (a == "job_title" && b == "employer"):
				v = "consistent"
			}
			vs = append(vs, fmt.Sprintf(`{"new":"N%s","candidate":"C%s.%s","verdict":"%s"}`, m[1], m[1], m[2], v))
		}
		return `{"verdicts":[` + strings.Join(vs, ",") + `]}`, nil
	}
	ids := make([]string, 0, len(stagedChunkText))
	for id := range stagedChunkText {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if strings.Contains(prompt, stagedChunkText[id]) {
			var fs []string
			for _, f := range stagedFacts[id] {
				fs = append(fs, fmt.Sprintf(`{"subject":"user","attribute":%q,"value":%q,"passage":1,"quote":%q}`, f[0], f[1], f[2]))
			}
			return `{"facts":[` + strings.Join(fs, ",") + `]}`, nil
		}
	}
	return `{"facts":[]}`, nil
}

func stagedDay(d int) time.Time { return time.Date(2023, 1, d, 0, 0, 0, 0, time.UTC) }

func stagedCand(doc string, day int, chunks ...string) domain.FactBackfillCandidate {
	return domain.FactBackfillCandidate{DocumentID: doc, ChunkIDs: chunks, SessionTime: stagedDay(day),
		Tags: []string{"q1"}, Author: "lme-q1", TimeSource: domain.TimeSourceExplicit}
}

// stagedMainCorpus is d1..d8, shuffled (d7 before d6: the tie is ordered by the list).
func stagedMainCorpus() []domain.FactBackfillCandidate {
	return []domain.FactBackfillCandidate{
		stagedCand("d5", 5, "c5"), stagedCand("d1", 1, "c1"), stagedCand("d7", 6, "c7"),
		stagedCand("d3", 3, "c3"), stagedCand("d6", 6, "c6"), stagedCand("d2", 2, "c2"),
		stagedCand("d8", 7, "c8a", "c8b"), stagedCand("d4", 4, "c4"),
	}
}

func stagedChunkStore() domain.VectorReader {
	var docs []domain.Document
	for id, text := range stagedChunkText {
		doc := "d" + strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(id, "c"), "a"), "b")
		docs = append(docs, domain.Document{ID: id, Text: text, DocumentType: domain.DocTypeMnemonicFact,
			Metadata: map[string]any{"tags": []string{"q1"}, "document_id": doc, "source_agent_id": "lme-q1"}})
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	return authz.NewEnforcingVectorStore(&laneChunkStore{docs: docs}, quietLog())
}

func stagedExtractorConfig() FactExtractorConfig {
	return FactExtractorConfig{
		Supersession: true, Assessments: true, FuzzyCandidates: 2, CardinalityMinVotes: 3,
		// Small enough that every passage is a window of its own (d8: two windows).
		MaxPromptChars: 100, AssessorModel: "judge", GeneratorModel: "gen",
	}
}

// stagedSnapshot is everything the equivalence compares.
type stagedSnapshot struct {
	Items       map[domain.KnowledgeItemID]domain.KnowledgeItem
	Assessments map[string]domain.Assessment
	Profiles    map[string]domain.PredicateProfile
	Markers     map[string]domain.ExtractionMarker
	Chains      map[string][]domain.Fact
	Journal     int
}

func snapshotOf(m *memSubstrate) stagedSnapshot {
	m.mu.Lock()
	s := stagedSnapshot{
		Items: map[domain.KnowledgeItemID]domain.KnowledgeItem{}, Assessments: map[string]domain.Assessment{},
		Profiles: map[string]domain.PredicateProfile{}, Markers: map[string]domain.ExtractionMarker{},
		Chains: map[string][]domain.Fact{}, Journal: len(m.journal),
	}
	keys := map[[2]string]bool{}
	for id, it := range m.items {
		s.Items[id] = it
		keys[[2]string{it.EntityID, it.AssertedBy}] = true
	}
	for id, a := range m.asms {
		s.Assessments[id] = a
	}
	for p, pr := range m.profiles {
		s.Profiles[p] = pr
	}
	for k, mk := range m.markers {
		s.Markers[k] = mk
	}
	m.mu.Unlock()
	for k := range keys {
		chain := m.derive(k[0], k[1], nil)
		for i := range chain {
			chain[i].Embedding = nil
		}
		s.Chains[k[0]+"|"+k[1]] = chain
	}
	return s
}

func diffSnapshots(t *testing.T, want, got stagedSnapshot) {
	t.Helper()
	if !reflect.DeepEqual(want.Items, got.Items) {
		t.Errorf("items differ: serial %d, two-phase %d", len(want.Items), len(got.Items))
	}
	if !reflect.DeepEqual(want.Assessments, got.Assessments) {
		for id := range want.Assessments {
			if _, ok := got.Assessments[id]; !ok {
				t.Errorf("assessment %s of the serial run is missing (inputs %v)", id, want.Assessments[id].InputItemIDs)
			}
		}
		for id := range got.Assessments {
			if _, ok := want.Assessments[id]; !ok {
				t.Errorf("assessment %s is not one the serial run made (inputs %v)", id, got.Assessments[id].InputItemIDs)
			}
		}
		t.Errorf("assessments differ: serial %d, two-phase %d", len(want.Assessments), len(got.Assessments))
	}
	if !reflect.DeepEqual(want.Profiles, got.Profiles) {
		t.Errorf("profiles differ:\nserial    %+v\ntwo-phase %+v", want.Profiles, got.Profiles)
	}
	if !reflect.DeepEqual(want.Markers, got.Markers) {
		t.Errorf("markers differ:\nserial    %+v\ntwo-phase %+v", want.Markers, got.Markers)
	}
	if !reflect.DeepEqual(want.Chains, got.Chains) {
		t.Errorf("derived chains differ")
	}
}

// runStagedFixture backfills d9 alone first (the earlier run), then the main corpus with
// the given group concurrency. model, when non-nil, replaces the deterministic model for
// the main run (the crash test).
func runStagedFixture(t *testing.T, sub *memSubstrate, gc int, ctx context.Context, gen *scriptedGen) (FactBackfillReport, error) {
	t.Helper()
	if gen == nil {
		gen = &scriptedGen{answer: stagedModel}
	}
	e := newTestExtractor(gen, sub, stagedExtractorConfig())
	read := stagedChunkStore()
	if len(sub.items) == 0 {
		pre := NewFactBackfill(&fakeLister{cands: []domain.FactBackfillCandidate{stagedCand("d9", 9, "c9")}}, read, e)
		if rep, err := pre.Run(context.Background(), FactBackfillOptions{Ordered: true, Logger: quietLog()}); err != nil || rep.Extracted != 1 {
			t.Fatalf("pre-run: %+v %v", rep, err)
		}
	}
	b := NewFactBackfill(&fakeLister{cands: stagedMainCorpus()}, read, e)
	return b.Run(ctx, FactBackfillOptions{Ordered: true, Concurrency: 1, GroupConcurrency: gc, Logger: quietLog()})
}

func serialStagedSnapshot(t *testing.T) stagedSnapshot {
	t.Helper()
	sub := newMemSubstrate()
	rep, err := runStagedFixture(t, sub, 1, context.Background(), nil)
	if err != nil || rep.TwoPhase || rep.Extracted != 8 || rep.Failed != 0 {
		t.Fatalf("serial run: %+v %v", rep, err)
	}
	return snapshotOf(sub)
}

func TestFactBackfillTwoPhase_EquivalentToSerialOrdered(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	want := serialStagedSnapshot(t)
	// The fixture does what it claims: both flips happened, assessments were stored,
	// and d8 is two windows.
	if want.Profiles["home_city"].Cardinality != domain.CardinalitySingle || want.Profiles["likes"].Cardinality != domain.CardinalityMulti {
		t.Fatalf("fixture: profiles %+v, want home_city single and likes multi", want.Profiles)
	}
	if len(want.Assessments) < 6 || want.Journal != 0 {
		t.Fatalf("fixture: %d assessments, journal %d", len(want.Assessments), want.Journal)
	}
	for _, gc := range []int{2, 4, 16} {
		t.Run(fmt.Sprintf("group_concurrency_%d", gc), func(t *testing.T) {
			sub := newMemSubstrate()
			rep, err := runStagedFixture(t, sub, gc, context.Background(), nil)
			if err != nil || !rep.TwoPhase || rep.Extracted != 8 || rep.Failed != 0 || rep.Cancelled != 0 {
				t.Fatalf("two-phase run: %+v %v", rep, err)
			}
			// d4's vote flips home_city: d5's speculated plan was wrong and was redone.
			if rep.SpeculationWaves < 2 || rep.AssessmentCallsDiscarded < 1 {
				t.Fatalf("expected a re-speculation after the cardinality flip: %+v", rep)
			}
			got := snapshotOf(sub)
			diffSnapshots(t, want, got)
			if got.Journal != 0 {
				t.Fatalf("committed documents left %d journal rows", got.Journal)
			}
		})
	}
}

// A run that stops between the phases resumes with NO extraction call (every window is
// reloaded from the journal) and still assesses every window: the end state is the
// serial run's.
func TestFactBackfillTwoPhase_ResumesAfterACrashBetweenPhases(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	want := serialStagedSnapshot(t)

	sub := newMemSubstrate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var extractCalls atomic.Int32
	crash := &scriptedGen{answer: func(p string) (string, error) {
		if isAssessmentPrompt(p) {
			cancel() // the first judge call: phase 1 is complete â€” "crash" here
			return "", context.Canceled
		}
		extractCalls.Add(1)
		return stagedModel(p)
	}}
	rep, err := runStagedFixture(t, sub, 4, ctx, crash)
	if !errors.Is(err, context.Canceled) || rep.Cancelled != 8 || rep.Extracted != 0 {
		t.Fatalf("crashed run: %+v %v", rep, err)
	}
	if n := extractCalls.Load(); n != 10 { // d9 in the pre-run + the main corpus's 9 windows
		t.Fatalf("made %d extraction calls, want 1 + 9 windows", n)
	}
	mid := snapshotOf(sub)
	if len(mid.Assessments) != 0 || mid.Journal != 9 {
		t.Fatalf("after the crash: %d assessments (want none committed), %d journal rows (want 9)", len(mid.Assessments), mid.Journal)
	}
	for k, mk := range mid.Markers {
		if mk.DocumentID != "d9" {
			t.Fatalf("a marker advanced before its assessment: %s %+v", k, mk)
		}
	}

	resume := &scriptedGen{answer: func(p string) (string, error) {
		if !isAssessmentPrompt(p) {
			return "", errors.New("the resumed run must not call the extractor")
		}
		return stagedModel(p)
	}}
	rep, err = runStagedFixture(t, sub, 4, context.Background(), resume)
	if err != nil || rep.Extracted != 8 || rep.Failed != 0 || rep.JournalWindowsReplayed != 9 {
		t.Fatalf("resumed run: %+v %v", rep, err)
	}
	diffSnapshots(t, want, snapshotOf(sub))
}

// Without the staged reads (a reader that is not a domain.FactStagedReader) the backfill
// runs each group serially â€” the serial path is always correct.
func TestFactBackfillTwoPhase_FallsBackWithoutStagedReads(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	sub := newMemSubstrate()
	gen := &scriptedGen{answer: stagedModel}
	e := NewFactExtractor(gen, &fakeEmbedder{}, authz.NewEnforcingKnowledgeWriter(sub, nil, quietLog()),
		authz.NewEnforcingFactItemReader(plainFactReader{sub}, quietLog()), stagedExtractorConfig())
	b := NewFactBackfill(&fakeLister{cands: stagedMainCorpus()}, stagedChunkStore(), e)
	rep, err := b.Run(context.Background(), FactBackfillOptions{Ordered: true, GroupConcurrency: 4, Logger: quietLog()})
	if err != nil || rep.TwoPhase || rep.Extracted != 8 {
		t.Fatalf("report %+v err %v, want a serial run", rep, err)
	}
}

// plainFactReader hides memSubstrate's staged reads.
type plainFactReader struct{ m *memSubstrate }

func (p plainFactReader) SearchFacts(ctx context.Context, v []float32, o domain.FactSearchOptions) ([]domain.FactHit, error) {
	return p.m.SearchFacts(ctx, v, o)
}
func (p plainFactReader) FactChain(ctx context.Context, f domain.Fact, s *domain.TagPredicate) ([]domain.Fact, error) {
	return p.m.FactChain(ctx, f, s)
}
func (p plainFactReader) SimilarFacts(ctx context.Context, f domain.Fact, v []float32, k int, s *domain.TagPredicate) ([]domain.Fact, error) {
	return p.m.SimilarFacts(ctx, f, v, k, s)
}
