package domain_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

func speakerDoc(t *testing.T, parts []domain.MemoryPartInput) domain.ExternalDocument {
	t.Helper()
	et, err := domain.ResolveIngestEventTime(nil, parts, "", time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	d := domain.ExternalDocument{Body: et.Body}
	d.ApplyEventTime(et)
	return d
}

// ADR-0136 A3.3: speakers never enter the body — the same parts with and without
// speakers assemble the same bytes and the same part spans.
func TestResolveIngestEventTime_SpeakersLeaveBodyByteIdentical(t *testing.T) {
	plain := speakerDoc(t, []domain.MemoryPartInput{{Text: "hi there"}, {Text: "hello back"}})
	labelled := speakerDoc(t, []domain.MemoryPartInput{{Text: "hi there", Speaker: "user"}, {Text: "hello back", Speaker: "assistant"}})
	if plain.Body != labelled.Body || plain.Body != "hi there"+domain.PartSeparator+"hello back" {
		t.Fatalf("body = %q vs %q", plain.Body, labelled.Body)
	}
	for i := range plain.Parts {
		if plain.Parts[i].Offset != labelled.Parts[i].Offset || plain.Parts[i].Length != labelled.Parts[i].Length {
			t.Fatalf("part %d span moved", i)
		}
	}
	if labelled.Parts[1].Speaker != "assistant" || plain.Parts[1].Speaker != "" {
		t.Fatalf("speakers = %+v / %+v", labelled.Parts, plain.Parts)
	}
}

// A chunk that starts mid-way through a part takes THAT part's speaker at start, and
// lists every speaker it overlaps, in body order.
func TestChunkSpeakers_MidPartChunkTakesThatPartsSpeaker(t *testing.T) {
	doc := speakerDoc(t, []domain.MemoryPartInput{
		{Text: "I booked the flight to Lisbon.", Speaker: "user"},
		{Text: "Great choice. I recommend the tram. Also visit Sintra.", Speaker: "assistant"},
		{Text: "I will, thanks.", Speaker: "user"},
	})
	// Chunk 0 = all of part 0 and the start of part 1; chunk 1 starts inside part 1
	// ("I recommend ...") and runs into part 2; chunk 2 is inside part 2 only.
	c0 := doc.Body[:strings.Index(doc.Body, " I recommend")]
	c1 := doc.Body[strings.Index(doc.Body, "I recommend"):strings.Index(doc.Body, " thanks")]
	c2 := "thanks."
	spans := domain.LocateChunkSpans(doc.Body, []string{c0, c1, c2, "not in the body"})
	got := domain.ChunkSpeakers(doc, spans)
	want := []domain.ChunkSpeaker{
		{AtStart: "user", Speakers: []string{"user", "assistant"}},
		{AtStart: "assistant", Speakers: []string{"assistant", "user"}},
		{AtStart: "user", Speakers: []string{"user"}},
		{}, // not located: unknown, never guessed
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChunkSpeakers = %+v\nwant %+v", got, want)
	}
}

// A chunk beginning on the blank-line separator takes the part it actually begins
// with; a document with no speakers labels nothing.
func TestChunkSpeakers_SeparatorStartAndUnlabelled(t *testing.T) {
	doc := speakerDoc(t, []domain.MemoryPartInput{{Text: "alpha", Speaker: "a"}, {Text: "beta", Speaker: "b"}})
	sep := len("alpha")
	got := domain.ChunkSpeakers(doc, []domain.ChunkSpan{{Start: sep, End: len(doc.Body), OK: true}})
	if got[0].AtStart != "b" || !reflect.DeepEqual(got[0].Speakers, []string{"b"}) {
		t.Fatalf("separator start = %+v", got[0])
	}
	plain := speakerDoc(t, []domain.MemoryPartInput{{Text: "alpha"}, {Text: "beta"}})
	if got := domain.ChunkSpeakers(plain, []domain.ChunkSpan{{Start: 0, End: 5, OK: true}}); got[0].AtStart != "" || got[0].Speakers != nil {
		t.Fatalf("unlabelled = %+v", got[0])
	}
}

// A3.2 precedence, the same on the live path (SourceMetaOf) and the backfill
// (FactBackfillCandidate.SourceMeta).
func TestSourceMeta_ClientDescriptiveFieldsTakePrecedence(t *testing.T) {
	at := time.Date(2023, 5, 20, 0, 0, 0, 0, time.UTC)
	doc := domain.ExternalDocument{SourceType: "ingest_memory", SourceKind: "conversation", Title: "T",
		Author: "agent-1", AuthorLabel: "Alice", Timestamp: at, TimeSource: domain.TimeSourceExplicit}
	live := domain.SourceMetaOf(doc, at, "agent-1")
	cand := domain.FactBackfillCandidate{SourceType: "ingest_memory", SourceKind: "conversation", Title: "T",
		Author: "agent-1", AuthorLabel: "Alice", SessionTime: at, TimeSource: domain.TimeSourceExplicit}.SourceMeta()
	if live != cand {
		t.Fatalf("live %+v != backfill %+v", live, cand)
	}
	if live.SourceType != "conversation" || live.Author != "Alice" {
		t.Fatalf("precedence = %+v", live)
	}
	doc.SourceKind, doc.AuthorLabel = "", ""
	if m := domain.SourceMetaOf(doc, at, "agent-1"); m.SourceType != "ingest_memory" || m.Author != "agent-1" {
		t.Fatalf("fallback = %+v", m)
	}
	if got := domain.SourceDocumentMetadata(domain.ExternalDocument{SourceKind: "conversation", AuthorLabel: " "}); !reflect.DeepEqual(got, map[string]any{"source_kind": "conversation"}) {
		t.Fatalf("documents metadata = %v", got)
	}
}
