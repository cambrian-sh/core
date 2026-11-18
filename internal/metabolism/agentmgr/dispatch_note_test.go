package agentmgr

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// observingCollector is a collector that also observes dispatches, as
// network.Server does (domain.AgentWorkObserver).
type observingCollector struct {
	recordingCollector
	seen []string
}

func (o *observingCollector) AgentInstanceDispatched(agentID, instanceID string, lease domain.LeaseID) {
	o.seen = append(o.seen, agentID+"|"+instanceID+"|"+string(lease))
}

// T-20261118-agent-artifacts-from-planner-lane-steps-carry: NoteDispatch hands
// the handoff's lease to an observing collector; no lease, no instance or a
// collector that does not observe is a no-op.
func TestNoteDispatch_TellsTheObservingCollector(t *testing.T) {
	im := NewInstanceManager("127.0.0.1:1")
	h := &domain.Handoff{Context: map[string]string{handoffLeaseKey: "lease-1"}}
	im.NoteDispatch("probe", "inst-1", h) // no collector yet: nothing to tell

	plain := &recordingCollector{files: map[string]string{}}
	im.SetOutputCollector(plain)
	im.NoteDispatch("probe", "inst-1", h) // a collector that does not observe

	obs := &observingCollector{recordingCollector: recordingCollector{files: map[string]string{}}}
	im.SetOutputCollector(obs)
	im.NoteDispatch("probe", "inst-1", h)
	im.NoteDispatch("probe", "", h)
	im.NoteDispatch("probe", "inst-1", &domain.Handoff{})
	im.NoteDispatch("probe", "inst-1", nil)
	if len(obs.seen) != 1 || obs.seen[0] != "probe|inst-1|lease-1" {
		t.Fatalf("observed %v", obs.seen)
	}
}
