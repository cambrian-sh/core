package domain

import "testing"

type recordingObserver struct{ seen []RetrievalDecision }

func (r *recordingObserver) ObserveRetrieval(d RetrievalDecision) { r.seen = append(r.seen, d) }

func TestMultiDecisionObserver_FansOutInOrder(t *testing.T) {
	a, b := &recordingObserver{}, &recordingObserver{}
	m := MultiDecisionObserver{a, b}
	m.ObserveRetrieval(RetrievalDecision{QueryID: "q1"})
	for name, o := range map[string]*recordingObserver{"a": a, "b": b} {
		if len(o.seen) != 1 || o.seen[0].QueryID != "q1" {
			t.Errorf("observer %s did not receive the decision: %+v", name, o.seen)
		}
	}
}

func TestMultiDecisionObserver_SkipsNilEntries(t *testing.T) {
	// A nil entry must not take down the retrieval path: the fan-out runs on the
	// hottest code in the kernel, where a panic is a kernel outage, not a log line.
	a := &recordingObserver{}
	m := MultiDecisionObserver{nil, a, nil}
	m.ObserveRetrieval(RetrievalDecision{QueryID: "q1"})
	if len(a.seen) != 1 {
		t.Fatalf("live observer skipped: %+v", a.seen)
	}
}

func TestMultiDecisionObserver_EmptyIsNoop(t *testing.T) {
	MultiDecisionObserver{}.ObserveRetrieval(RetrievalDecision{})
}
