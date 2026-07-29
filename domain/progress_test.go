package domain

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestProgressPhase_ClosedVocabulary(t *testing.T) {
	valid := []ProgressPhase{
		PhaseUnderstanding, PhasePlanning, PhaseSearching,
		PhaseRunningTool, PhaseWorking, PhaseWriting,
	}
	for _, p := range valid {
		if !p.Valid() {
			t.Errorf("phase %q should be valid", p)
		}
	}
	// The vocabulary is CLOSED (ADR-0098 D7). Anything resembling internal state is not a
	// phase — that is the whole point of the boundary.
	for _, p := range []ProgressPhase{"", "chat_agent", "retrieval_agent", "step_3", "llm:deepseek"} {
		if p.Valid() {
			t.Errorf("phase %q must not be valid — internal state must not cross the boundary", p)
		}
	}
}

func TestProgressUpdate_TextNeverLeaksInternals(t *testing.T) {
	u := ProgressUpdate{ConversationID: "c1", Step: 2, TotalSteps: 4, Phase: PhaseRunningTool}
	got := u.Text()

	if !strings.Contains(got, "step 2 of 4") {
		t.Errorf("expected step counter in %q", got)
	}
	if !strings.Contains(got, string(PhaseRunningTool)) {
		t.Errorf("expected phase text in %q", got)
	}
	// No markup, no identifiers — this string lands on surfaces we do not control.
	for _, bad := range []string{"_", "agent", "llm:", "<", ">", "*", "`"} {
		if strings.Contains(got, bad) {
			t.Errorf("rendered text %q contains %q — it reaches end users verbatim", got, bad)
		}
	}
}

func TestProgressUpdate_TextWithoutPlanOmitsDenominator(t *testing.T) {
	// TotalSteps 0 means the plan is not known yet. Inventing "step 1 of 1" would be a lie
	// that the next update contradicts.
	u := ProgressUpdate{ConversationID: "c1", Phase: PhaseUnderstanding}
	if got := u.Text(); strings.Contains(got, "step") {
		t.Errorf("expected no step counter without a plan, got %q", got)
	}
}

func TestProgressUpdate_UnknownPhaseDegradesNotLeaks(t *testing.T) {
	// ADR-0098 D7: a new internal phase with no mapping degrades to the generic phrase,
	// never to a raw internal string.
	u := ProgressUpdate{ConversationID: "c1", Phase: ProgressPhase("kg_extractor_agent")}
	got := u.Text()
	if strings.Contains(got, "kg_extractor") {
		t.Fatalf("unmapped phase leaked internal name: %q", got)
	}
	if got != string(PhaseWorking) {
		t.Errorf("expected degrade to %q, got %q", PhaseWorking, got)
	}
}

func TestProgressUpdate_Validate(t *testing.T) {
	tests := []struct {
		name    string
		u       ProgressUpdate
		wantErr bool
	}{
		{"valid", ProgressUpdate{ConversationID: "c1", Phase: PhaseWorking}, false},
		{"no conversation", ProgressUpdate{Phase: PhaseWorking}, true},
		{"blank conversation", ProgressUpdate{ConversationID: "   ", Phase: PhaseWorking}, true},
		{"phase outside vocabulary", ProgressUpdate{ConversationID: "c1", Phase: "whatever"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.u.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// recordingSink captures what reached the sink.
type recordingSink struct{ got []ProgressUpdate }

func (r *recordingSink) Progress(_ context.Context, u ProgressUpdate) { r.got = append(r.got, u) }

func TestEmitProgress_BestEffort(t *testing.T) {
	ctx := context.Background()

	// ADR-0098 D5: the observer must not be able to break the observed. A nil sink is the
	// OSS default reaching a call site, and it must be silent, not a panic.
	EmitProgress(ctx, nil, ProgressUpdate{ConversationID: "c1", Phase: PhaseWorking})

	// A no-op sink is likewise safe.
	EmitProgress(ctx, NoopProgressSink{}, ProgressUpdate{ConversationID: "c1", Phase: PhaseWorking})

	sink := &recordingSink{}
	EmitProgress(ctx, sink, ProgressUpdate{ConversationID: "c1", Phase: PhaseSearching})
	if len(sink.got) != 1 {
		t.Fatalf("expected 1 update, got %d", len(sink.got))
	}
	if sink.got[0].UpdatedAt.IsZero() {
		t.Error("EmitProgress should stamp UpdatedAt when the caller left it zero")
	}
}

func TestEmitProgress_DropsInvalidRatherThanPropagating(t *testing.T) {
	sink := &recordingSink{}
	// Malformed updates are a programming error at the emission seam. They must not reach
	// a user-visible surface, and must not fail the work being described.
	EmitProgress(context.Background(), sink, ProgressUpdate{Phase: PhaseWorking})               // no conversation
	EmitProgress(context.Background(), sink, ProgressUpdate{ConversationID: "c1", Phase: "??"}) // bad phase

	if len(sink.got) != 0 {
		t.Errorf("expected invalid updates to be dropped, sink received %d", len(sink.got))
	}
}

func TestEmitProgress_PreservesCallerTimestamp(t *testing.T) {
	sink := &recordingSink{}
	when := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	EmitProgress(context.Background(), sink, ProgressUpdate{
		ConversationID: "c1", Phase: PhaseWriting, UpdatedAt: when,
	})
	if !sink.got[0].UpdatedAt.Equal(when) {
		t.Errorf("caller timestamp overwritten: got %v want %v", sink.got[0].UpdatedAt, when)
	}
}
