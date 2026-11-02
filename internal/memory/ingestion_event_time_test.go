package memory

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// leafParser returns a fixed structure: the given leaves under the given parents.
type leafParser struct{ leaves []StructNode }

func (p leafParser) Parse(_ context.Context, req StructureParseRequest) (*StructuredDocument, error) {
	return &StructuredDocument{DocID: req.DocID, OK: true, Nodes: p.leaves}, nil
}

func chatDoc(t *testing.T, times []time.Time, turns []string) domain.ExternalDocument {
	t.Helper()
	parts := make([]domain.MemoryPartInput, len(turns))
	for i := range turns {
		tt := times[i]
		parts[i] = domain.MemoryPartInput{Text: turns[i], OccurredAt: &tt}
	}
	et, err := domain.ResolveIngestEventTime(nil, parts, "", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	doc := domain.ExternalDocument{SourceURI: "chat://s1", Title: "session 1", ThreadID: "s1", Body: et.Body}
	doc.ApplyEventTime(et)
	return doc
}

// ADR-0134 P0 / owner Q5: each flat chunk carries the earliest time of the parts it
// covers (here one part per paragraph chunk), every chunk is stamped in the typed
// fields, and the chunker's document-level "timestamp" no longer rides in metadata.
func TestPersistChunks_StampsEachChunkFromItsParts(t *testing.T) {
	t.Parallel()
	im := newCaptureIngestManager(t, nil)
	a := time.Date(2023, 5, 20, 7, 0, 0, 0, time.UTC)
	times := []time.Time{a, a.Add(time.Hour), a.Add(48 * time.Hour)}
	doc := chatDoc(t, times, []string{"user: we booked Lisbon", "assistant: noted", "user: trip cancelled"})

	chunks, _ := OptionCChunker{}.Chunk(context.Background(), &doc)
	if _, err := im.persistChunks(context.Background(), doc, chunks, "e1", "", false); err != nil {
		t.Fatal(err)
	}
	saved := im.agent.Manager.Store.(*captureAllStore).snapshot()
	if len(saved) != 3 {
		t.Fatalf("saved %d chunks, want 3", len(saved))
	}
	for i, d := range saved {
		if !d.EventTime.Equal(times[i]) || !d.EventTimeEnd.IsZero() {
			t.Errorf("chunk %d stamp = %+v, want %v", i, d.EventStamp, times[i])
		}
		if d.TimeSource != domain.TimeSourceExplicit || d.TimePrecision != domain.TimePrecisionSecond {
			t.Errorf("chunk %d source/precision = %q/%q", i, d.TimeSource, d.TimePrecision)
		}
		if d.CreatedAt.IsZero() {
			t.Errorf("chunk %d: the ingest clock (created_at) must be stated so a re-ingest moves it", i)
		}
		if _, ok := d.Metadata[domain.MetaEventTimestamp]; ok {
			t.Errorf("chunk %d still carries a metadata timestamp copy", i)
		}
	}
}

// Structure-leaf chunks used to carry no time at all. They are stamped too; a chunk
// that merges two leaves (joined by a single newline, so not a verbatim substring of
// the body) spans both parts' times.
func TestPersistChunks_StampsStructureLeafChunks(t *testing.T) {
	t.Parallel()
	a := time.Date(2023, 5, 20, 7, 0, 0, 0, time.UTC)
	times := []time.Time{a, a.Add(time.Hour), a.Add(2 * time.Hour)}
	turns := []string{"user: first", "assistant: second", "user: third"}
	doc := chatDoc(t, times, turns)
	p := leafParser{leaves: []StructNode{
		{ID: "l1", Kind: "paragraph", ParentID: "s1", Text: turns[0]},
		{ID: "l2", Kind: "paragraph", ParentID: "s1", Text: turns[1]},
		{ID: "l3", Kind: "paragraph", ParentID: "s2", Text: turns[2]},
	}}
	im := newCaptureIngestManager(t, p)
	if _, err := im.persistChunks(context.Background(), doc, nil, "e1", "", false); err != nil {
		t.Fatal(err)
	}
	saved := im.agent.Manager.Store.(*captureAllStore).snapshot()
	if len(saved) != 2 {
		t.Fatalf("saved %d chunks, want 2 (l1+l2 merged, l3)", len(saved))
	}
	if !saved[0].EventTime.Equal(times[0]) || !saved[0].EventTimeEnd.Equal(times[1]) {
		t.Errorf("merged leaf chunk = %+v, want %v..%v", saved[0].EventStamp, times[0], times[1])
	}
	if !saved[1].EventTime.Equal(times[2]) || !saved[1].EventTimeEnd.IsZero() {
		t.Errorf("leaf chunk = %+v, want %v", saved[1].EventStamp, times[2])
	}
}

// A document ingested with no stated time is stamped ingest_default, never left
// unlabelled, and the label survives to the hit (HitEventTime).
func TestPersistChunks_IngestDefaultIsLabelled(t *testing.T) {
	t.Parallel()
	im := newCaptureIngestManager(t, nil)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	et, err := domain.ResolveIngestEventTime(nil, nil, "", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	doc := domain.ExternalDocument{SourceURI: "file://a.txt", Body: "plain text"}
	doc.ApplyEventTime(et)
	chunks, _ := OptionCChunker{}.Chunk(context.Background(), &doc)
	if _, err := im.persistChunks(context.Background(), doc, chunks, "e1", "", false); err != nil {
		t.Fatal(err)
	}
	d := im.agent.Manager.Store.(*captureAllStore).snapshot()[0]
	if h := domain.HitEventTime(*d); h.Source != domain.TimeSourceIngestDefault || !h.EventTime.Equal(now) {
		t.Fatalf("hit time = %+v", h)
	}
}
