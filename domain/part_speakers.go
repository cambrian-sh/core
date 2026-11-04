package domain

// ADR-0136 Amendment A3.3: speakers survive chunking; the chunks themselves do not
// change. A multi-part ingest records each part's byte span in the body and its
// caller-given speaker (DocumentPart); every chunk is then labelled with the speaker
// of the part holding its first character and the distinct speakers it overlaps.
// The labels are chunk METADATA — never chunk text — so document ids, chunk ids,
// chunk text, embeddings and KG triplets are identical with or without them.

// Chunk metadata keys written by the ingest path for a multi-part document whose
// parts carry speakers. The fact extractor reads MetaSpeakerAtStart as passage data
// (A3.4); both live ingest and facts-backfill read it from the stored chunk.
const (
	// MetaSpeakerAtStart is the speaker of the part containing the chunk's first
	// character (string).
	MetaSpeakerAtStart = "speaker_at_start"
	// MetaSpeakers is the distinct speakers whose parts the chunk overlaps, in body
	// order ([]string).
	MetaSpeakers = "speakers"
)

// ChunkSpeaker is one chunk's speaker labels; the zero value means "none known".
type ChunkSpeaker struct {
	AtStart  string
	Speakers []string
}

// ChunkSpeakers labels each chunk from its located span (LocateChunkSpans, the same
// spans ChunkStamps uses — computed against the body before any context prepend).
// A chunk that was not located, or that overlaps no labelled part, gets the zero
// value: an unknown speaker is stated as unknown, never guessed from a neighbour.
//
// AtStart is the speaker of the part that holds span.Start. When span.Start falls
// outside every part (on the blank-line separator between two parts — chunkers trim,
// so this needs a chunk that begins with the separator), it is the speaker of the
// first part the chunk overlaps, which is the part it actually begins with.
func ChunkSpeakers(doc ExternalDocument, spans []ChunkSpan) []ChunkSpeaker {
	out := make([]ChunkSpeaker, len(spans))
	if !hasSpeakers(doc.Parts) {
		return out
	}
	for i, sp := range spans {
		if !sp.OK {
			continue
		}
		var cs ChunkSpeaker
		seen := map[string]bool{}
		atStartSet := false
		for _, p := range doc.Parts {
			if p.Length == 0 {
				continue // an empty part contributes no text, so nobody speaks in the chunk through it
			}
			end := p.Offset + p.Length
			// Half-open overlap of [sp.Start, sp.End) and [p.Offset, end).
			if !(sp.Start < end && p.Offset < sp.End) {
				continue
			}
			if !atStartSet {
				// The first overlapping part in body order holds span.Start, or is the
				// first part after it when span.Start sits on a separator.
				cs.AtStart = p.Speaker
				atStartSet = true
			}
			if p.Speaker != "" && !seen[p.Speaker] {
				seen[p.Speaker] = true
				cs.Speakers = append(cs.Speakers, p.Speaker)
			}
		}
		out[i] = cs
	}
	return out
}

func hasSpeakers(parts []DocumentPart) bool {
	for _, p := range parts {
		if p.Speaker != "" {
			return true
		}
	}
	return false
}
