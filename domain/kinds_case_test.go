package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The case-modelling extensions of the kind registry
// (T-20261107-knowledge-substrate-extensions-for-case): enums, lifecycle rules,
// entity-scoped resolution, the designated_actor authority and declared event types.
// All of it is data; the "decision" kind below is declared here, not in Go.

func decisionSpec() KindSpec {
	return KindSpec{
		Kind:            "decision",
		Policy:          ResolutionPolicyDesignatedActor,
		ResolutionScope: ResolutionScopeEntity,
		Designated:      &DesignatedActors{ApprovedViaCommit: true, Sources: []string{"board_minutes"}},
		Predicates: map[string]ValueSpec{
			"question": {Type: "text"},
			"owner":    {Type: "entity"},
			"status":   {Type: "text", Enum: []string{"open", "proposed", "decided", "superseded", "dropped"}},
			"chosen":   {Type: "text"},
		},
		Lifecycle: &KindLifecycle{
			StatePredicate: "status",
			Initial:        []string{"open"},
			Transitions: map[string][]string{
				"open":     {"proposed", "dropped"},
				"proposed": {"decided", "open", "dropped"},
				"decided":  {"superseded"},
			},
			Required: map[string][]string{"decided": {"owner", "chosen"}},
		},
		EventTypes: []EventTypeSpec{
			{Type: "decision_made", Roles: []string{"decision", "decider", "evidence"}, RequiredRoles: []string{"decision", "decider"}},
		},
	}
}

func mustReg(t *testing.T, specs ...KindSpec) *KindRegistry {
	t.Helper()
	r, err := NewKindRegistry(specs, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func txt(p, v string) StatementValue { return StatementValue{Predicate: p, Type: "text", Text: v} }

func refusedWith(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !errors.Is(err, ErrKindRefused) || !strings.Contains(err.Error(), want) {
		t.Fatalf("want a refusal naming %q wrapping ErrKindRefused, got %v", want, err)
	}
}

func TestKindEnum_AcceptsDeclaredValueAndNamesTheSetOnRefusal(t *testing.T) {
	r := mustReg(t, decisionSpec())
	if err := r.ValidateValues("decision", []StatementValue{txt("status", "open")}); err != nil {
		t.Fatalf("declared value refused: %v", err)
	}
	refusedWith(t, r.ValidateValues("decision", []StatementValue{txt("status", "maybe")}),
		`value "maybe" is not one of the declared values (open, proposed, decided, superseded, dropped)`)
	// Undeclared kinds are still unvalidated (ADR-0110 D2).
	if err := r.ValidateValues("other", []StatementValue{txt("status", "anything")}); err != nil {
		t.Fatal(err)
	}
}

func TestKindLifecycle_TransitionsInitialAndRequired(t *testing.T) {
	r := mustReg(t, decisionSpec())
	item := func(vals ...StatementValue) KnowledgeItem { return KnowledgeItem{Kind: "decision", Values: vals} }
	cur := func(vals ...StatementValue) *KnowledgeItem { it := item(vals...); return &it }

	if err := r.ValidateTransition("decision", nil, item(txt("status", "open"))); err != nil {
		t.Fatalf("initial state refused: %v", err)
	}
	refusedWith(t, r.ValidateTransition("decision", nil, item(txt("status", "decided"))), `"decided" is not an initial state of status`)
	if err := r.ValidateTransition("decision", cur(txt("status", "open")), item(txt("status", "proposed"))); err != nil {
		t.Fatalf("declared transition refused: %v", err)
	}
	refusedWith(t, r.ValidateTransition("decision", cur(txt("status", "open")), item(txt("status", "decided"))),
		`transition status "open" -> "decided" is not declared (from "open": proposed, dropped)`)
	refusedWith(t, r.ValidateTransition("decision", cur(txt("status", "dropped")), item(txt("status", "open"))),
		`("dropped" is terminal)`)
	// Same state is always allowed (editing other fields).
	if err := r.ValidateTransition("decision", cur(txt("status", "proposed")), item(txt("status", "proposed"), txt("question", "q2"))); err != nil {
		t.Fatal(err)
	}
	// Required predicates: neither on the item nor carried.
	refusedWith(t, r.ValidateTransition("decision", cur(txt("status", "proposed")), item(txt("status", "decided"), txt("chosen", "A"))),
		`state status="decided" requires predicate "owner", which has no current value`)
	// Carried by the current resolved item.
	carried := cur(txt("status", "proposed"), StatementValue{Predicate: "owner", Type: "entity", EntityRef: "person/alice"})
	if err := r.ValidateTransition("decision", carried, item(txt("status", "decided"), txt("chosen", "A"))); err != nil {
		t.Fatalf("carried required predicate refused: %v", err)
	}
	// No state predicate on the item, or a negation: no lifecycle check.
	if err := r.ValidateTransition("decision", cur(txt("status", "dropped")), item(txt("question", "q"))); err != nil {
		t.Fatal(err)
	}
	neg := item(txt("status", "open"))
	neg.Negation = true
	if err := r.ValidateTransition("decision", cur(txt("status", "dropped")), neg); err != nil {
		t.Fatal(err)
	}
}

func TestKindRegistry_RefusesMalformedCaseExtensionsAtBoot(t *testing.T) {
	base := decisionSpec
	for name, c := range map[string]struct {
		mut  func(*KindSpec)
		want string
	}{
		"enum on number":         {func(s *KindSpec) { s.Predicates["n"] = ValueSpec{Type: "number", Enum: []string{"1"}} }, "enum is only valid on a text predicate"},
		"entity scope latest":    {func(s *KindSpec) { s.Policy, s.Designated = "", nil }, `needs a policy other than "latest_assertion"`},
		"unknown scope":          {func(s *KindSpec) { s.ResolutionScope = "world" }, `unknown resolution_scope "world"`},
		"designated without set": {func(s *KindSpec) { s.Designated = &DesignatedActors{} }, "needs a designated set"},
		"designated star":        {func(s *KindSpec) { s.Designated = &DesignatedActors{Principals: []string{"*"}} }, `designated actor "*"`},
		"set without policy": {func(s *KindSpec) {
			s.Policy, s.ResolutionScope = "", ""
		}, "declares a designated set but its policy is"},
		"state not text":      {func(s *KindSpec) { s.Lifecycle.StatePredicate = "owner" }, "must be a declared text predicate"},
		"state outside enum":  {func(s *KindSpec) { s.Lifecycle.Transitions["open"] = []string{"limbo"} }, `names state "limbo"`},
		"required undeclared": {func(s *KindSpec) { s.Lifecycle.Required["decided"] = []string{"rationale"} }, `requires predicate "rationale"`},
		"event role required not allowed": {func(s *KindSpec) {
			s.EventTypes = []EventTypeSpec{{Type: "x", Roles: []string{"a"}, RequiredRoles: []string{"b"}}}
		}, `requires role "b"`},
	} {
		t.Run(name, func(t *testing.T) {
			s := base()
			c.mut(&s)
			_, err := NewKindRegistry([]KindSpec{s}, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want boot refusal %q, got %v", c.want, err)
			}
		})
	}
	// An event type declared by two kinds is a boot error.
	other := KindSpec{Kind: "incident", Predicates: map[string]ValueSpec{"x": {Type: "text"}},
		EventTypes: []EventTypeSpec{{Type: "decision_made"}}}
	if _, err := NewKindRegistry([]KindSpec{decisionSpec(), other}, nil); err == nil || !strings.Contains(err.Error(), "declared by both") {
		t.Fatalf("duplicate event type: %v", err)
	}
	// Entity scope under an input-taking authority is refused.
	fact := MemoryFactKindSpec()
	fact.ResolutionScope = ResolutionScopeEntity
	if _, err := NewKindRegistry([]KindSpec{fact}, []ResolutionAuthority{ValidTimeSupersessionAuthority{}}); err == nil ||
		!strings.Contains(err.Error(), "input-taking") {
		t.Fatalf("entity scope on input authority: %v", err)
	}
}

func TestDesignatedActorAuthority_OnlyEligibleWordsWin(t *testing.T) {
	spec := decisionSpec()
	at := func(m int) time.Time { return time.Date(2026, 11, 7, 10, m, 0, 0, time.UTC) }
	agent := KnowledgeItem{ID: "a", AssertedBy: "agent:scout", AssertedAt: at(9), SourceRef: KnowledgeProposalSourceRefPrefix + "x",
		Values: []StatementValue{txt("chosen", "B")}}
	forged := KnowledgeItem{ID: "f", AssertedBy: "user:mallory", AssertedAt: at(8), SourceRef: "knowledge_commitx", // no colon: not the marker
		Values: []StatementValue{txt("chosen", "C")}}
	commit1 := KnowledgeItem{ID: "c1", AssertedBy: "user:alice", AssertedAt: at(1), SourceRef: KnowledgeCommitSourceRefPrefix + "ap1",
		Values: []StatementValue{txt("chosen", "A")}}
	minutes := KnowledgeItem{ID: "m", AssertedBy: "board_minutes", AssertedAt: at(5), Values: []StatementValue{txt("chosen", "A2")}}

	var a DesignatedActorAuthority
	if w, why := a.ResolveKind(spec, []KnowledgeItem{agent, forged}); w != nil || why != ReasonNoDesignated {
		t.Fatalf("no eligible item must mean no resolution, got %v %q", w, why)
	}
	w, why := a.ResolveKind(spec, []KnowledgeItem{agent, commit1, forged, minutes})
	if w == nil || w.ID != "m" || why != ReasonDesignatedAssertion {
		t.Fatalf("latest ELIGIBLE item must win (the minutes at :05, not the agent at :09), got %v %q", w, why)
	}
	// Order independence.
	w2, _ := a.ResolveKind(spec, []KnowledgeItem{minutes, forged, commit1, agent})
	if w2 == nil || w2.ID != w.ID {
		t.Fatal("result depends on arrival order")
	}
	// A designated negation retracts.
	retract := KnowledgeItem{ID: "r", AssertedBy: "user:bob", AssertedAt: at(6), SourceRef: KnowledgeCommitSourceRefPrefix + "ap2", Negation: true}
	if w, why := a.ResolveKind(spec, []KnowledgeItem{commit1, minutes, retract, agent}); w != nil || why != ReasonNegated {
		t.Fatalf("designated negation: %v %q", w, why)
	}
	// Without a spec the authority admits nobody.
	if w, why := a.Resolve([]KnowledgeItem{commit1}); w != nil || why != ReasonNoDesignated {
		t.Fatal("spec-less Resolve must fail closed")
	}
}

func TestValidateEvent_DeclaredTypesOnly(t *testing.T) {
	r := mustReg(t, decisionSpec())
	ok := Event{Type: "decision_made", Roles: []EventRole{{Role: "decision", EntityID: "d1"}, {Role: "decider", EntityID: "p1"}}}
	if err := r.ValidateEvent(ok); err != nil {
		t.Fatal(err)
	}
	refusedWith(t, r.ValidateEvent(Event{Type: "decision_made", Roles: []EventRole{{Role: "decision", EntityID: "d1"}}}),
		`event type "decision_made" requires role "decider"`)
	refusedWith(t, r.ValidateEvent(Event{Type: "decision_made", Roles: append(ok.Roles, EventRole{Role: "bystander", EntityID: "x"})}),
		`does not declare role "bystander"`)
	if err := r.ValidateEvent(Event{Type: "gate_passage", Roles: []EventRole{{Role: "anything", EntityID: "x"}}}); err != nil {
		t.Fatal("undeclared event type must pass:", err)
	}
	var nilReg *KindRegistry
	if err := nilReg.ValidateEvent(ok); err != nil {
		t.Fatal(err)
	}
}

func TestApproverActor(t *testing.T) {
	for in, want := range map[string]string{"alice": "user:alice", "agent:x": "agent:x", "": "", "standing:stand-1": ""} {
		if got := ApproverActor(in); got != want {
			t.Errorf("ApproverActor(%q) = %q, want %q", in, got, want)
		}
	}
}

// A kind that sets none of the new fields keeps its pre-extension shape.
func TestKindRegistry_DefaultKindUnchanged(t *testing.T) {
	r := mustReg(t, KindSpec{Kind: "commitment", Predicates: map[string]ValueSpec{"due": {Type: "date"}}})
	if r.EntityScoped("commitment") || r.HasLifecycle("commitment") {
		t.Fatal("a plain kind must not gain scope or lifecycle")
	}
	s, _ := r.Spec("commitment")
	if s.Policy != ResolutionPolicyLatestAssertion {
		t.Fatalf("policy = %q", s.Policy)
	}
	if err := r.ValidateTransition("commitment", nil, KnowledgeItem{Values: []StatementValue{{Predicate: "due", Type: "date"}}}); err != nil {
		t.Fatal(err)
	}
}
