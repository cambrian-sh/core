package chat

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2109 (owner decision 2026-11-18, option B): a reply records the memory records
// its turn rested on, so an erase of any of them can find and redact it.

// recall writes a memory_recall entry for the turn being dispatched, as the
// agent-plane QueryMemory does under the turn's lease (recall_ledger.go).
func (r *groundingRig) recall(turn string, refs ...string) {
	r.ledger.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerToolResult, ToolName: domain.RecallToolName,
		Provenance: domain.LedgerProvenance{ConversationID: "c1", TurnID: turn, StepIndex: -1, Owner: "u1"},
		SourceRefs: refs, Body: []byte(`{"hits":[]}`)})
}

func TestReplyRecordsTheSourcesOfItsTurnsLedgerEntries(t *testing.T) {
	r := newGroundingRig(t, Grounding{})
	r.pool.onDispatch = func(h *domain.Handoff, call int) {
		turn := r.lastUserID()
		r.recall(turn, "chunk-b", "doc-1", "chunk-a")
		// A delegated step's output inherited its recall's refs (and repeats one).
		r.ledger.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerStepOutput,
			Provenance: domain.LedgerProvenance{ConversationID: "c1", TurnID: turn, RunID: "run-1", StepIndex: 0, Owner: "u1"},
			SourceRefs: []string{"doc-1", "exp-9", ""}, Body: []byte("step")})
		// Another turn's recall is not this reply's source.
		r.recall("some-other-turn", "doc-other")
	}
	r.pool.replies = []string{"The agreement number is HERON-b5r1."}
	msg, _ := r.turn(t, "What is the agreement number?")

	want := []string{"chunk-a", "chunk-b", "doc-1", "exp-9"}
	if !reflect.DeepEqual(msg.SourceRefs, want) {
		t.Fatalf("returned reply refs = %v, want %v", msg.SourceRefs, want)
	}
	msgs, _ := r.store.ListMessages(context.Background(), "c1", 0, 0)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want user + reply", len(msgs))
	}
	if msgs[0].Role != domain.MessageRoleUser || len(msgs[0].SourceRefs) != 0 {
		t.Errorf("the person's message must carry no sources: %+v", msgs[0])
	}
	if !reflect.DeepEqual(msgs[1].SourceRefs, want) {
		t.Errorf("stored reply refs = %v, want %v", msgs[1].SourceRefs, want)
	}
}

func TestReplyOfATurnThatRecalledNothingRecordsNoSources(t *testing.T) {
	r := newGroundingRig(t, Grounding{})
	r.pool.replies = []string{"Hello."}
	msg, _ := r.turn(t, "hi")
	if len(msg.SourceRefs) != 0 {
		t.Errorf("refs = %v, want none", msg.SourceRefs)
	}
}

// The sources are read whatever the entry's owner fence: they are ids for the erase,
// never content shown to anyone.
func TestReplySourcesIgnoreTheOwnerFence(t *testing.T) {
	r := newGroundingRig(t, Grounding{})
	r.pool.onDispatch = func(h *domain.Handoff, call int) {
		r.ledger.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerToolResult, ToolName: domain.RecallToolName,
			Provenance: domain.LedgerProvenance{ConversationID: "c1", TurnID: r.lastUserID(), StepIndex: -1, Owner: "someone-else"},
			SourceRefs: []string{"doc-x"}, Body: []byte(`{}`)})
	}
	msg, _ := r.turn(t, "q")
	if !reflect.DeepEqual(msg.SourceRefs, []string{"doc-x"}) {
		t.Errorf("refs = %v, want [doc-x]", msg.SourceRefs)
	}
}

type failingFind struct{ *memLedger }

func (failingFind) Find(context.Context, domain.LedgerQuery) ([]domain.LedgerEntry, error) {
	return nil, errors.New("ledger down")
}

// A ledger that cannot be read does not fail the turn: the reply is stored without
// sources (and the warning says a forget will not reach it).
func TestReplyIsStoredWhenItsSourcesCannotBeRead(t *testing.T) {
	r := newGroundingRig(t, Grounding{})
	r.svc.SetLedger(r.ledger, failingFind{r.ledger})
	r.pool.replies = []string{"answer"}
	msg, _ := r.turn(t, "q")
	if msg.Content != "answer" || len(msg.SourceRefs) != 0 {
		t.Errorf("reply = %+v", msg)
	}
}

// Emit outside a turn (a watch speaking) records no sources.
func TestEmitRecordsNoSources(t *testing.T) {
	r := newGroundingRig(t, Grounding{})
	msg, err := r.svc.Emit(context.Background(), "c1", "heads up")
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.SourceRefs) != 0 {
		t.Errorf("refs = %v", msg.SourceRefs)
	}
}
