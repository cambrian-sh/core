package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// The post_model view of a model reply's tool calls (ADR-0139 D11; ticket
// T-20261106-post-model-hooks-never-see-native-tool-call).
//
// On the native tool lane the step's deliverable is not the reply text: it is the
// answer ARGUMENT of the configured final-answer tool call (ADR-0139 D3 — narration
// written around the call never reaches the user). A post_model hook that saw only
// the text could neither redact nor refuse what an agent actually answers, so the
// event carries the reply's tool calls (read-only) and the deliverable as a
// first-class field a may_rewrite hook can rewrite.
//
// The deliverable generalises to GUARDED ARGUMENTS (ticket
// T-20261106-post-model-hooks-do-not-guard-control-verb): the string arguments of
// the SDK's control verbs that leave the reply without ever reaching the
// ToolExecutor — yield_subgoal's intent becomes a plan step, delegate_to_planner's
// task a new Execute request, a memory note's content and a proposed skill's body
// are persisted. Which tool/argument pairs are guarded is configuration
// (execution.hooks.guarded_arguments; the SDK's vocabulary, Zero-Hardcode Rule),
// and the final answer is always among them.
//
// Only guarded arguments are rewritable here. Every OTHER tool call is executed —
// or refused — by the ToolExecutor, whose pre_tool hooks see and may rewrite its
// arguments; rewriting them twice, in two places, would be one policy expressed
// twice.

// HookToolCall is one tool call of a model reply, as a post_model hook sees it.
// Read-only: a hook rewrites the deliverable through HookResult.FinalAnswer.
type HookToolCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// HookFinalAnswer is the reply's deliverable: the string argument Arg of the call
// to Tool (execution.step_output.final_answer_tool / final_answer_arg), at index
// Call of HookEvent.ToolCalls.
type HookFinalAnswer struct {
	Tool string `json:"tool"`
	Arg  string `json:"arg"`
	Call int    `json:"call"`
	// Value is the deliverable (empty when Unreadable is set).
	Value string `json:"value"`
	// Unreadable says why the deliverable could not be read as a string: the
	// call's arguments are not a JSON object, the argument is missing or not a
	// string, or the reply carries more than one final-answer call. The kernel then
	// cannot apply a rewrite, so a may_rewrite post_model hook DENIES the reply
	// instead of running (fail closed, whatever its on_failure): a declared
	// redaction that cannot be applied must not pass the unredacted value.
	Unreadable string `json:"unreadable,omitempty"`
}

// errDeliverableUnwritable marks a failure that leaves the deliverable — or a
// guarded argument — possibly unredacted. HookChain.Run denies on it regardless
// of the hook's on_failure.
var errDeliverableUnwritable = errors.New("a guarded argument cannot be rewritten")

// GuardedArg names one guarded outbound argument: the string argument Arg of every
// call to Tool in a model reply.
type GuardedArg struct {
	Tool string `json:"tool"`
	Arg  string `json:"arg"`
}

// HookGuardedArg is one guarded argument of a reply, as a post_model hook sees it:
// the argument Arg of the call at index Call of HookEvent.ToolCalls (a call to Tool).
type HookGuardedArg struct {
	Tool string `json:"tool"`
	Arg  string `json:"arg"`
	Call int    `json:"call"`
	// Value is the argument (empty when Unreadable is set).
	Value string `json:"value"`
	// Unreadable says why the argument could not be read as a string: the call's
	// arguments are not a JSON object, or the argument is not a string. As for the
	// deliverable, a may_rewrite post_model hook then DENIES the reply instead of
	// running (fail closed, whatever its on_failure).
	Unreadable string `json:"unreadable,omitempty"`
}

// HookArgRewrite is a may_rewrite post_model hook's new value for one guarded
// argument, named as HookEvent.GuardedArgs lists it.
type HookArgRewrite struct {
	Call  int    `json:"call"`
	Arg   string `json:"arg"`
	Value string `json:"value"`
}

// HookGuardedView lists a reply's guarded arguments, in call order. The
// deliverable (finalTool's finalArg) is read strictly, as HookReplyView reads it: a
// final-answer call without a string answer is Unreadable. A configured control-verb
// argument is read leniently: a call that omits it (or sends null) carries nothing
// to guard and gets no entry, but arguments that are not a JSON object, or a
// present non-string value, are Unreadable. A guard repeating the deliverable is
// ignored (listed once).
func HookGuardedView(calls []ModelToolCall, finalTool, finalArg string, guards []GuardedArg) []HookGuardedArg {
	var out []HookGuardedArg
	for i, c := range calls {
		if finalTool != "" && c.Name == finalTool {
			g := HookGuardedArg{Tool: c.Name, Arg: finalArg, Call: i}
			if v, err := ToolCallStringArg(c.Arguments, finalArg); err != nil {
				g.Unreadable = err.Error()
			} else {
				g.Value = v
			}
			out = append(out, g)
		}
		for _, gd := range guards {
			if gd.Tool != c.Name || gd.Arg == "" || (c.Name == finalTool && gd.Arg == finalArg) {
				continue
			}
			g := HookGuardedArg{Tool: c.Name, Arg: gd.Arg, Call: i}
			v, present, err := optionalStringArg(c.Arguments, gd.Arg)
			switch {
			case err != nil:
				g.Unreadable = err.Error()
			case !present:
				continue
			default:
				g.Value = v
			}
			out = append(out, g)
		}
	}
	return out
}

// optionalStringArg reads an optional string argument: absent or null ⇒ !present.
// Empty arguments (a call with no argument blob at all) carry nothing either.
func optionalStringArg(args []byte, arg string) (string, bool, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		return "", false, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil || obj == nil {
		return "", false, errors.New("the call's arguments are not a JSON object")
	}
	raw, ok := obj[arg]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false, nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false, fmt.Errorf("the %q argument is not a string", arg)
	}
	return s, true, nil
}

// HookReplyView builds the post_model view of a reply's tool calls: every call
// (read-only) and the deliverable of the call named tool (nil when the reply makes
// no such call, or tool is empty).
func HookReplyView(calls []ModelToolCall, tool, arg string) ([]HookToolCall, *HookFinalAnswer) {
	if len(calls) == 0 {
		return nil, nil
	}
	view := make([]HookToolCall, len(calls))
	var fa *HookFinalAnswer
	for i, c := range calls {
		view[i] = HookToolCall{ID: c.ID, Name: c.Name, Args: append(json.RawMessage(nil), c.Arguments...)}
		if tool == "" || c.Name != tool {
			continue
		}
		if fa != nil {
			// Two deliverables: rewriting one would pass the other unredacted.
			fa.Value, fa.Unreadable = "", "the reply carries more than one "+tool+" call"
			continue
		}
		fa = &HookFinalAnswer{Tool: tool, Arg: arg, Call: i}
		v, err := ToolCallStringArg(c.Arguments, arg)
		if err != nil {
			fa.Unreadable = err.Error()
		} else {
			fa.Value = v
		}
	}
	return view, fa
}

// ToolCallStringArg reads the string argument arg of a tool call's JSON arguments.
func ToolCallStringArg(args []byte, arg string) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil || obj == nil {
		return "", errors.New("the call's arguments are not a JSON object")
	}
	raw, ok := obj[arg]
	if !ok {
		return "", fmt.Errorf("the call has no %q argument", arg)
	}
	var s string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &s) != nil {
		return "", fmt.Errorf("the %q argument is not a string", arg)
	}
	return s, nil
}

// SetToolCallStringArg returns args with the string argument arg set to value. The
// other fields are kept (re-encoded compactly; key order is not preserved, which
// JSON does not promise), and the result is a valid JSON object. HTML characters
// are not escaped, so the value round-trips byte for byte.
func SetToolCallStringArg(args []byte, arg, value string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil || obj == nil {
		return nil, errors.New("the call's arguments are not a JSON object")
	}
	v, err := marshalNoEscape(value)
	if err != nil {
		return nil, err
	}
	obj[arg] = v
	return marshalNoEscape(obj)
}

func marshalNoEscape(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
