package config

import "testing"

// ADR-0107 stage 3a: the projection dual-write is opt-in, and the read switch
// does not exist yet. A default-on write would put a second INSERT on every
// embedding write of every deployment before the arm that prices it has run.
func TestDefaultConfig_EmbeddingProjectionWriteDisabled(t *testing.T) {
	if DefaultConfig().Execution.Retrieval.EmbeddingProjectionWrite {
		t.Fatal("embedding_projection_write must default OFF (ADR-0107 D3)")
	}
}

func TestDefaultConfig_EmbeddingProjectionReadDisabled(t *testing.T) {
	if DefaultConfig().Execution.Retrieval.EmbeddingProjectionRead {
		t.Fatal("embedding_projection_read must default OFF (ADR-0107 D3)")
	}
}

// The read switch changes where the ranking's vectors come from; two runs that
// differ only in it must NOT share a fingerprint (ADR-0107 D3 / ADR-0103 D7).
func TestRetrievalFingerprint_ChangesWithProjectionRead(t *testing.T) {
	a := DefaultConfig().Execution
	b := DefaultConfig().Execution
	b.Retrieval.EmbeddingProjectionRead = true
	if a.RetrievalFingerprint("bge-large/1024") == b.RetrievalFingerprint("bge-large/1024") {
		t.Fatal("projection read flag must change the retrieval fingerprint")
	}
}
