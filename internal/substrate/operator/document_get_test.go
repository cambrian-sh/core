package operator_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

type fakeDocGetter struct {
	docs         map[string]domain.Document
	err          error
	gotPrincipal domain.PrincipalRef
	sawScope     bool
}

func (f *fakeDocGetter) GetDocument(_ context.Context, p domain.PrincipalRef, id string) (domain.Document, error) {
	f.gotPrincipal, f.sawScope = p, true
	if f.err != nil {
		return domain.Document{}, f.err
	}
	d, ok := f.docs[id]
	if !ok {
		return domain.Document{}, domain.ErrDocumentNotFound
	}
	return d, nil
}

func svcWithGetter(g domain.DocumentGetter) *operator.Service {
	s := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	if g != nil {
		s.SetDocumentGetter(g)
	}
	return s
}

// The primitive itself: an id in, that document's BODY out.
//
// ListDocuments is keyed but returns no body; QueryMemory returns bodies but is
// ranked. This is the read that joins them, and the body is the whole point — a
// citation an operator cannot read is not a citation.
func TestGetDocument_ReturnsTheBody(t *testing.T) {
	s := svcWithGetter(&fakeDocGetter{docs: map[string]domain.Document{
		"msg-1": {
			ID:           "msg-1",
			Text:         "PO-4471 is confirmed for the 14th.",
			Summary:      "confirmation",
			DocumentType: "memory",
			Metadata: map[string]any{
				"source": "slack:sales-internal",
				"tags":   []any{"sales", "customer_facing"},
			},
		},
	}})

	resp, err := s.GetDocument(context.Background(), &pb.GetDocumentOpRequest{Id: "msg-1"})
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if !resp.GetFound() {
		t.Fatal("found=false for a document that exists")
	}
	if resp.GetText() != "PO-4471 is confirmed for the 14th." {
		t.Fatalf("body not returned: %q", resp.GetText())
	}
	if resp.GetSource() != "slack:sales-internal" {
		t.Fatalf("source breadcrumb lost: %q", resp.GetSource())
	}
	if len(resp.GetTags()) != 2 {
		t.Fatalf("tags lost: %v", resp.GetTags())
	}
}

// A by-id BODY read must never reach the store without a predicate.
//
// ListDocuments can be unscoped because it "discloses nothing a tag listing would
// not"; this discloses everything, and ids travel freely — an alert cites one, a
// console links one. An unscoped variant would be a way to read a restricted
// document out of the reference to it. The operator plane reads at system scope,
// and this asserts that scope is passed EXPLICITLY rather than omitted, because a
// forgotten predicate and a deliberate system read look identical from here.
func TestGetDocument_AlwaysIdentifiesItsPrincipal(t *testing.T) {
	g := &fakeDocGetter{docs: map[string]domain.Document{"d": {ID: "d", Text: "x"}}}
	s := svcWithGetter(g)

	if _, err := s.GetDocument(context.Background(), &pb.GetDocumentOpRequest{Id: "d"}); err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if !g.sawScope {
		t.Fatal("the store was never called")
	}
	if g.gotPrincipal.Kind != domain.PrincipalSystem {
		t.Fatalf("the operator plane reads as the system principal; got %+v", g.gotPrincipal)
	}
}

// A dangling citation and a blank document are different answers.
//
// NotFound-as-an-error would make every unresolvable citation look like a
// transport failure, and a console cannot tell an operator "this reference points
// at nothing" from "the kernel is unreachable".
func TestGetDocument_MissingIsFoundFalseNotAnError(t *testing.T) {
	s := svcWithGetter(&fakeDocGetter{docs: map[string]domain.Document{}})

	resp, err := s.GetDocument(context.Background(), &pb.GetDocumentOpRequest{Id: "nope"})
	if err != nil {
		t.Fatalf("a missing id produced an ERROR rather than found=false: %v", err)
	}
	if resp.GetFound() {
		t.Fatal("found=true for an id that does not exist")
	}
	if resp.GetId() != "nope" {
		t.Fatalf("the requested id was not echoed back: %q", resp.GetId())
	}
}

// Unwired must be Unimplemented, never found=false for everything: a missing
// wiring would otherwise be indistinguishable from a corpus where nothing exists.
func TestGetDocument_UnwiredIsUnimplemented(t *testing.T) {
	s := svcWithGetter(nil)
	_, err := s.GetDocument(context.Background(), &pb.GetDocumentOpRequest{Id: "x"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented, got %v", err)
	}
}

func TestGetDocument_EmptyIDIsRejected(t *testing.T) {
	s := svcWithGetter(&fakeDocGetter{docs: map[string]domain.Document{}})
	_, err := s.GetDocument(context.Background(), &pb.GetDocumentOpRequest{Id: ""})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for an empty id, got %v", err)
	}
}

// A real store failure must stay an error. Collapsing it into found=false would
// report "this document does not exist" when the truth is "the database is down" —
// the same silent-absence failure the four-outcome drift model exists to prevent.
func TestGetDocument_StoreFailureIsNotReportedAsMissing(t *testing.T) {
	s := svcWithGetter(&fakeDocGetter{err: errors.New("connection refused")})
	_, err := s.GetDocument(context.Background(), &pb.GetDocumentOpRequest{Id: "msg-1"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("a store failure was not surfaced as an error: %v", err)
	}
}
