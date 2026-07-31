package memory

import (
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// captureBus records published events.
type captureBus struct {
	mu     sync.Mutex
	events []domain.DomainEvent
}

func (b *captureBus) Subscribe(string, domain.EventHandler) {}

func (b *captureBus) Publish(e domain.DomainEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e)
	return nil
}

func (b *captureBus) written() []domain.MemoryWrittenEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []domain.MemoryWrittenEvent
	for _, e := range b.events {
		if w, ok := e.(domain.MemoryWrittenEvent); ok {
			out = append(out, w)
		}
	}
	return out
}

// ONE event per document, whatever the chunk count.
//
// Per document and not per chunk because a chunk is an internal unit of
// retrieval: a 200-chunk upload would put 200 rows on an operator's feed
// describing the single action they took. The chunk count is detail and rides in
// the summary.
func TestPublishWritten_OneEventPerDocumentNotPerChunk(t *testing.T) {
	bus := &captureBus{}
	im := &IngestionManager{}
	im.SetEventBus(bus)

	im.publishWritten(domain.ExternalDocument{
		SourceURI: "s3://bucket/report.pdf",
		Title:     "Q3 report",
		ThreadID:  "ingest-thread-1",
	}, "source_doc:report", 200)

	got := bus.written()
	if len(got) != 1 {
		t.Fatalf("%d events for one document, want exactly 1", len(got))
	}
	if got[0].DocID != "source_doc:report" {
		t.Errorf("DocID = %q, want the source-doc entity id", got[0].DocID)
	}
	if !strings.Contains(got[0].Summary, "200 chunks") {
		t.Errorf("the chunk count must survive in the summary, got %q", got[0].Summary)
	}
	if !strings.Contains(got[0].Summary, "Q3 report") {
		t.Errorf("the summary must name the document, got %q", got[0].Summary)
	}
	// The ingest THREAD, not a task session: an ingestion thread is not a run, and
	// reporting it as one would make every upload look like an execution's output.
	if got[0].SessionID != "ingest-thread-1" {
		t.Errorf("SessionID = %q, want the ingest thread", got[0].SessionID)
	}
}

// A document with no title still identifies itself.
func TestPublishWritten_FallsBackToTheSourceURI(t *testing.T) {
	bus := &captureBus{}
	im := &IngestionManager{}
	im.SetEventBus(bus)

	im.publishWritten(domain.ExternalDocument{SourceURI: "file:///notes.md"}, "source_doc:notes", 3)

	got := bus.written()
	if len(got) != 1 || !strings.Contains(got[0].Summary, "notes.md") {
		t.Fatalf("untitled document did not identify itself: %+v", got)
	}
}

// No bus, or no entity, publishes nothing rather than panicking — the same
// nil-safety every other optional seam on this manager has.
func TestPublishWritten_NilSafe(t *testing.T) {
	(&IngestionManager{}).publishWritten(domain.ExternalDocument{SourceURI: "x"}, "e", 1)

	bus := &captureBus{}
	im := &IngestionManager{}
	im.SetEventBus(bus)
	// An empty entity id means the mint failed; there is no document to announce.
	im.publishWritten(domain.ExternalDocument{SourceURI: "x"}, "", 1)
	if n := len(bus.written()); n != 0 {
		t.Fatalf("published %d events with no source-doc entity, want 0", n)
	}
}

// A FAILED ingest announces nothing.
//
// Ported from RememberService's TestRemember_NoEventOnRejectedWrite when the
// publisher moved here. It is the half worth keeping: an operator feed that
// reports writes which were refused is worse than one that reports nothing,
// because it invites action on material that is not there.
func TestPublishWritten_NotCalledWhenTheIngestFails(t *testing.T) {
	bus := &captureBus{}
	im := &IngestionManager{}
	im.SetEventBus(bus)

	// ProcessSync's contract: publishWritten runs only after chunks persist. The
	// two failure shapes are a mint failure (empty entity) and zero chunks; both
	// return before the publish, so neither announces anything.
	im.publishWritten(domain.ExternalDocument{SourceURI: "x"}, "", 0)

	if n := len(bus.written()); n != 0 {
		t.Fatalf("a failed ingest published %d events, want 0", n)
	}
}

func TestPublishWritten_SingularChunkReadsCorrectly(t *testing.T) {
	bus := &captureBus{}
	im := &IngestionManager{}
	im.SetEventBus(bus)
	im.publishWritten(domain.ExternalDocument{Title: "note"}, "source_doc:n", 1)
	got := bus.written()
	if len(got) != 1 || !strings.Contains(got[0].Summary, "(1 chunk)") {
		t.Fatalf("summary should read '1 chunk', got %q", got[0].Summary)
	}
}
