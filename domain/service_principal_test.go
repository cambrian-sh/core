package domain

import (
	"context"
	"slices"
	"testing"
)

func TestSourcePrincipal_IsDeterministicAndKindQualified(t *testing.T) {
	p := SourcePrincipal("telegram", "bot1")
	if p != SourcePrincipal(" telegram ", "bot1") || p.String() != "source:telegram:bot1" || p.WriterID() != "source:telegram:bot1" {
		t.Fatalf("got %v / %q", p, p.WriterID())
	}
	if p.SourceConnector() != "telegram" || !p.IsService() {
		t.Fatalf("connector %q service %v", p.SourceConnector(), p.IsService())
	}
	// A ':' in the connector cannot shift the connector/id boundary.
	if q := SourcePrincipal("a:b", "c"); q.SourceConnector() != "a_b" || q.String() != "source:a_b:c" {
		t.Fatalf("connector with ':': %v", q)
	}
	if SourcePrincipal("x", "").ID != "x:default" || !SourcePrincipal("", "y").IsZero() {
		t.Fatal("empty id defaults; empty connector is no source")
	}
}

func TestEvidenceSourcePrincipal(t *testing.T) {
	if got := EvidenceSourcePrincipal(Evidence{SourceID: "slack:workspace-x", NamespaceID: "default"}); got.String() != "source:slack:workspace-x" {
		t.Fatalf("got %v", got)
	}
	if got := EvidenceSourcePrincipal(Evidence{SourceID: "handbook", NamespaceID: "default"}); got.String() != "source:handbook:default" {
		t.Fatalf("got %v", got)
	}
	if !EvidenceSourcePrincipal(Evidence{}).IsZero() {
		t.Fatal("evidence with no source has no source principal")
	}
}

func TestWriterID_ServiceKindsQualifiedPeopleBare(t *testing.T) {
	if SystemPrincipal.WriterID() != "system:kernel" {
		t.Fatalf("kernel writer id %q, want the reserved system:kernel", SystemPrincipal.WriterID())
	}
	if AgentPrincipal("kernel").WriterID() != "kernel" || UserPrincipal("u1").WriterID() != "u1" {
		t.Fatal("agents and persons keep their bare id (existing provenance values)")
	}
	if AgentPrincipal("kernel").WriterID() == SystemPrincipal.WriterID() {
		t.Fatal("an agent named kernel must not share the kernel's writer id")
	}
	for id, want := range map[string]bool{"system:kernel": true, "source:x:y": true, "kernel": false, "sourcery": false} {
		if IsReservedPrincipalID(id) != want {
			t.Errorf("IsReservedPrincipalID(%q) != %v", id, want)
		}
	}
}

func TestStampProvenanceSource_ReplacesForgedStamp(t *testing.T) {
	got := StampProvenanceSource([]string{"a", ProvenanceSourceTagPrefix + "forged"}, "source:x:y")
	if !slices.Equal(got, []string{"a", ProvenanceSourceTagPrefix + "source:x:y"}) || !HasProvenanceSource(got) {
		t.Fatalf("got %v", got)
	}
}

// OSS stamps a SOURCE writer's provenance (facts then key per source) and leaves
// every other writer's authored tags alone — the kernel's included, so a backfill
// keys its facts like the live ingest did.
func TestAllowAllClassifyWrite_StampsSourcesOnly(t *testing.T) {
	ctx := context.Background()
	got, dec := AllowAllAuthorizer{}.ClassifyWrite(ctx, SourcePrincipal("slack", "ws"), []string{"t"})
	if !dec.Allowed || !slices.Equal(got, []string{"t", ProvenanceSourceTagPrefix + "source:slack:ws"}) {
		t.Fatalf("source: %v", got)
	}
	for _, p := range []PrincipalRef{SystemPrincipal, AgentPrincipal("a"), UserPrincipal("u")} {
		if got, _ := (AllowAllAuthorizer{}).ClassifyWrite(ctx, p, []string{"t"}); !slices.Equal(got, []string{"t"}) {
			t.Fatalf("%v: %v", p, got)
		}
	}
}
