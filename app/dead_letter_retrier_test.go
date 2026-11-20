package app

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type stubRetrier struct{ got []string }

func (s *stubRetrier) RetryDeadLetter(_ context.Context, id string) error {
	s.got = append(s.got, id)
	return nil
}

// ENG-1654: until a plugin registers a retrier the holder answers
// ErrDeadLetterRetryUnavailable and reports none available; a registration lands in
// it and every later retry reaches the plugin.
func TestDeadLetterRetrierHolder_LateBound(t *testing.T) {
	h := &deadLetterRetrierHolder{}
	if h.RetryAvailable() || !errors.Is(h.RetryDeadLetter(context.Background(), "dl-1"), domain.ErrDeadLetterRetryUnavailable) {
		t.Fatal("an empty holder must say retry is unavailable")
	}
	h.set(nil) // ignored
	s := &stubRetrier{}
	h.set(s)
	if !h.RetryAvailable() || h.RetryDeadLetter(context.Background(), "dl-1") != nil || len(s.got) != 1 || s.got[0] != "dl-1" {
		t.Fatalf("registered retrier not reached: %v", s.got)
	}
}
