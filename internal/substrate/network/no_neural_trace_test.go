package network

import (
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1462 (owner decision 2026-11-14): agent neural traces are deleted
// completely — not memory, not telemetry. The server used to hold the raw tier-0
// vector store only to write them (unclassified, around the tier-2 chokepoint,
// AGENTS.md §2.2). This guard keeps any raw store from coming back into the
// server: a server field of type domain.VectorStore is a second write pathway
// that skips MemoryStack.WriteStore.
func TestServer_HoldsNoRawVectorStore(t *testing.T) {
	vs := reflect.TypeOf((*domain.VectorStore)(nil)).Elem()
	st := reflect.TypeOf(Server{})
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if f.Type == vs {
			t.Errorf("Server.%s is a raw domain.VectorStore; principal-serving writes go through MemoryStack.WriteStore (ENG-1462)", f.Name)
		}
	}
}
