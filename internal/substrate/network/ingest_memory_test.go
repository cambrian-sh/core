package network

import (
	"context"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"

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

type fakeIngestionProcessor struct {
	gotDoc domain.ExternalDocument
	docID  string
	// err exercises the handler's gRPC code mapping on the CHUNKED path — the only
	// path there is. It used to be exercised through the MemoryWriter fallback,
	// which no longer exists: every ingest goes through the chunker.
	err error
}

func (f *fakeIngestionProcessor) ProcessSync(_ context.Context, doc domain.ExternalDocument) (string, error) {
	f.gotDoc = doc
	if f.err != nil {
		return "", f.err
	}
	return f.docID, nil
}

func (f *fakeMemoryWriter) Remember(_ context.Context, agentID, text string, hint []string, source, sessionID string, _ float64) (string, error) {
	f.gotAgentID, f.gotText, f.gotHint, f.gotSource, f.gotSession = agentID, text, hint, source, sessionID
	if f.err != nil {
		return "", f.err
	}
	return f.docID, nil
}

func TestIngestMemory_ChunkingPathThreadsTagsAndImportance(t *testing.T) {
	p := &fakeIngestionProcessor{docID: "source_doc:doc"}
	s := &Server{IngestionProcessor: p}

	resp, err := s.IngestMemory(agentCtx("analyst"), &pb.IngestMemoryRequest{
		Text:       "body",
		Source:     "source-uri",
		SessionId:  "sess-1",
		Tags:       []string{"document-qa", "source_document", "doc"},
		Importance: 1.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetDocId() != "source_doc:doc" {
		t.Fatalf("doc id = %q, want source_doc:doc", resp.GetDocId())
	}
	if p.gotDoc.SourceURI != "source-uri" || p.gotDoc.Body != "body" || p.gotDoc.Author != "analyst" || p.gotDoc.ThreadID != "sess-1" {
		t.Fatalf("processor got doc = %+v", p.gotDoc)
	}
	if len(p.gotDoc.Tags) != 3 || p.gotDoc.Tags[2] != "doc" {
		t.Fatalf("processor tags = %#v", p.gotDoc.Tags)
	}
	if p.gotDoc.Importance != 1.0 {
		t.Fatalf("processor importance = %v, want 1.0", p.gotDoc.Importance)
	}
}

// 0035-05: an unknown principal (no scope profile) maps to PermissionDenied
// (fail-closed), and is refused BEFORE any ingest work happens.
//
// The guard moved onto the handler when RememberService was deleted: it used to
// live in that service, which sat on the unreachable raw-write fallback, so the
// live chunked path had no equivalent — the enforcing store only asks
// ClassifyWrite and has no notion of "we have never heard of you".
func TestIngestMemory_UnknownPrincipalIsPermissionDenied(t *testing.T) {
	p := &fakeIngestionProcessor{docID: "source_doc:x"}
	s := &Server{IngestionProcessor: p, Authz: denyUnknownAuthz{}}

	_, err := s.IngestMemory(agentCtx("ghost"), &pb.IngestMemoryRequest{Text: "x"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for unknown principal, got %v", err)
	}
	if p.gotDoc.Body != "" {
		t.Error("an unknown principal reached the ingestion pipeline; the guard must " +
			"refuse before any work is done")
	}
}

// denyUnknownAuthz resolves nobody: ReadFilter returns a nil predicate, which is
// the decision point saying it could not resolve the principal AT ALL — distinct
// from "registered but unprofiled", which resolves to an unrestricted predicate.
type denyUnknownAuthz struct{}

func (denyUnknownAuthz) Authorize(context.Context, domain.AccessRequest) domain.AccessDecision {
	return domain.AccessDecision{}
}

func (denyUnknownAuthz) Filter(context.Context, domain.PrincipalRef, domain.SurfaceRef, domain.ResourceKind, []domain.Taggable) ([]domain.Taggable, []domain.AccessDecision) {
	return nil, nil
}

func (denyUnknownAuthz) ReadFilter(context.Context, domain.PrincipalRef, domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	return nil, domain.AccessDecision{Reason: domain.ReasonNoPrincipal}
}

func (denyUnknownAuthz) ClassifyWrite(context.Context, domain.PrincipalRef, []string) ([]string, domain.AccessDecision) {
	return nil, domain.AccessDecision{}
}

// 0035-05: a coined narrow-only hint (tag outside the controlled vocabulary) maps
// to InvalidArgument — the agent must learn the tag has to be added by the operator.
func TestIngestMemory_CoinedHintIsInvalidArgument(t *testing.T) {
	s := &Server{IngestionProcessor: &fakeIngestionProcessor{err: authz.ErrWriteDenied}}

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
	p := &fakeIngestionProcessor{docID: "source_doc:doc-42"}
	s := &Server{IngestionProcessor: p}

	resp, err := s.IngestMemory(agentCtx("analyst"), &pb.IngestMemoryRequest{
		Text: "an insight", Tags: []string{"analytics"}, Source: "src", SessionId: "sess-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetDocId() != "source_doc:doc-42" {
		t.Errorf("expected the source-doc entity id, got %q", resp.GetDocId())
	}
	if p.gotDoc.Author != "analyst" {
		t.Errorf("handler must thread the authenticated x-agent-id as Author, got %q", p.gotDoc.Author)
	}
	// The session id becomes the INGEST THREAD, not a task session: an ingestion
	// thread is not a run (internal/memory/ingestion_chunks.go), and conflating them
	// once made every corpus chunk look like the output of a run.
	if p.gotDoc.Body != "an insight" || p.gotDoc.SourceURI != "src" || p.gotDoc.ThreadID != "sess-1" {
		t.Errorf("handler must thread request fields, got body=%q source=%q thread=%q",
			p.gotDoc.Body, p.gotDoc.SourceURI, p.gotDoc.ThreadID)
	}
}

// With no chunking pipeline the RPC FAILS rather than writing to the store
// directly (not a panic, and not a silent second shape of memory).
//
// This used to assert Unimplemented and then fall through to a raw
// MemoryWriter.Remember, which produced an un-chunked row with no source-document
// entity, invisible to ListDocuments. Every ingest goes through the chunker; there
// is no second way in.
func TestIngestMemory_NoChunkingPipelineFailsRatherThanWritingDirectly(t *testing.T) {
	s := &Server{}
	_, err := s.IngestMemory(agentCtx("a"), &pb.IngestMemoryRequest{Text: "x"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition with no ingestion pipeline, got %v", err)
	}
	if !strings.Contains(status.Convert(err).Message(), "chunker") {
		t.Errorf("the error must name the invariant it is protecting, got %q", err)
	}
}
