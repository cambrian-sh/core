package domain

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

func d(s string) *time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &t
}

var session = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fitem builds a memory_fact item of the user/home_city key.
func fitem(id, value string, from, to *time.Time) KnowledgeItem {
	f := Fact{
		ID: id, Subject: "user", Attribute: "home_city", Value: value,
		SubjectKey: "user", AttributeKey: "home_city", ValueNorm: NormalizeFactText(value),
		ValidFrom: from, ValidTo: to, AssertedAt: session, Actor: "alice",
		SourceChunkID: "chunk-" + id, TurnIndex: 0, Quote: value,
	}
	it := f.ToItem("default")
	it.ID = KnowledgeItemID(id) // readable ids in the assertions
	return it
}

func intervals(out ResolutionOutcome) map[KnowledgeItemID]ItemInterval {
	m := map[KnowledgeItemID]ItemInterval{}
	for _, iv := range out.Chain {
		m[iv.ItemID] = iv
	}
	return m
}

func single() ResolutionInputs { return ResolutionInputs{Cardinality: CardinalitySingle} }

var vts = ValidTimeSupersessionAuthority{}

// ADR-0136 gate: TestFacts_SupersessionDirectionByDate, ported to the A1 authority.
// Direction is decided by the valid-time dates, never by arrival order: in order, out of
// order, into the middle of a chain, non-overlapping, restatement, same instant.
func TestFacts_SupersessionDirectionByDate(t *testing.T) {
	t.Run("in order: the older open value is closed at the newer start", func(t *testing.T) {
		out := vts.ResolveWith([]KnowledgeItem{fitem("paris", "Paris", d("2023-01-01"), nil), fitem("london", "London", d("2023-06-01"), nil)}, single())
		iv := intervals(out)
		if p := iv["paris"]; p.To == nil || !p.To.Equal(*d("2023-06-01")) || p.SupersededBy != "london" || p.Current {
			t.Fatalf("paris = %+v, want closed at 2023-06-01 by london", p)
		}
		if l := iv["london"]; l.To != nil || l.SupersededBy != "" || !l.Current {
			t.Fatalf("london = %+v, want open and current", l)
		}
		if out.Winner == nil || out.Winner.ID != "london" || out.Reason != ReasonFactCurrent {
			t.Fatalf("winner = %v %q", out.Winner, out.Reason)
		}
	})
	t.Run("out of order: an older statement arriving late is closed against the newer", func(t *testing.T) {
		// The set is what counts; the late arrival is listed last.
		out := vts.ResolveWith([]KnowledgeItem{fitem("london", "London", d("2023-06-01"), nil), fitem("paris", "Paris", d("2023-01-01"), nil)}, single())
		if p := intervals(out)["paris"]; p.To == nil || !p.To.Equal(*d("2023-06-01")) || p.SupersededBy != "london" {
			t.Fatalf("late paris = %+v, want closed at 2023-06-01 by london", p)
		}
		if out.Winner.ID != "london" {
			t.Fatalf("winner %s", out.Winner.ID)
		}
	})
	t.Run("out of order into the middle of a chain", func(t *testing.T) {
		out := vts.ResolveWith([]KnowledgeItem{
			fitem("a", "Paris", d("2022-01-01"), nil), fitem("c", "Rome", d("2024-01-01"), nil), fitem("b", "London", d("2023-01-01"), nil),
		}, single())
		iv := intervals(out)
		if a := iv["a"]; !a.To.Equal(*d("2023-01-01")) || a.SupersededBy != "b" {
			t.Fatalf("a = %+v, want closed at 2023 by b", a)
		}
		if b := iv["b"]; b.To == nil || !b.To.Equal(*d("2024-01-01")) || b.SupersededBy != "c" {
			t.Fatalf("b = %+v, want [2023, 2024) superseded by c", b)
		}
	})
	t.Run("non-overlapping: a stated end before the next start is not a supersession", func(t *testing.T) {
		out := vts.ResolveWith([]KnowledgeItem{fitem("paris", "Paris", d("2020-01-01"), d("2021-01-01")), fitem("london", "London", d("2022-01-01"), d("2022-06-01"))}, single())
		iv := intervals(out)
		if p := iv["paris"]; p.SupersededBy != "" || !p.To.Equal(*d("2021-01-01")) {
			t.Fatalf("paris = %+v, want its own stated end, no successor", p)
		}
		if l := iv["london"]; l.SupersededBy != "" || !l.To.Equal(*d("2022-06-01")) || l.Current {
			t.Fatalf("london = %+v", l)
		}
		if out.Winner != nil || out.Reason != ReasonFactClosed {
			t.Fatalf("nothing is current: %v %q", out.Winner, out.Reason)
		}
	})
	t.Run("restatement is a continuation, never a replacement", func(t *testing.T) {
		out := vts.ResolveWith([]KnowledgeItem{fitem("p1", "Paris", d("2023-01-01"), nil), fitem("p2", "paris", d("2023-06-01"), nil)}, single())
		iv := intervals(out)
		if !iv["p1"].Current || !iv["p2"].Current || iv["p1"].SupersededBy != "" {
			t.Fatalf("both statements of one value stay open: %+v", iv)
		}
	})
	t.Run("same instant: the later-ordered value wins, the other keeps an empty interval", func(t *testing.T) {
		a, b := fitem("paris", "Paris", d("2023-01-01"), nil), fitem("london", "London", d("2023-01-01"), nil)
		b.AssertedAt = session.Add(time.Hour) // recorded later
		out := vts.ResolveWith([]KnowledgeItem{b, a}, single())
		if p := intervals(out)["paris"]; !p.Empty || p.SupersededBy != "london" {
			t.Fatalf("paris = %+v, want empty, superseded by london", p)
		}
	})
	t.Run("no stated start: the assertion time orders it, and says so", func(t *testing.T) {
		a := fitem("paris", "Paris", nil, nil)
		b := fitem("london", "London", nil, nil)
		b.AssertedAt = session.AddDate(0, 6, 0)
		iv := intervals(vts.ResolveWith([]KnowledgeItem{a, b}, single()))
		if p := iv["paris"]; !p.FromInferred || p.SupersededBy != "london" || !p.To.Equal(b.AssertedAt) {
			t.Fatalf("paris = %+v", p)
		}
	})
}

// A1.2 rule 2: under multi or unknown cardinality values accumulate; only a negation, a
// supersedes_prior cue or a stored contradictory verdict closes one.
func TestFacts_CardinalityCuesAndVerdicts(t *testing.T) {
	jazz, rock := fitem("jazz", "jazz", d("2022-01-01"), nil), fitem("rock", "rock", d("2023-01-01"), nil)
	for _, card := range []string{CardinalityMulti, CardinalityUnknown} {
		iv := intervals(vts.ResolveWith([]KnowledgeItem{jazz, rock}, ResolutionInputs{Cardinality: card}))
		if !iv["jazz"].Current || !iv["rock"].Current {
			t.Fatalf("%s: both values accumulate, got %+v", card, iv)
		}
	}
	cued := rock
	cued.Values = append(append([]StatementValue(nil), rock.Values...), StatementValue{})
	for i, v := range cued.Values {
		if v.Predicate == FactPredSupersedesPrior {
			cued.Values[i].Number = 1
		}
	}
	cued.Values = cued.Values[:len(cued.Values)-1]
	if iv := intervals(vts.ResolveWith([]KnowledgeItem{jazz, cued}, ResolutionInputs{Cardinality: CardinalityMulti})); iv["jazz"].SupersededBy != "rock" {
		t.Fatalf("a cue closes the prior value: %+v", iv["jazz"])
	}
	asm := Assessment{ID: "a1", Authority: AssessmentAuthorityLLMContradiction, Verdicts: []AssessmentVerdict{{A: "rock", B: "jazz", Verdict: VerdictContradictory}}}
	if iv := intervals(vts.ResolveWith([]KnowledgeItem{jazz, rock}, ResolutionInputs{Cardinality: CardinalityUnknown, Assessments: []Assessment{asm}})); iv["jazz"].SupersededBy != "rock" || iv["rock"].SupersededBy != "" {
		t.Fatalf("a stored contradictory verdict closes by dates: %+v", iv)
	}
	asm.Verdicts[0].Verdict = VerdictConsistent
	if iv := intervals(vts.ResolveWith([]KnowledgeItem{jazz, rock}, ResolutionInputs{Cardinality: CardinalityUnknown, Assessments: []Assessment{asm}})); !iv["jazz"].Current {
		t.Fatalf("consistent keeps both: %+v", iv)
	}
	// An operator assessment overrules the model's.
	op := Assessment{ID: "a0", Authority: AssessmentAuthorityOperator, Verdicts: []AssessmentVerdict{{A: "jazz", B: "rock", Verdict: VerdictConsistent}}}
	asm.Verdicts[0].Verdict = VerdictContradictory
	if iv := intervals(vts.ResolveWith([]KnowledgeItem{jazz, rock}, ResolutionInputs{Cardinality: CardinalityUnknown, Assessments: []Assessment{op, asm}})); !iv["jazz"].Current {
		t.Fatalf("operator verdict must win: %+v", iv)
	}
	// A negation retires its value.
	neg := fitem("nojazz", "jazz", d("2024-01-01"), nil)
	neg.Negation = true
	out := vts.ResolveWith([]KnowledgeItem{jazz, neg}, ResolutionInputs{Cardinality: CardinalityMulti})
	if iv := intervals(out); iv["jazz"].SupersededBy != "nojazz" || iv["nojazz"].Current {
		t.Fatalf("negation: %+v", iv)
	}
	if out.Winner != nil || out.Reason != ReasonNegated {
		t.Fatalf("negated key: %v %q", out.Winner, out.Reason)
	}
}

// D5 rule 2-3 across keys: the model says WHETHER two facts contradict; the dates say
// which way (ported from TestFacts_ContradictionDirectionByDate).
func TestFacts_ContradictionDirectionByDate(t *testing.T) {
	lives := fitem("lives", "Paris", d("2022-01-01"), nil)
	moved := fitem("moved", "Berlin", d("2023-05-01"), nil)
	moved.EntityID = FactEntityID("", "user", "moved_to")
	asm := Assessment{ID: "x", Authority: AssessmentAuthorityLLMContradiction, Verdicts: []AssessmentVerdict{{A: "moved", B: "lives", Verdict: VerdictContradictory}}}
	in := ResolutionInputs{Cardinality: CardinalityUnknown, Assessments: []Assessment{asm}, Related: []KnowledgeItem{moved}}
	if iv := intervals(vts.ResolveWith([]KnowledgeItem{lives}, in)); iv["lives"].SupersededBy != "moved" || !iv["lives"].To.Equal(*d("2023-05-01")) {
		t.Fatalf("newer cross-key statement closes the older: %+v", iv["lives"])
	}
	in.Related = []KnowledgeItem{lives}
	if iv := intervals(vts.ResolveWith([]KnowledgeItem{moved}, in)); !iv["moved"].Current {
		t.Fatalf("an OLDER related item never closes the newer: %+v", iv["moved"])
	}
	ended := fitem("ended", "Lyon", d("2020-01-01"), d("2021-01-01"))
	in = ResolutionInputs{Assessments: []Assessment{{ID: "y", Verdicts: []AssessmentVerdict{{A: "ended", B: "moved", Verdict: VerdictContradictory}}}}, Related: []KnowledgeItem{moved}}
	if iv := intervals(vts.ResolveWith([]KnowledgeItem{ended}, in)); iv["ended"].SupersededBy != "" {
		t.Fatalf("non-overlapping: %+v", iv["ended"])
	}
}

// A1.10 falsification 1 (pure half): the derivation is a function of the SET of items
// and inputs. Every permutation of items, assessments and related items gives the same
// chain and winner.
func TestFacts_AuthorityIsOrderIndependent(t *testing.T) {
	items := []KnowledgeItem{
		fitem("a", "Paris", d("2021-01-01"), nil), fitem("b", "London", d("2022-01-01"), nil),
		fitem("c", "Paris", d("2022-01-01"), nil), fitem("d", "Rome", nil, nil), fitem("e", "Oslo", d("2024-01-01"), d("2024-03-01")),
	}
	other := fitem("x", "Madrid", d("2023-01-01"), nil)
	other.EntityID = FactEntityID("", "user", "lives_in")
	asms := []Assessment{
		{ID: "1", Authority: AssessmentAuthorityLLMContradiction, Verdicts: []AssessmentVerdict{{A: "a", B: "b", Verdict: VerdictContradictory}}},
		{ID: "2", Authority: AssessmentAuthorityLLMContradiction, Verdicts: []AssessmentVerdict{{A: "x", B: "c", Verdict: VerdictContradictory}}},
	}
	for _, card := range []string{CardinalitySingle, CardinalityMulti, CardinalityUnknown} {
		want := vts.ResolveWith(items, ResolutionInputs{Cardinality: card, Assessments: asms, Related: []KnowledgeItem{other}})
		r := rand.New(rand.NewSource(7))
		for range 50 {
			it := append([]KnowledgeItem(nil), items...)
			as := append([]Assessment(nil), asms...)
			r.Shuffle(len(it), func(i, j int) { it[i], it[j] = it[j], it[i] })
			r.Shuffle(len(as), func(i, j int) { as[i], as[j] = as[j], as[i] })
			got := vts.ResolveWith(it, ResolutionInputs{Cardinality: card, Assessments: as, Related: []KnowledgeItem{other}})
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: permutation changed the derivation:\n got %+v\nwant %+v", card, got, want)
			}
		}
	}
}

// A1-Q4 / ADR-0137 Q5: erasing the statement that closed a value leaves a content-free
// sentinel, and every later derivation keeps the prior value CLOSED — it never becomes
// current again. Erasing a predecessor removes it; the successor stays current.
func TestFacts_ErasedSentinelKeepsChainClosed(t *testing.T) {
	paris := fitem("paris", "Paris", d("2020-01-01"), nil)
	berlin := fitem("berlin", "Berlin", d("2022-01-01"), nil)
	// Before the erasure berlin closed paris at 2022; that is what the store reads off
	// the derivation and hands to the authority.
	closedAt := *intervals(vts.ResolveWith([]KnowledgeItem{paris, berlin}, single()))["paris"].To
	sentinel, ok := vts.ErasedSentinel(berlin, paris, closedAt)
	if !ok || !IsFactErasedSentinel(sentinel) || sentinel.EntityID != paris.EntityID || !sentinel.ValidFrom.Equal(*d("2022-01-01")) {
		t.Fatalf("sentinel = %+v", sentinel)
	}
	for _, v := range sentinel.Values {
		if v.Predicate != FactPredErased && v.Predicate != FactPredErasedCloses {
			t.Fatalf("the sentinel must carry no content, has %q", v.Predicate)
		}
	}
	if s2, _ := vts.ErasedSentinel(berlin, paris, closedAt); s2.ID != sentinel.ID {
		t.Fatal("sentinel id must be deterministic")
	}
	if _, ok := vts.ErasedSentinel(sentinel, paris, closedAt); ok {
		t.Fatal("erasing a sentinel leaves nothing")
	}
	// After erasing berlin: {paris, sentinel}. Under EVERY cardinality — the sentinel
	// does not depend on the profile — paris stays closed at 2022.
	for _, card := range []string{CardinalitySingle, CardinalityMulti, CardinalityUnknown} {
		out := vts.ResolveWith([]KnowledgeItem{sentinel, paris}, ResolutionInputs{Cardinality: card})
		p := intervals(out)["paris"]
		if p.Current || p.SupersededBy != sentinel.ID || !p.To.Equal(*d("2022-01-01")) {
			t.Fatalf("%s: paris re-opened after erasure: %+v", card, p)
		}
		if out.Winner != nil || out.Reason != ReasonFactErased {
			t.Fatalf("%s: winner %v reason %q, want none/erased", card, out.Winner, out.Reason)
		}
	}
	// Erasing the predecessor instead: it closed nothing, so it leaves no sentinel and
	// berlin stays current.
	out := vts.ResolveWith([]KnowledgeItem{berlin}, single())
	if out.Winner == nil || out.Winner.ID != "berlin" {
		t.Fatalf("successor must stay current, got %v %q", out.Winner, out.Reason)
	}
	// A sentinel closes ONLY the item it names: a multi neighbour stays open.
	jazz := fitem("jazz", "jazz", d("2020-01-01"), nil)
	if iv := intervals(vts.ResolveWith([]KnowledgeItem{jazz, paris, sentinel}, ResolutionInputs{Cardinality: CardinalityMulti})); !iv["jazz"].Current || iv["paris"].Current {
		t.Fatalf("targeted sentinel: %+v", iv)
	}
	// A genuinely newer statement after the erasure is current.
	rome := fitem("rome", "Rome", d("2023-01-01"), nil)
	if out := vts.ResolveWith([]KnowledgeItem{paris, sentinel, rome}, single()); out.Winner == nil || out.Winner.ID != "rome" {
		t.Fatalf("new value after erasure: %v", out.Winner)
	}
}

func TestFactItem_RoundTripAndIdentity(t *testing.T) {
	f := Fact{
		Subject: "User", Attribute: "Home City", Value: "Paris!", SubjectKey: NormalizeFactText("User"),
		AttributeKey: NormalizeFactAttribute("Home City"), ValueNorm: NormalizeFactText("Paris!"),
		ValidFrom: d("2023-05-20"), ValidFromSource: ValidFromStated, AssertedAt: session, Actor: "alice",
		Polarity: FactPositive, TurnIndex: 3, Quote: "I live in Paris", Speaker: "user", SupersedesPrior: true,
		SourceChunkID: "chunk-1", SourceDocumentID: "doc-1", EvidenceID: "ev-1", Tags: []string{"b", "a"},
	}
	it := f.ToItem("ns")
	if err := mustFactRegistry(t).ValidateValues(it.Kind, it.Values); err != nil {
		t.Fatalf("the item must fit core's memory_fact declaration: %v", err)
	}
	scope := FactScopeKey([]string{"a", "b"})
	if it.EntityID != scope+"/user/home_city" || it.SourceRef != "chunk-1|paris" || string(it.ID) != FactItemID("ev-1", "chunk-1", "user", "home_city", "paris") {
		t.Fatalf("identity: %+v", it)
	}
	if !strings.HasPrefix(string(it.ID), "ki_fact:") || len(it.ID) != len("ki_fact:")+64 {
		t.Fatalf("id %q", it.ID)
	}
	back := FactFromItem(it)
	if back.Attribute != "Home City" || back.Value != "Paris!" || back.ValueNorm != "paris" || back.Speaker != "user" ||
		!back.SupersedesPrior || back.TurnIndex != 3 || back.SourceChunkID != "chunk-1" || back.SourceDocumentID != "doc-1" ||
		back.ScopeKey != scope || back.SubjectKey != "user" || back.ValidFromSource != ValidFromStated || !back.ValidFrom.Equal(*f.ValidFrom) {
		t.Fatalf("round trip: %+v", back)
	}
	// Re-keying after classification moves entity id and subject reference together.
	it.Classification = []string{"z"}
	re := RekeyFactItem(it)
	if re.EntityID != FactScopeKey([]string{"z"})+"/user/home_city" {
		t.Fatalf("rekey entity: %s", re.EntityID)
	}
	for _, v := range re.Values {
		if v.Predicate == FactPredSubject && v.EntityRef != FactSubjectRef(FactScopeKey([]string{"z"}), "user") {
			t.Fatalf("rekey subject ref: %s", v.EntityRef)
		}
	}
	if FactPredicateOfEntity(re.EntityID) != "home_city" {
		t.Fatal("predicate of entity")
	}
}

func mustFactRegistry(t *testing.T) *KindRegistry {
	t.Helper()
	r, err := NewKindRegistry([]KindSpec{MemoryFactKindSpec()}, []ResolutionAuthority{ValidTimeSupersessionAuthority{}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFactNormalisationAndScopeKey(t *testing.T) {
	if NormalizeFactAttribute("Home  City") != "home_city" || NormalizeFactText("The Tesla, Model-3!") != "the tesla model 3" {
		t.Fatal("normalisation")
	}
	if FactScopeKey([]string{"b", "a", "a"}) != FactScopeKey([]string{"a", "b"}) || FactScopeKey(nil) != "" ||
		FactScopeKey([]string{"a"}) == FactScopeKey([]string{"b"}) {
		t.Fatal("scope key must be order/duplicate-insensitive and tag-sensitive")
	}
}

// A1.3: the assessment id is keyed on the judge and the candidate SET.
func TestAssessmentID_KeyedOnJudgeAndInputs(t *testing.T) {
	a := AssessmentID("llm_contradiction", "m1|p1", "h", []KnowledgeItemID{"x", "y", "x"})
	if a != AssessmentID("llm_contradiction", "m1|p1", "h", []KnowledgeItemID{"y", "x"}) {
		t.Fatal("input order/duplicates must not matter")
	}
	for _, other := range []string{
		AssessmentID("llm_contradiction", "m2|p1", "h", []KnowledgeItemID{"x", "y"}),
		AssessmentID("llm_contradiction", "m1|p1", "h2", []KnowledgeItemID{"x", "y"}),
		AssessmentID("llm_contradiction", "m1|p1", "h", []KnowledgeItemID{"x", "z"}),
	} {
		if other == a {
			t.Fatal("a different judge, prompt or candidate set is a different assessment")
		}
	}
}

// A1.4: learned cardinality flips only past the vote threshold with no opposing vote;
// a split vote stays unknown; a vote is counted once per assessment; a declared
// (config) profile keeps its cardinality whatever the votes say.
func TestPredicateProfile_LearnsPastThresholdAndOverrideWins(t *testing.T) {
	p := PredicateProfile{Predicate: "home_city", Cardinality: CardinalityUnknown, Source: ProfileSourceLearned}
	var changed bool
	for i, id := range []string{"a1", "a2"} {
		p, changed = ApplyProfileVote(p, ProfileVote{AssessmentID: id, Single: 1}, 3)
		if !changed || p.Cardinality != CardinalityUnknown {
			t.Fatalf("vote %d: %+v (below threshold must stay unknown)", i+1, p)
		}
	}
	if p2, ch := ApplyProfileVote(p, ProfileVote{AssessmentID: "a2", Single: 1}, 3); ch || p2.SingleVotes != 2 {
		t.Fatalf("a replayed assessment must not vote twice: %+v", p2)
	}
	p, _ = ApplyProfileVote(p, ProfileVote{AssessmentID: "a3", Single: 1}, 3)
	if p.Cardinality != CardinalitySingle || len(p.EvidenceAssessmentIDs) != 3 {
		t.Fatalf("third vote: %+v, want learned single with provenance", p)
	}
	p, _ = ApplyProfileVote(p, ProfileVote{AssessmentID: "a4", Multi: 1}, 3)
	if p.Cardinality != CardinalityUnknown {
		t.Fatalf("a split vote must fall back to unknown: %+v", p)
	}
	decl := PredicateProfile{Predicate: "likes", Cardinality: CardinalityMulti, Source: ProfileSourceDeclared}
	for _, id := range []string{"b1", "b2", "b3", "b4"} {
		decl, _ = ApplyProfileVote(decl, ProfileVote{AssessmentID: id, Single: 1}, 3)
	}
	if decl.Cardinality != CardinalityMulti || decl.SingleVotes != 4 {
		t.Fatalf("declared must win but record votes: %+v", decl)
	}
}
