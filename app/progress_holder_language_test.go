package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type langSink struct{ got []domain.ProgressUpdate }

func (s *langSink) Progress(_ context.Context, u domain.ProgressUpdate) { s.got = append(s.got, u) }

// An emitter that does not know the conversation's language (the planner's step progress)
// is rendered in the language the chat turn stated, until the turn ends
// (T-20261107-chat-progress-status-line-phases-are-english).
func TestProgressHolder_FillsTheTurnLanguage(t *testing.T) {
	h := &progressHolder{}
	sink := &langSink{}
	h.set(sink)
	var texts []string
	h.setFeed(func(e domain.DomainEvent) {
		if p, ok := e.(domain.ConversationProgressEvent); ok {
			texts = append(texts, p.Text)
		}
	})
	ctx := context.Background()
	h.Progress(ctx, domain.ProgressUpdate{ConversationID: "c1", Phase: domain.PhaseWorking, Language: "tr"})
	h.Progress(ctx, domain.ProgressUpdate{ConversationID: "c1", Phase: domain.PhasePlanning}) // planner, no language
	h.Progress(ctx, domain.ProgressUpdate{ConversationID: "c2", Phase: domain.PhasePlanning}) // another conversation
	h.Progress(ctx, domain.ProgressUpdate{ConversationID: "c1", Final: true})
	h.Progress(ctx, domain.ProgressUpdate{ConversationID: "c1", Phase: domain.PhasePlanning}) // next turn, not yet stated

	want := []string{"tr", "tr", "", "tr", ""}
	for i, u := range sink.got {
		if u.Language != want[i] {
			t.Errorf("update %d language = %q, want %q", i, u.Language, want[i])
		}
	}
	if len(texts) != 5 || texts[1] != "iş planlanıyor" || texts[2] != "planning the work" {
		t.Errorf("operator feed texts = %q", texts)
	}
}
