package domain

import "testing"

// A task-session document reports its session and no ingest thread.
func TestDocSessionID_StepRecord(t *testing.T) {
	meta := map[string]interface{}{
		MetaSourceAgent: "System",
		MetaSessionID:   "task-session-1",
	}
	if got := DocSessionID(meta); got != "task-session-1" {
		t.Errorf("DocSessionID = %q, want %q", got, "task-session-1")
	}
	if got := DocIngestThreadID(meta); got != "" {
		t.Errorf("a System step record has no ingest thread, got %q", got)
	}
}

// A freshly-written ingest chunk reports its thread and — crucially — NO session, so
// nothing that filters, counts or scopes by session mistakes corpus for a run's output.
func TestDocIngestThreadID_NewChunk(t *testing.T) {
	meta := map[string]interface{}{
		MetaSourceAgent:    "importer",
		MetaIngestThreadID: "musique:q17::p3",
	}
	if got := DocIngestThreadID(meta); got != "musique:q17::p3" {
		t.Errorf("DocIngestThreadID = %q, want %q", got, "musique:q17::p3")
	}
	if got := DocSessionID(meta); got != "" {
		t.Errorf("an ingest chunk must report NO task session, got %q", got)
	}
}

// Rows written before the key split kept their ingest thread in session_id. Reads still
// understand them; the distinguishing signal is that their producer is not "System".
func TestDocIngestThreadID_LegacyChunk(t *testing.T) {
	meta := map[string]interface{}{
		MetaSourceAgent: "importer",
		MetaSessionID:   "musique:q17::p3",
	}
	if got := DocIngestThreadID(meta); got != "musique:q17::p3" {
		t.Errorf("legacy ingest row not understood: got %q", got)
	}
}

// The legacy fallback must not swallow a real step record: a System-authored document's
// session is a session, never an ingest thread.
func TestDocIngestThreadID_LegacyFallbackSkipsStepRecords(t *testing.T) {
	meta := map[string]interface{}{
		MetaSourceAgent: "System",
		MetaSessionID:   "task-session-9",
	}
	if got := DocIngestThreadID(meta); got != "" {
		t.Errorf("a System step record must never read as an ingest thread, got %q", got)
	}
}

func TestDocMetadata_NilAndEmpty(t *testing.T) {
	if got := DocSessionID(nil); got != "" {
		t.Errorf("DocSessionID(nil) = %q", got)
	}
	if got := DocIngestThreadID(nil); got != "" {
		t.Errorf("DocIngestThreadID(nil) = %q", got)
	}
	if got := DocIngestThreadID(map[string]interface{}{}); got != "" {
		t.Errorf("DocIngestThreadID(empty) = %q", got)
	}
}

// Non-string values must not panic or leak a bogus ID.
func TestDocMetadata_WrongTypes(t *testing.T) {
	meta := map[string]interface{}{MetaSessionID: 42, MetaIngestThreadID: []string{"x"}}
	if got := DocSessionID(meta); got != "" {
		t.Errorf("non-string session_id must yield \"\", got %q", got)
	}
	if got := DocIngestThreadID(meta); got != "" {
		t.Errorf("non-string ingest_thread_id must yield \"\", got %q", got)
	}
}
