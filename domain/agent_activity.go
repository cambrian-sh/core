package domain

import (
	"fmt"
	"strings"
	"time"
)

// Agent activity: what an agent is DOING inside a call, while it is doing it.
//
// # Why this is not ProgressUpdate
//
// ADR-0098's ProgressUpdate is a supersedable SNAPSHOT with a closed phase
// vocabulary and no internal identifiers — deliberately, because it reaches end
// users on surfaces whose formatting we do not control, and because a
// twenty-six-round turn must stay one evolving line rather than twenty-six
// messages.
//
// This is the opposite shape on purpose: an APPEND-ONLY record that names the
// tool and its arguments. It exists for operator-facing consoles that are
// inspecting a run — where "which tool, with what, and did it work" is the whole
// question — and it must never be routed to a customer-facing status line.
//
// # Why the session token is the key
//
// A caller that mints a managed-LLM session token already holds the one
// identifier every downstream tool call carries (`lease_id`, historically
// `session_token_id`). Keying on it lets whoever started the work correlate the
// activity back to it without the agent naming anything, and without the kernel
// resolving a conversation that a non-chat caller does not have.
type AgentActivity struct {
	// SessionTokenID correlates this activity with whoever minted the token.
	// Empty when the caller had no managed session, in which case nobody can be
	// listening for it and the emission is skipped.
	SessionTokenID string
	AgentID        string
	// Tool is the name as the kernel's registry knows it, not the sanitized name
	// a provider echoed back.
	Tool string
	// Args is the call's arguments with credential-shaped values removed. Not
	// omitted entirely: "searched for X" is the substance of what an operator is
	// watching, and a redacted map still says which knobs were turned.
	Args map[string]string
	// Done distinguishes the start of a call from its outcome. Both are emitted:
	// a page fetch takes seconds, and a console that only learns about it
	// afterwards is not showing work in progress.
	Done bool
	// Denied and Err describe the outcome, and are only meaningful when Done.
	Denied bool
	Err    string
	At     time.Time
}

// AgentActivityObserver receives agent activity as it happens.
//
// Add-many, like DecisionObserver: several plugins may legitimately want the
// same stream. The implementation MUST return promptly and MUST NOT panic — it
// is called synchronously on the tool path, around a call the agent is waiting
// on.
type AgentActivityObserver interface {
	ObserveAgentActivity(a AgentActivity)
}

// MultiAgentActivityObserver fans one activity out to several observers, so the
// call site stays a single nil check.
type MultiAgentActivityObserver []AgentActivityObserver

func (m MultiAgentActivityObserver) ObserveAgentActivity(a AgentActivity) {
	for _, o := range m {
		if o != nil {
			o.ObserveAgentActivity(a)
		}
	}
}

// credentialish arg names whose VALUES are never carried.
//
// The activity stream reaches an operator console, and an operator configuring
// an ingress is not thereby entitled to read every secret a tool was handed. The
// key still travels, because "it passed an api_key" is worth seeing; the value
// does not.
var credentialish = map[string]bool{
	"api_key": true, "apikey": true, "key": true, "token": true,
	"access_token": true, "auth": true, "authorization": true,
	"secret": true, "password": true, "passwd": true, "pwd": true,
	"credential": true, "session": true, "signature": true, "sig": true,
}

// RedactArgs renders tool arguments for an activity record.
//
// Values are truncated as well as filtered: a scrape result or a pasted document
// can be megabytes, and an activity stream is a status feed, not a copy of the
// payload.
func RedactArgs(args map[string]any) map[string]string {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]string, len(args))
	for k, v := range args {
		if credentialish[strings.ToLower(k)] {
			out[k] = "[redacted]"
			continue
		}
		s := stringifyArg(v)
		if len(s) > 160 {
			s = s[:160] + "…"
		}
		out[k] = s
	}
	return out
}

func stringifyArg(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}
