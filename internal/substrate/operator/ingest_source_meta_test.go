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
