package operator

import (
	"strings"
	"testing"
)

// Every event the mapper can render must be BRIDGED to the feed.
//
// Three separate features have now shipped with a publisher, a mapper case, a
// projection fold and a rendered column — and stayed silently blank because the
// event type was missing from `feedEventTypes`. ScoutUsefulness, then retention,
// then the live pipeline meters. The comments in that list record the first two;
// this test is what makes the third the last.
//
// It reads the mapper's own source rather than a hand-kept list, so a mapper
// case added tomorrow is covered without anybody remembering to update a test.
func TestFeedBridge_EveryMappedEventIsDelivered(t *testing.T) {
	bridged := make(map[string]bool, len(feedEventTypes))
	for _, t := range feedEventTypes {
		bridged[t] = true
	}

	for _, ev := range mappedEventTypes(t) {
		if ev == "" {
			// The mapper case has no resolvable EventType constant; this test
			// reports only what it can prove.
			continue
		}
		if liveOnlyLanes[ev] {
			continue
		}
		if !bridged[ev] {
			t.Errorf("event %q has a mapper case but is not in feedEventTypes — "+
				"it will be built, published and silently never delivered", ev)
		}
	}
}

// liveOnlyLanes are mapped events that deliberately do NOT go through the
// bridge.
//
// They are emitted straight onto the ephemeral lane with seq 0 and are never
// replayed: a token chunk, a reasoning exchange, and a "working on it" status
// line. Replaying a progress line for a turn that finished an hour ago would be
// worse than showing no progress at all, which is why they bypass the spool.
//
// Listed explicitly so the exemption is a decision with a reason, not a gap.
var liveOnlyLanes = map[string]bool{
	"token.chunk":           true,
	"agent.llm_exchange":    true,
	"conversation.progress": true,
}

// mappedEventTypes returns the EventType() constant of every domain event the
// mapper switches on, read from the source.
func mappedEventTypes(t *testing.T) []string {
	t.Helper()
	src := readSource(t, "mapper.go")
	var out []string
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		// `case domain.XEvent:` — the mapper's own switch.
		if !strings.HasPrefix(line, "case domain.") || !strings.HasSuffix(line, "Event:") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(line, "case domain."), "Event:")
		out = append(out, eventTypeConstantFor(t, name))
	}
	if len(out) == 0 {
		t.Fatalf("read no mapper cases — this test has stopped checking anything")
	}
	return out
}

// eventTypeConstantFor resolves `FooEvent` to the string its EventType() returns,
// by reading the constant the domain package declares for it.
func eventTypeConstantFor(t *testing.T, name string) string {
	t.Helper()
	src := readSource(t, "../../../domain/event.go")
	needle := "func (" + name + "Event) EventType() string { return EventType"
	i := strings.Index(src, needle)
	if i < 0 {
		// Some events are mapped without a dedicated constant; skip rather than
		// fail, so this test reports only what it can prove.
		return ""
	}
	rest := src[i+len(needle):]
	constName := "EventType" + rest[:strings.Index(rest, " }")]
	// Now find `ConstName = "value"`.
	j := strings.Index(src, constName+" = \"")
	if j < 0 {
		return ""
	}
	tail := src[j+len(constName)+4:]
	return tail[:strings.Index(tail, "\"")]
}
