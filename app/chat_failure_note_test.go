package app

import (
	"errors"
	"strings"
	"testing"
)

// The behaviour under test is "the person waiting on the turn learns something they can act
// on". The quota case is the one that motivated it: it was previously indistinguishable from
// a hang.
func TestChatFailureNote_Classification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string // substring the note must contain
	}{
		{
			name: "provider quota — the case that looked like a hang",
			err:  errors.New(`openai: 429 {"error":{"type":"GoUsageLimitError","message":"Weekly usage limit reached. Resets in 2 days."}}`),
			want: "usage limit is reached",
		},
		{"openai insufficient_quota", errors.New("insufficient_quota: you exceeded your current quota"), "usage limit is reached"},
		{"anthropic credit", errors.New("your credit balance is too low"), "usage limit is reached"},
		{"bad key", errors.New("401 Unauthorized: invalid api key"), "rejected our credentials"},
		{"missing model", errors.New(`model not found: "deepseek-v9"`), "not available from its provider"},
		{"slow", errors.New("context deadline exceeded"), "did not answer in time"},
		{"unreachable", errors.New("dial tcp 127.0.0.1:11434: connection refused"), "could not be reached"},
		{"breaker", errors.New("no healthy generator for role planner"), "temporarily unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chatFailureNote(tc.err)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("note %q does not contain %q", got, tc.want)
			}
		})
	}
}

// A quota refusal frequently ALSO mentions a deadline, because the retry ladder burns the
// caller's budget before giving up. Reporting that as a timeout sends the operator to look
// at latency instead of at their bill, so the quota classification must win.
func TestChatFailureNote_QuotaBeatsDeadlineWhenBothPresent(t *testing.T) {
	err := errors.New("context deadline exceeded after 429 Weekly usage limit reached")
	got := chatFailureNote(err)
	if !strings.Contains(got, "usage limit is reached") {
		t.Fatalf("quota must take precedence over the deadline wording, got %q", got)
	}
}

func TestChatFailureNote_NilIsEmptySoAGoodTurnClearsTheLine(t *testing.T) {
	// Empty note on a final update means CLEAR (ADR-0098). A non-empty string here would
	// leave a failure line up after a turn that succeeded.
	if got := chatFailureNote(nil); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

// ADR-0098's Text() contract: these lines reach surfaces we do not control the formatting
// of, so they must carry no markup, no internal identifiers and no transport-interpretable
// punctuation. An unrecognised error must therefore NOT be echoed through.
func TestChatFailureNote_DoesNotLeakTheRawErrorOnTheDefaultPath(t *testing.T) {
	raw := `pq: duplicate key value violates unique constraint "agent_scopes_pkey" host=10.1.2.3 token=sk-abc123`
	got := chatFailureNote(errors.New(raw))
	for _, leak := range []string{"pq:", "agent_scopes_pkey", "10.1.2.3", "sk-abc123", `"`} {
		if strings.Contains(got, leak) {
			t.Fatalf("note leaked %q from the raw error: %q", leak, got)
		}
	}
	if got == "" {
		t.Fatal("an unrecognised failure must still say SOMETHING — silence is the original bug")
	}
}
