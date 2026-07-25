package domain

import (
	"encoding/json"
	"testing"
)

// The identity types must survive JSON round-tripping as plain strings, because they cross
// the storage and proto edges that way.
func TestIDs_MarshalAsPlainStrings(t *testing.T) {
	type payload struct {
		Session SessionID `json:"session"`
		Run     RunID     `json:"run"`
		Lease   LeaseID   `json:"lease"`
	}
	b, err := json.Marshal(payload{Session: "s1", Run: "r1", Lease: "l1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(b), `{"session":"s1","run":"r1","lease":"l1"}`; got != want {
		t.Errorf("marshal = %s, want %s", got, want)
	}

	var back payload
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Session != "s1" || back.Run != "r1" || back.Lease != "l1" {
		t.Errorf("round-trip lost values: %+v", back)
	}
}

// Document metadata is map[string]interface{} — an UNTYPED edge. A typed ID stored there
// satisfies no `.(string)` assertion, so every reader silently misses.
//
// This is not hypothetical: typing pendingItem.SessionID caused exactly this, and the
// Tier-2 tagging tests caught it. The rule the failure taught: convert to string when
// writing into metadata, and DocSessionID reads it back.
func TestSessionID_MustBeStringInDocumentMetadata(t *testing.T) {
	sid := SessionID("task-session-1")

	wrong := map[string]interface{}{MetaSessionID: sid}
	if got := DocSessionID(wrong); got != "" {
		t.Fatalf("a TYPED id in metadata should not satisfy the string read; got %q", got)
	}

	right := map[string]interface{}{MetaSessionID: string(sid)}
	if got := DocSessionID(right); got != "task-session-1" {
		t.Errorf("DocSessionID = %q, want %q", got, "task-session-1")
	}
}

// LeaseBinding's zero value must never read as an identity — an unbound lease grants
// nothing.
func TestLeaseBinding_IsZero(t *testing.T) {
	if !(LeaseBinding{}).IsZero() {
		t.Error("empty binding must be zero")
	}
	if !(LeaseBinding{StepIndex: 3}).IsZero() {
		t.Error("a step index alone is not identity — binding must still be zero")
	}
	if (LeaseBinding{SessionID: "s"}).IsZero() {
		t.Error("a binding with a session is not zero")
	}
	if (LeaseBinding{RunID: "r"}).IsZero() {
		t.Error("a binding with a run is not zero")
	}
	if (LeaseBinding{AgentID: "a"}).IsZero() {
		t.Error("a binding with an agent is not zero")
	}
}

func TestIDs_String(t *testing.T) {
	if SessionID("s").String() != "s" || RunID("r").String() != "r" || LeaseID("l").String() != "l" {
		t.Error("String() must return the underlying value")
	}
}
