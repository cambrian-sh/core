package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type recordingRetirer struct {
	ids []string
	err error
}

func (r *recordingRetirer) RetireChunks(_ context.Context, ids []string) (domain.DocumentCascadeCounts, error) {
	r.ids = append(r.ids, ids...)
	return domain.DocumentCascadeCounts{}, r.err
}

// ENG-1535: with a chunk retirer the decay deletes through it (never the bare
// vector store), counts only a successful retirement, and spares what it
// spared before (a reinforced or recently read row).
func TestDecayLoner_DeletesThroughTheChunkRetirer(t *testing.T) {
	r := &recordingRetirer{}
	a := NewAgent(nil, 0) // no Manager: the vector-store path must not be reached
	a.Chunks = r
	var n int
	a.decayLoner(context.Background(), domain.Document{ID: "c1", ActivationStrength: 0.1}, map[string]bool{}, false, &n)
	a.decayLoner(context.Background(), domain.Document{ID: "c2", ActivationStrength: 0.9}, map[string]bool{}, false, &n)
	a.decayLoner(context.Background(), domain.Document{ID: "c3", ActivationStrength: 0.1, AccessCount: 5}, map[string]bool{}, false, &n)
	if n != 1 || len(r.ids) != 1 || r.ids[0] != "c1" {
		t.Fatalf("deleted %d via %v, want only c1", n, r.ids)
	}
	r.err = errors.New("db down")
	a.decayLoner(context.Background(), domain.Document{ID: "c4", ActivationStrength: 0.1}, map[string]bool{}, false, &n)
	if n != 1 {
		t.Fatalf("a failed retirement was counted: %d", n)
	}
}
