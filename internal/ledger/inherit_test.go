package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// T-20261108-ledger-step-outputs-do-not-inherit-the: a step output is never easier
// to read than the tool results it was built from, and the inheritance follows the
// plan's bindings transitively.
func TestStepOutputInheritsTheClassificationOfItsSources(t *testing.T) {
	ctx := context.Background()
	st := newMem()
	// The OSS allow-all classifier keeps the hint: a tool result is tagged with the
	// tool's domain tags, a step output (no hint) with nothing.
	r := NewRecorder(st, domain.AllowAllAuthorizer{}, RecorderConfig{Retention: time.Hour})
	p0 := domain.LedgerProvenance{ConversationID: "conv", RunID: "run-1", StepIndex: 0}
	if _, err := r.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: p0,
		ToolName: "hr_lookup", ArgsJSON: []byte(`{"id":"E-1"}`), Writer: domain.AgentPrincipal("worker"),
		ClassificationHint: []string{"hr"}, ContentType: "application/json", Body: []byte(`{"salary":91000}`)}); err != nil {
		t.Fatal(err)
	}
	out0, err := r.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerStepOutput, Provenance: p0,
		Writer: domain.AgentPrincipal("worker"), ContentType: "text/plain", Body: []byte("E-1 earns 91000")})
	if err != nil {
		t.Fatal(err)
	}
	p1 := p0
	p1.StepIndex = 1
	out1, err := r.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerStepOutput, Provenance: p1,
		Writer: domain.AgentPrincipal("writer"), DerivedFrom: []string{out0.ID}, ContentType: "text/plain",
		Body: []byte("The draft mentions 91000")})
	if err != nil {
		t.Fatal(err)
	}
	// ENG-2183: the step output's own classification is its sources' (the OSS
	// allow-all keeps the hint), on top of the SourceTags read gate.
	if len(out0.Tags) != 1 || out0.Tags[0] != "hr" {
		t.Fatalf("the step output's own tags must be its sources' [hr], got %v", out0.Tags)
	}
	hasHR := func(sets [][]string) bool {
		for _, s := range sets {
			if len(s) == 1 && s[0] == "hr" {
				return true
			}
		}
		return false
	}
	for _, e := range []domain.LedgerEntry{out0, out1} {
		if !hasHR(e.SourceTags) {
			t.Fatalf("entry %s (step %d) must inherit [hr], got %v", e.ID, e.StepIndex, e.SourceTags)
		}
	}

	reader := authz.NewEnforcingLedger(st, nil)
	noHR := domain.WithScope(ctx, &domain.TagPredicate{ForbiddenTags: []string{"hr"}})
	for _, id := range []string{out0.ID, out1.ID} {
		got, err := reader.Get(noHR, id)
		if err != nil || got != nil {
			t.Fatalf("a reader without the hr scope read derived entry %s by id (err %v)", id, err)
		}
	}
	rows, err := reader.Find(noHR, domain.LedgerQuery{RunID: "run-1"})
	if err != nil || len(rows) != 0 {
		t.Fatalf("ledger_find leaked %d derived/hr entries to a reader without the hr scope (err %v)", len(rows), err)
	}
	// A reader whose predicate admits hr reads both; so does the kernel's bypass.
	for _, eff := range []*domain.TagPredicate{{}, {Bypass: true}} {
		got, err := reader.Get(domain.WithScope(ctx, eff), out1.ID)
		if err != nil || got == nil {
			t.Fatalf("an hr-admitting reader (%+v) could not read the derived entry (err %v)", eff, err)
		}
	}
	// The stored step output carries its sources' classification as its own (ENG-2183).
	if e := st.rows[out0.ID]; len(e.Tags) != 1 || e.Tags[0] != "hr" {
		t.Fatalf("stored own tags = %v, want [hr]", e.Tags)
	}
}

// An AnyOf reader shows why the inheritance is a conjunction, not a tag union: a
// union would ADD "hr" to the output's own tags and so admit a reader whose AnyOf
// clause names hr even though that reader cannot read the finance-only source.
func TestDerivedEntryRequiresEverySourceToBeReadable(t *testing.T) {
	e := domain.LedgerEntry{Tags: []string{"hr"}, SourceTags: [][]string{{"finance"}}}
	anyHR := &domain.TagPredicate{AnyOfClauses: [][]string{{"hr"}}}
	if domain.LedgerEntryReadable(anyHR, e) {
		t.Fatal("a reader admitted by the entry's own tags but not by its source read it")
	}
	both := &domain.TagPredicate{AnyOfClauses: [][]string{{"hr", "finance"}}}
	if !domain.LedgerEntryReadable(both, e) {
		t.Fatal("a reader admitted by the entry and by its source was refused")
	}
	if domain.LedgerEntryReadable(nil, e) {
		t.Fatal("nil predicate must fail closed")
	}
}

// T-20261109-chat-agent-answers-a-published-rule: a memory recall's entry carries
// each recalled chunk's own tags (LedgerRecord.SourceClassifications), so a reader
// who could not read one of the chunks cannot read the entry either.
func TestRecallEntryIsNeverWiderThanItsChunks(t *testing.T) {
	ctx := context.Background()
	st := newMem()
	r := NewRecorder(st, domain.AllowAllAuthorizer{}, RecorderConfig{Retention: time.Hour})
	e, err := r.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerToolResult,
		Provenance: domain.LedgerProvenance{ConversationID: "conv", TurnID: "m1", StepIndex: -1},
		ToolName:   domain.RecallToolName, ArgsJSON: []byte(`{"query":"deadline"}`), Writer: domain.AgentPrincipal("chat_agent"),
		SourceClassifications: [][]string{{"public"}, {"hr"}}, ContentType: "application/json", Body: []byte(`{"hits":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	reader := authz.NewEnforcingLedger(st, nil)
	if got, _ := reader.Get(domain.WithScope(ctx, &domain.TagPredicate{ForbiddenTags: []string{"hr"}}), e.ID); got != nil {
		t.Fatal("a reader without the hr scope read a recall that contains an hr chunk")
	}
	if got, _ := reader.Get(domain.WithScope(ctx, &domain.TagPredicate{}), e.ID); got == nil {
		t.Fatal("a reader of every chunk could not read the recall entry")
	}
}
