package domain

// Document metadata keys that carry IDENTITY.
//
// These are named constants rather than inline strings because the key `session_id` was, for
// a long time, four different things at once: a task session, an ingestion thread, a
// synthetic retrieval-loop ID, and (in benchmark corpora) a source document's identity. Any
// reader that asked "which session wrote this?" got a truthful answer only some of the time.
//
// The rule going forward: one key, one meaning. A value that is not a task session does not
// go in MetaSessionID.
const (
	// MetaSessionID is the durable task Session that produced this document — and nothing
	// else. Written for agent/step records under a live session; absent otherwise.
	MetaSessionID = "session_id"

	// MetaIngestThreadID is the ingestion thread a corpus chunk arrived on
	// (domain.IngestDocument.ThreadID). It groups chunks by their SOURCE, which is a
	// completely different question from "which run produced this", and it used to be
	// written into MetaSessionID — so ingested corpus looked, to every reader, like the
	// output of some session.
	MetaIngestThreadID = "ingest_thread_id"

	// MetaSourceAgent is the producer of the document. "System" marks the kernel's own
	// auto-recorded step output, which is what the ADR-0048 D1 self-recall filter keys on.
	MetaSourceAgent = "source_agent"
)

// DocIngestThreadID returns the ingestion thread of a document, tolerating documents written
// before the key split.
//
// Back-compat is read-only and deliberately one-way: rows stored under the old key are still
// understood, but nothing writes MetaSessionID for an ingest thread again. The fallback is
// safe because a document carrying MetaIngestThreadID never also carries an ingest thread in
// MetaSessionID — the writer emits exactly one of them.
func DocIngestThreadID(meta map[string]interface{}) string {
	if meta == nil {
		return ""
	}
	if v, ok := meta[MetaIngestThreadID].(string); ok && v != "" {
		return v
	}
	// Legacy rows: an ingest chunk is identifiable because its producer is the document's
	// author, never the kernel's "System" step recorder.
	if src, _ := meta[MetaSourceAgent].(string); src != "System" {
		if v, ok := meta[MetaSessionID].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// DocSessionID returns the TASK SESSION that produced a document, or "".
//
// It deliberately does NOT fall back to any other key: a caller asking "which session wrote
// this?" must get "" for an ingested corpus chunk rather than an ingestion thread ID that
// merely looks like an answer.
func DocSessionID(meta map[string]interface{}) string {
	if meta == nil {
		return ""
	}
	v, _ := meta[MetaSessionID].(string)
	return v
}
