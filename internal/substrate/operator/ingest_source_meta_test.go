package operator_test

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// T-20261103-operator-plane-ingestrequest-cannot-carry (contract 0104): the operator
// ingest carries source_kind / title / author / per-part speaker under the agent
// plane's A3 rules — descriptive only, trimmed, capped, control characters refused —
// and the authenticated operator stays the writer of record.
func TestIngestMemory_DescriptiveSourceMetadata(t *testing.T) {
	t.Parallel()
	svc, _, ing := newIngestService()
	svc.SetSourceMetaCaps(operator.SourceMetaCaps{Kind: 64, Title: 10, Author: 128, Speaker: 3})

	_, err := svc.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{
		CommandId: "m1", Reason: "seed", SourceKind: "  chat  ", Title: "Weekly ops review notes", Author: "Ayşe",
		Parts: []*pb.MemoryPartOp{{Text: "hello", Speaker: "Mehmet"}, {Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("IngestMemory: %v", err)
	}
	got := ing.last
	if got.SourceKind != "chat" || got.Title != "Weekly ops" || got.AuthorLabel != "Ayşe" {
		t.Fatalf("descriptive fields not normalised: kind=%q title=%q author=%q", got.SourceKind, got.Title, got.AuthorLabel)
	}
	if got.Author != "alice" {
		t.Fatalf("the client's author must never replace the operator: %q", got.Author)
	}
	if len(got.EventTime.Parts) != 2 || got.EventTime.Parts[0].Speaker != "Meh" || got.EventTime.Parts[1].Speaker != "" {
		t.Fatalf("part speakers: %+v", got.EventTime.Parts)
	}

	for _, bad := range []*pb.IngestMemoryOpRequest{
		{CommandId: "m2", Reason: "r", Text: "x", Title: "line1\nline2"},
		{CommandId: "m3", Reason: "r", Text: "x", Author: "a\tb"},
		{CommandId: "m4", Reason: "r", Parts: []*pb.MemoryPartOp{{Text: "x", Speaker: "a\x00"}}},
	} {
		if _, err := svc.IngestMemory(opCtx(), bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: want InvalidArgument for a control character, got %v", bad.GetCommandId(), err)
		}
	}
}

// ENG-2170 (contract 0112): the operator ingest carries labels separately from the
// classification hint, normalised and bounded under the same rule as the agent plane.
func TestIngestMemory_LabelsTravelSeparatelyFromTags(t *testing.T) {
	t.Parallel()
	svc, _, ing := newIngestService()
	svc.SetSourceMetaCaps(operator.SourceMetaCaps{LabelCount: 3, LabelChars: 16})
	if _, err := svc.IngestMemory(opCtx(), &pb.IngestMemoryOpRequest{
		CommandId: "l1", Reason: "seed", Text: "x", Tags: []string{"scope-docs"},
		Labels: []string{"vantis", " sop-001 ", "superseded", "sop-001"},
	}); err != nil {
		t.Fatalf("IngestMemory: %v", err)
	}
	got := ing.last
	if len(got.Labels) != 3 || got.Labels[1] != "sop-001" || got.Labels[2] != "superseded" {
		t.Fatalf("labels = %v", got.Labels)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "scope-docs" {
		t.Fatalf("tags = %v: labels must never join the classification", got.Tags)
	}
	for _, bad := range []*pb.IngestMemoryOpRequest{
		{CommandId: "l2", Reason: "r", Text: "x", Labels: []string{"a\x00"}},
		{CommandId: "l3", Reason: "r", Text: "x", Labels: []string{"a", "b", "c", "d"}},
		{CommandId: "l4", Reason: "r", Text: "x", Labels: []string{"a-label-longer-than-16"}},
	} {
		if _, err := svc.IngestMemory(opCtx(), bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: want InvalidArgument, got %v", bad.GetCommandId(), err)
		}
	}
	if ing.calls != 1 {
		t.Errorf("ingestor called %d times; a refused label must stop the request before any work", ing.calls)
	}
}
