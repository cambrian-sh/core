package network

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/internal/memory"
	"github.com/cambrian-sh/cambrian-runtime/internal/scope"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeMemoryWriter is a capturing MemoryWriter seam. It returns a pre-seeded error
// (to exercise the handler's gRPC code mapping) or a fixed doc ID on success.
type fakeMemoryWriter struct {
	err   error
	docID string

	gotAgentID, gotText, gotSource, gotSession string
	gotHint                                    []string
}

func (f *fakeMemoryWriter) Remember(_ context.Context, agentID, text string, hint []string, source, sessionID string, _ float64) (string, error) {
	f.gotAgentID, f.gotText, f.gotHint, f.gotSource, f.gotSession = agentID, text, hint, source, sessionID
	if f.err != nil {
		return "", f.err
	}
	return f.docID, nil
}

// 0035-05: an unknown principal (no scope profile) maps to PermissionDenied (fail-closed).
func TestIngestMemory_UnknownPrincipalIsPermissionDenied(t *testing.T) {
	s := &Server{MemoryWriter: &fakeMemoryWriter{err: memory.ErrUnknownPrincipal}}

	_, err := s.IngestMemory(agentCtx("ghost"), &pb.IngestMemoryRequest{Text: "x"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for unknown principal, got %v", err)
	}
}

// 0035-05: a coined narrow-only hint (tag outside the controlled vocabulary) maps
// to InvalidArgument — the agent must learn the tag has to be added by the operator.
func TestIngestMemory_CoinedHintIsInvalidArgument(t *testing.T) {
	s := &Server{MemoryWriter: &fakeMemoryWriter{err: scope.ErrUnknownClassification}}

	_, err := s.IngestMemory(agentCtx("analyst"), &pb.IngestMemoryRequest{
		Text: "x", Tags: []string{"invented"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for coined hint, got %v", err)
	}
}

// 0035-05: on success the handler threads the authenticated identity + request fields
// to the writer and returns the new doc ID.
func TestIngestMemory_SuccessReturnsDocIDAndThreadsIdentity(t *testing.T) {
	w := &fakeMemoryWriter{docID: "doc-42"}
	s := &Server{MemoryWriter: w}

	resp, err := s.IngestMemory(agentCtx("analyst"), &pb.IngestMemoryRequest{
		Text: "an insight", Tags: []string{"analytics"}, Source: "src", SessionId: "sess-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetDocId() != "doc-42" {
		t.Errorf("expected doc id doc-42, got %q", resp.GetDocId())
	}
	if w.gotAgentID != "analyst" {
		t.Errorf("handler must thread the authenticated x-agent-id, got %q", w.gotAgentID)
	}
	if w.gotText != "an insight" || w.gotSource != "src" || w.gotSession != "sess-1" {
		t.Errorf("handler must thread request fields, got text=%q source=%q session=%q", w.gotText, w.gotSource, w.gotSession)
	}
}

// 0035-05: with no writer configured the RPC is Unimplemented (not a panic).
func TestIngestMemory_NotConfiguredIsUnimplemented(t *testing.T) {
	s := &Server{}
	_, err := s.IngestMemory(agentCtx("a"), &pb.IngestMemoryRequest{Text: "x"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented when MemoryWriter is nil, got %v", err)
	}
}
