package app

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type fixedStreams struct{}

func (fixedStreams) StreamState(string) string { return domain.StreamQuarantined }
func (fixedStreams) StreamRefcount(string) int { return 3 }

// The operator service holds this holder from boot; a plugin's registration
// (RegisterStreamRegistry) lands later. Before it — or with none — the answers
// are exactly an unwired kernel's: unknown and -1 (ENG-250).
func TestStreamRegistryHolder(t *testing.T) {
	h := &streamRegistryHolder{}
	if h.StreamState("s") != domain.StreamUnknown || h.StreamRefcount("s") != -1 {
		t.Fatalf("unset holder: %q %d", h.StreamState("s"), h.StreamRefcount("s"))
	}
	h.set(nil) // ignored
	if h.StreamState("s") != domain.StreamUnknown {
		t.Fatal("a nil registration must not replace the cannot-tell answer")
	}
	h.set(fixedStreams{})
	if h.StreamState("s") != domain.StreamQuarantined || h.StreamRefcount("s") != 3 {
		t.Fatalf("registered: %q %d", h.StreamState("s"), h.StreamRefcount("s"))
	}
}
