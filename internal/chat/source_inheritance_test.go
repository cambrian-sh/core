package chat

import (
	"reflect"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/replycheck"
)

// ENG-2165 (owner decision 2026-11-19, option B): a reply inherits the preceding agent
// reply's OWN records always, and every other given record only when it repeats one
// of the distinctive values of what carried it.

func agentMsg(content string, refs, own []string) domain.Message {
	return domain.Message{Role: domain.MessageRoleAgent, Content: content, SourceRefs: refs, OwnSourceRefs: own}
}

func TestHistorySources_PrecedingReplyOwnAlwaysEverythingElseOnOverlap(t *testing.T) {
	quoting := agentMsg("Agreement HERON-77 is signed.", []string{"doc-d"}, []string{"doc-d"})
	bridge := agentMsg("Glad to help.", []string{"doc-d", "doc-e"}, []string{"doc-e"})
	history := []domain.Message{
		{Role: domain.MessageRoleUser, Content: "what is the agreement?"},
		quoting,
		{Role: domain.MessageRoleUser, Content: "thanks"},
		bridge,
	}
	got := historySources(history)
	if len(got) != 2 {
		t.Fatalf("sources = %+v, want the two agent replies", got)
	}
	// Newest first: the bridge is the preceding reply — its own refs always, its
	// inherited ones on overlap; the quoting reply is older — all on overlap.
	if !reflect.DeepEqual(got[0].always, []string{"doc-e"}) || !reflect.DeepEqual(got[0].onOverlap, []string{"doc-d"}) {
		t.Fatalf("preceding reply source = %+v", got[0])
	}
	if got[1].always != nil || !reflect.DeepEqual(got[1].onOverlap, []string{"doc-d"}) {
		t.Fatalf("older reply source = %+v", got[1])
	}

	opt := replycheck.Options{MinInteger: 1000}
	if r := inheritedRefs("Anything else today?", got, opt); !reflect.DeepEqual(r, []string{"doc-e"}) {
		t.Fatalf("an unrelated reply inherits %v, want only the preceding reply's own doc-e", r)
	}
	if r := inheritedRefs("As I said, HERON-77.", got, opt); !reflect.DeepEqual(r, []string{"doc-d", "doc-e"}) {
		t.Fatalf("a reply copying the value inherits %v, want doc-d too", r)
	}
}

func TestHistorySources_RedactedOrRefLessPrecedingReplyPassesNothingOn(t *testing.T) {
	now := time.Now()
	quoting := agentMsg("Agreement HERON-77 is signed.", []string{"doc-d"}, []string{"doc-d"})
	redacted := agentMsg(domain.RedactedReplyText, []string{"doc-x"}, []string{"doc-x"})
	redacted.RedactedAt = &now
	got := historySources([]domain.Message{quoting, redacted})
	if len(got) != 1 || got[0].always != nil {
		t.Fatalf("sources = %+v: a redacted preceding reply passes nothing, and the older one is not promoted to one hop", got)
	}
	if r := inheritedRefs("Hello again.", got, replycheck.Options{}); r != nil {
		t.Fatalf("inherited %v, want nothing", r)
	}
}

func TestHistorySources_LegacyRowCountsEveryRefAsOwn(t *testing.T) {
	// A row stored before migration 0063 reads OwnSourceRefs = SourceRefs (the store).
	legacy := agentMsg("Anything.", []string{"doc-a", "doc-b"}, []string{"doc-a", "doc-b"})
	got := historySources([]domain.Message{legacy})
	if !reflect.DeepEqual(got[0].always, []string{"doc-a", "doc-b"}) || got[0].onOverlap != nil {
		t.Fatalf("legacy preceding reply source = %+v, want every ref always", got[0])
	}
}

func TestInheritedRefs_SummaryAndIndexOnlyOnOverlap(t *testing.T) {
	sources := []givenSource{
		{text: "Earlier: the invoice total was 48,250.00 EUR.", onOverlap: []string{"doc-inv"}},
		{text: "turn 3: request CUS-0042 status", onOverlap: []string{"doc-cus"}},
	}
	opt := replycheck.Options{MinInteger: 1000}
	if r := inheritedRefs("Sure, here is a general answer.", sources, opt); r != nil {
		t.Fatalf("inherited %v from a summary and index it repeats nothing of", r)
	}
	if r := inheritedRefs("The total stays 48,250 EUR.", sources, opt); !reflect.DeepEqual(r, []string{"doc-inv"}) {
		t.Fatalf("inherited %v, want the summary's doc-inv only", r)
	}
	if r := broadRefs(sources); !reflect.DeepEqual(r, []string{"doc-cus", "doc-inv"}) {
		t.Fatalf("broadRefs = %v", r)
	}
}
