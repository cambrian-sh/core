package ledger

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// closedCeilingDP restates the premium write rule as amended by ENG-2161 (ADR-0035
// C2): the classification is the writer's ceiling narrowed by the hint (ceiling ∩
// hint), the whole ceiling for an empty hint — except that an empty hint under a
// ceiling holding closed tags is refused with ReasonClassificationRequired. A writer
// with no ceiling gets ceiling ∩ hint = nothing, which core's ClassifyWriteStrict
// refuses for a tagged hint (ENG-2115). Core cannot import premium (AGENTS.md §1.2);
// premium's own tests pin the rule on the real Authorizer.
type closedCeilingDP struct {
	domain.AllowAllAuthorizer
	ceilings map[string][]string
	closed   []string
}

func (c closedCeilingDP) ClassifyWrite(_ context.Context, p domain.PrincipalRef, hint []string) ([]string, domain.AccessDecision) {
	ceiling := c.ceilings[p.WriterID()]
	req := authz.RequestedClassification(hint)
	if len(req) == 0 {
		for _, t := range ceiling {
			if slices.Contains(c.closed, t) {
				return nil, domain.AccessDecision{Reason: domain.ReasonClassificationRequired,
					ClassificationChoice: &domain.WriteClassificationChoice{Ceiling: ceiling, Closed: c.closed}}
			}
		}
	}
	out := []string{}
	for _, t := range ceiling {
		if len(req) == 0 || slices.Contains(req, t) {
			out = append(out, t)
		}
	}
	return domain.StampProvenanceSource(out, p.WriterID()), domain.AccessDecision{Allowed: true, Reason: domain.ReasonAllowed}
}

// captureWarn routes the default logger into a buffer for one test.
func captureWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func eng2173Recorder() (*Recorder, *memStore) {
	st := newMem()
	dp := closedCeilingDP{
		ceilings: map[string][]string{"analyst": {"scope-public", "scope-finance"}},
		closed:   []string{"scope-finance"},
	}
	return NewRecorder(st, dp, RecorderConfig{Retention: time.Hour}), st
}

var eng2173Prov = domain.LedgerProvenance{ConversationID: "conv-2173", TurnID: "turn-1", RunID: "run-2173", StepIndex: 0}

// ENG-2173 (owner decision 2026-11-19, inherit from sources): a tool result built
// from classified records — a memory recall's chunks — is classified with their
// classification, even when the writer's ceiling holds a closed tag and the tool
// has no domain tags; provenance stamps and owner tags of the sources are not
// requested.
func TestToolResultInheritsTheClassificationOfItsSources(t *testing.T) {
	r, _ := eng2173Recorder()
	e, err := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: eng2173Prov,
		ToolName: domain.RecallToolName, Writer: domain.AgentPrincipal("analyst"),
		SourceClassifications: [][]string{{"scope-finance", "provenance:source=ingest", "owner:alice"}},
		ContentType:           "application/json", Body: []byte(`{"hits":[{"text":"Q3 margin 12%"}]}`)})
	if err != nil {
		t.Fatalf("a tool result built from classified sources must be recorded: %v", err)
	}
	if !slices.Contains(e.Tags, "scope-finance") || slices.Contains(e.Tags, "scope-public") {
		t.Fatalf("tags = %v, want the sources' scope-finance (not the whole ceiling)", e.Tags)
	}
	if slices.Contains(e.Tags, "owner:alice") || slices.Contains(e.Tags, "provenance:source=ingest") {
		t.Fatalf("tags = %v: a source's owner/provenance stamp is not inherited as a request", e.Tags)
	}
	if !slices.Contains(e.Tags, "provenance:source=analyst") {
		t.Fatalf("tags = %v, want the writer's provenance stamp", e.Tags)
	}
	if r.Unrecorded() != 0 {
		t.Fatalf("unrecorded = %d", r.Unrecorded())
	}
}

// A staged call's tool result derived from earlier entries (DerivedFrom) inherits
// their classification the same way.
func TestToolResultInheritsFromItsInputEntries(t *testing.T) {
	r, _ := eng2173Recorder()
	ctx := context.Background()
	in, err := r.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: eng2173Prov,
		ToolName: "ledger_export", Writer: domain.AgentPrincipal("analyst"), ClassificationHint: []string{"scope-finance"},
		ContentType: "text/plain", Body: []byte("revenue 4.1M")})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: eng2173Prov,
		ToolName: "python_run", Writer: domain.AgentPrincipal("analyst"), DerivedFrom: []string{in.ID},
		ContentType: "text/plain", Body: []byte("growth 8%")})
	if err != nil {
		t.Fatalf("a staged result built from a classified input must be recorded: %v", err)
	}
	if !slices.Contains(out.Tags, "scope-finance") {
		t.Fatalf("tags = %v, want the input's scope-finance", out.Tags)
	}
}

// Built from nothing: the tool's domain tags classify the entry.
func TestToolResultWithoutSourcesUsesTheToolsTags(t *testing.T) {
	r, _ := eng2173Recorder()
	e, err := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: eng2173Prov,
		ToolName: "weather", Writer: domain.AgentPrincipal("analyst"), ClassificationHint: []string{"scope-public"},
		ContentType: "text/plain", Body: []byte("sunny")})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.Tags, "scope-public") || slices.Contains(e.Tags, "scope-finance") {
		t.Fatalf("tags = %v, want the tool's scope-public", e.Tags)
	}
}

// Sources the writer cannot classify with (outside its ceiling) fall back to the
// tool's tags; SourceTags still gate every read by the sources.
func TestToolResultSourcesOutsideTheCeilingFallBackToTheToolsTags(t *testing.T) {
	r, _ := eng2173Recorder()
	e, err := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: eng2173Prov,
		ToolName: "hr_lookup", Writer: domain.AgentPrincipal("analyst"), ClassificationHint: []string{"scope-public"},
		SourceClassifications: [][]string{{"scope-hr"}}, ContentType: "text/plain", Body: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.Tags, "scope-public") {
		t.Fatalf("tags = %v, want the tool's scope-public", e.Tags)
	}
	if len(e.SourceTags) != 1 || e.SourceTags[0][0] != "scope-hr" {
		t.Fatalf("source tags = %v, want [[scope-hr]] kept for the read gate", e.SourceTags)
	}
}

// Neither sources nor tool tags: refused — and loudly. Counted, logged at WARN with
// the decision point's reason, never written.
func TestToolResultWithNeitherIsRefusedLoudly(t *testing.T) {
	buf := captureWarn(t)
	r, st := eng2173Recorder()
	_, err := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: eng2173Prov,
		ToolName: "echo", Writer: domain.AgentPrincipal("analyst"), ContentType: "text/plain", Body: []byte("hi")})
	if !errors.Is(err, ErrUnclassifiable) {
		t.Fatalf("err = %v, want ErrUnclassifiable", err)
	}
	if st.puts != 0 {
		t.Fatalf("a refused entry was written (%d puts)", st.puts)
	}
	if r.Unrecorded() != 1 {
		t.Fatalf("unrecorded = %d, want 1", r.Unrecorded())
	}
	log := buf.String()
	for _, w := range []string{"level=WARN", "ledger_entry_unclassifiable", "tool=echo", "unrecorded_total=1", "scope-finance"} {
		if !strings.Contains(log, w) {
			t.Errorf("WARN log must carry %q: %s", w, log)
		}
	}
}

// A writer with no ceiling keeps the decision point's default: its inherited hint
// cannot be carried (ENG-2115), so the entry falls through to the untagged default,
// exactly as before ENG-2173 — and nothing is counted as unrecorded.
func TestToolResultNoCeilingWriterKeepsTheDefault(t *testing.T) {
	r, _ := eng2173Recorder()
	e, err := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: eng2173Prov,
		ToolName: domain.RecallToolName, Writer: domain.AgentPrincipal("chat"),
		SourceClassifications: [][]string{{"scope-finance"}}, ContentType: "text/plain", Body: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(e.Tags, "scope-finance") || len(e.SourceTags) != 1 {
		t.Fatalf("tags = %v source tags = %v: want the default with the source gate kept", e.Tags, e.SourceTags)
	}
	if r.Unrecorded() != 0 {
		t.Fatalf("unrecorded = %d", r.Unrecorded())
	}
}

// The inheritance is the tool-result rule only (the owner's decision): a step output
// keeps its own hint; with none under a closed ceiling it is refused and counted.
func TestStepOutputDoesNotTakeTheInheritedHint(t *testing.T) {
	captureWarn(t)
	r, _ := eng2173Recorder()
	_, err := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerStepOutput, Provenance: eng2173Prov,
		Writer: domain.AgentPrincipal("analyst"), SourceClassifications: [][]string{{"scope-finance"}},
		ContentType: "text/plain", Body: []byte("summary")})
	if !errors.Is(err, ErrUnclassifiable) || r.Unrecorded() != 1 {
		t.Fatalf("err = %v unrecorded = %d, want a counted refusal", err, r.Unrecorded())
	}
}
