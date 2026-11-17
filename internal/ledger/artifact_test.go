package ledger

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0157 D5.2: an artifact entry is kept whole (no max_entry_bytes head) and an
// artifact over the artifact cap is refused by name, never truncated.
func TestRecorder_ArtifactKeptWholeAndCapped(t *testing.T) {
	st := newMem()
	r := NewRecorder(st, domain.AllowAllAuthorizer{}, RecorderConfig{MaxEntryBytes: 4, MaxArtifactBytes: 32})
	body := []byte("a model file of 24 bytes")
	e, err := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerArtifact, Provenance: prov,
		ToolName: "upload_artifact", Writer: domain.AgentPrincipal("analyst"), InstanceID: "i-1", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	got := st.rows[e.ID]
	if got.Truncated || string(got.Body) != string(body) || got.InstanceID != "i-1" {
		t.Fatalf("artifact entry: truncated=%v body=%q instance=%q", got.Truncated, got.Body, got.InstanceID)
	}
	_, err = r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerArtifact, Provenance: prov,
		ToolName: "upload_artifact", Writer: domain.AgentPrincipal("analyst"), Body: make([]byte, 33)})
	if !errors.Is(err, domain.ErrArtifactTooLarge) {
		t.Fatalf("oversized artifact: %v", err)
	}
}

// The same bytes written by two writers (or two instances) are two artifact
// entries; a retry by the same writer is the same entry.
func TestRecorder_ArtifactIdentityIncludesWriter(t *testing.T) {
	st := newMem()
	r := NewRecorder(st, domain.AllowAllAuthorizer{}, RecorderConfig{})
	rec := func(writer, inst string) string {
		e, err := r.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerArtifact, Provenance: prov,
			ToolName: "upload_artifact", Writer: domain.AgentPrincipal(writer), InstanceID: inst, Body: []byte("same")})
		if err != nil {
			t.Fatal(err)
		}
		return e.ID
	}
	a, b, a2, a3 := rec("alice_agent", "i1"), rec("bob_agent", "i2"), rec("alice_agent", "i1"), rec("alice_agent", "i9")
	if a == b || a == a3 || a != a2 || len(st.rows) != 3 {
		t.Fatalf("ids a=%s b=%s retry=%s other-instance=%s rows=%d", a, b, a2, a3, len(st.rows))
	}
}

// An artifact written during a plan step inherits that step's tool results:
// their ids become its DerivedFrom, their classifications its source tags, and
// the memory records they quote (SourceRefs) its own — transitively, so an
// ADR-0137 erase of a recalled document reaches the artifact built from it.
func TestRecorder_ArtifactInheritsStepInputsAndSourceRefs(t *testing.T) {
	st := newMem()
	r := NewRecorder(st, domain.AllowAllAuthorizer{}, RecorderConfig{})
	ctx := context.Background()
	recall, err := r.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: prov,
		ToolName: domain.RecallToolName, Writer: domain.AgentPrincipal("analyst"),
		SourceClassifications: [][]string{{"finance"}}, SourceRefs: []string{"chunk-1", "doc-7"}, Body: []byte(`{"hits":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	art, err := r.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerArtifact, Provenance: prov,
		ToolName: "agent_out", Writer: domain.AgentPrincipal("analyst"), Body: []byte("chart")})
	if err != nil {
		t.Fatal(err)
	}
	got := st.rows[art.ID]
	if len(got.DerivedFrom) != 1 || got.DerivedFrom[0] != recall.ID {
		t.Fatalf("DerivedFrom = %v, want [%s]", got.DerivedFrom, recall.ID)
	}
	if len(got.SourceRefs) != 2 || got.SourceRefs[0] != "chunk-1" || got.SourceRefs[1] != "doc-7" {
		t.Fatalf("SourceRefs = %v", got.SourceRefs)
	}
	finance := false
	for _, set := range got.SourceTags {
		for _, tg := range set {
			finance = finance || tg == "finance"
		}
	}
	if !finance {
		t.Fatalf("the artifact is easier to read than the recall it was built from: %v", got.SourceTags)
	}
	// A step output that names the artifact carries its refs on (transitive).
	out, err := r.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerStepOutput,
		Provenance: domain.LedgerProvenance{RunID: "run-2", StepIndex: 0}, DerivedFrom: []string{art.ID},
		Writer: domain.AgentPrincipal("analyst"), Body: []byte("summary")})
	if err != nil {
		t.Fatal(err)
	}
	if refs := st.rows[out.ID].SourceRefs; len(refs) != 2 {
		t.Fatalf("source refs not carried transitively: %v", refs)
	}
}
