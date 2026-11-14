package chat

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// A refused token acquisition (exhausted budget, unbindable lease) must not be
// discarded: the turn still runs, as before, but the cause is logged so the
// lease-less turn's later failures can be traced back to it
// (T-20261113-chat-turn-silently-runs-with-no-llm-lease-when, AGENTS.md §3.2).
// Not parallel: it swaps the default slog logger.
func TestTurn_TokenAcquisitionFailureIsLoggedNotDiscarded(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	svc, _, _ := setup(t, domain.ProfileOperator)
	binder := &recordingBinder{}
	svc.SetLeaseBinder(binder, "chat_agent")
	svc.acquireToken = func(context.Context, int, time.Duration) (string, func(), error) {
		return "", nil, errors.New("session budget exhausted")
	}

	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"}); err != nil {
		t.Fatalf("turn: %v (a refused lease must not fail the turn)", err)
	}
	if len(binder.bound) != 0 {
		t.Fatalf("no lease was issued, yet %d were bound", len(binder.bound))
	}
	out := buf.String()
	if !strings.Contains(out, "WITHOUT an LLM lease") || !strings.Contains(out, "session budget exhausted") {
		t.Fatalf("the refused acquisition was not logged with its cause; log = %q", out)
	}
}
