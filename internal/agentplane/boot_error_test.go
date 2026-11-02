package agentplane

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261102-kg-extractor-cold-start-drops-the-first-kg: a failed boot reaches
// the caller WITH its cause. GetOrBootInstance used to return a bare nil, so the
// transport could only say "boot agent X failed" and the kg_extractor's
// cold-start socket timeout was invisible.
func TestCallAgent_BootFailureCarriesCause(t *testing.T) {
	cause := errors.New("agent kg_extractor_agent: socket did not appear within 10s")
	a := New(&bootFailDialer{err: cause})
	_, err := a.CallAgent(context.Background(), "kg_extractor_agent", &domain.Handoff{}, "")
	if !errors.Is(err, cause) {
		t.Fatalf("CallAgent error %v does not wrap the boot cause", err)
	}
}

type bootFailDialer struct {
	countingDialer
	err error
}

func (d *bootFailDialer) GetOrBootInstance(context.Context, *domain.AgentDefinition, string) (*domain.Instance, error) {
	return nil, d.err
}
