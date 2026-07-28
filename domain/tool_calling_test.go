package domain

import (
	"context"
	"testing"
)

// "Finished" is a WHITELIST — Anthropic's rule is "when stop_reason is NOT end_turn,
// treat the response as incomplete". A blacklist is what the SDK loop had, and it read
// a narrating model as a finished one.
func TestStopReason_OnlyEndTurnIsFinished(t *testing.T) {
	finished := map[StopReason]bool{
		StopEndTurn:   true,
		StopToolUse:   false,
		StopMaxTokens: false,
		StopSequence:  false,
		StopRefusal:   false,
		StopUnknown:   false,
	}
	for reason, want := range finished {
		if got := reason.IsFinished(); got != want {
			t.Errorf("%q.IsFinished() = %v, want %v", reason, got, want)
		}
	}
}

// The load-bearing default: a signal we do not recognise must mean "keep going".
// A new or mis-mapped provider value silently meaning "done" is the whole failure
// class this package exists to remove.
func TestStopReason_UnknownIsNotFinished(t *testing.T) {
	if StopUnknown.IsFinished() {
		t.Fatal("an unrecognised stop reason must never count as finished")
	}
	if !(ModelTurn{StopReason: StopUnknown}).ShouldContinue() {
		t.Fatal("an unrecognised stop reason must continue the loop")
	}
}

func TestModelTurn_ShouldContinue(t *testing.T) {
	cases := []struct {
		name string
		turn ModelTurn
		want bool
	}{
		{"tool call pending", ModelTurn{StopReason: StopToolUse,
			ToolCalls: []ModelToolCall{{Name: "write_file"}}}, true},
		{"clean finish", ModelTurn{Text: "done", StopReason: StopEndTurn}, false},
		{"truncated", ModelTurn{Text: "half", StopReason: StopMaxTokens}, true},
		// The opencode #14972 case: Gemini and LiteLLM return finish_reason "stop"
		// on responses that DO carry tool calls. A loop trusting the declared reason
		// alone runs one tool and halts. The action outranks the narration.
		{"provider says stop but sent a tool call", ModelTurn{StopReason: StopEndTurn,
			ToolCalls: []ModelToolCall{{Name: "write_file"}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.turn.ShouldContinue(); got != c.want {
				t.Errorf("ShouldContinue() = %v, want %v", got, c.want)
			}
		})
	}
}

// textOnlyGen implements Generator and nothing else.
type textOnlyGen struct{}

func (textOnlyGen) Generate(context.Context, string) (string, error) { return "ok", nil }

// toolGen implements both.
type toolGen struct{ textOnlyGen }

func (toolGen) GenerateWithTools(context.Context, []ModelMessage, []ToolDefinition) (ModelTurn, error) {
	return ModelTurn{StopReason: StopEndTurn}, nil
}

// SupportsToolCalling is the assertion every caller branches on, so both answers are
// pinned. A capability that reports "yes" and then refuses is worse than no capability.
func TestSupportsToolCalling(t *testing.T) {
	if SupportsToolCalling(textOnlyGen{}) {
		t.Error("a text-only generator must not report tool-calling support")
	}
	if !SupportsToolCalling(toolGen{}) {
		t.Error("a tool-calling generator must report support")
	}
}
