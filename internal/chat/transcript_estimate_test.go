package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2127: the transcript budget counts with the kernel's content-aware
// estimator, so a pasted base64/hex blob costs what a provider will bill
// (~1.3 chars/token), not len/4. Under the old private len/4 copy the 256-char
// blob below cost 68 and the window kept both messages.
func TestBudgetWindowCountsABlobAtItsRealCost(t *testing.T) {
	msgs := []domain.Message{
		{Seq: 1, Content: strings.Repeat("a1B2", 64)}, // 197 tokens + 4
		{Seq: 2, Content: "ok"},
	}
	over, win := budgetWindow(msgs, 120)
	if len(over) != 1 || over[0].Seq != 1 || len(win) != 1 || win[0].Seq != 2 {
		t.Fatalf("overflow %v window %v, want the blob folded", seqs(over), seqs(win))
	}
}

// Through a real turn (TurnService, store, summarizer): a person pastes a blob,
// the next turn's transcript budget folds it into the summary instead of
// handing the model a window that does not fit.
func TestATurnFoldsAPastedBlobThatOverrunsTheBudget(t *testing.T) {
	r := newGroundingRig(t, Grounding{TranscriptBudgetTokens: 120})
	var folded [][]int64
	r.svc.SetSummarizer(func(_ context.Context, _ string, msgs []domain.Message) (string, error) {
		folded = append(folded, seqs(msgs))
		return "summary", nil
	})
	r.pool.replies = []string{"ok", "ok"}
	r.turn(t, strings.Repeat("a1B2", 64))
	_, h := r.turn(t, "second")
	if len(folded) != 1 || len(folded[0]) == 0 || folded[0][0] != 1 {
		t.Fatalf("summarised = %v, want the blob (message 1) folded", folded)
	}
	if tr := h.Payload.Metadata["transcript"]; strings.Contains(tr, "a1B2a1B2") {
		t.Errorf("the blob is still in the transcript: %q", tr)
	}
}

func seqs(msgs []domain.Message) []int64 {
	out := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Seq)
	}
	return out
}
