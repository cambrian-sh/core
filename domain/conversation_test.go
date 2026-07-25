package domain

import "testing"

// ADR-0084 D7: customer-facing conversations are tools-only. This is the property that
// stops an external end user pulling on the tenant's shared knowledge, so it is asserted
// directly rather than left to an agent default someone can flip.
func TestConversationProfile_Recall(t *testing.T) {
	cases := map[ConversationProfile]bool{
		ProfileOperator: true,
		ProfileEmployee: true,
		ProfileCustomer: false,
	}
	for p, want := range cases {
		if got := p.Recall(); got != want {
			t.Errorf("%s.Recall() = %v, want %v", p, got, want)
		}
	}
}

// An unknown profile must never be treated as a usable default (fail-closed).
func TestConversationProfile_Valid(t *testing.T) {
	for _, p := range []ConversationProfile{ProfileOperator, ProfileEmployee, ProfileCustomer} {
		if !p.Valid() {
			t.Errorf("%s should be valid", p)
		}
	}
	for _, p := range []ConversationProfile{"", "admin", "Operator"} {
		if p.Valid() {
			t.Errorf("%q should NOT be valid", p)
		}
	}
}

func TestMessageRole_Valid(t *testing.T) {
	for _, r := range []MessageRole{MessageRoleUser, MessageRoleAgent, MessageRoleSystem} {
		if !r.Valid() {
			t.Errorf("%s should be valid", r)
		}
	}
	for _, r := range []MessageRole{"", "assistant", "User"} {
		if r.Valid() {
			t.Errorf("%q should NOT be valid", r)
		}
	}
}

func validConversation() Conversation {
	return Conversation{ID: "c1", OwnerID: "u1", Status: ConversationOpen, Profile: ProfileEmployee}
}

func TestConversation_Validate(t *testing.T) {
	if err := validConversation().Validate(); err != nil {
		t.Fatalf("valid conversation rejected: %v", err)
	}

	tests := map[string]func(*Conversation){
		"missing ID":      func(c *Conversation) { c.ID = "  " },
		"missing OwnerID": func(c *Conversation) { c.OwnerID = "" },
		"bad status":      func(c *Conversation) { c.Status = "archived" },
		"bad profile":     func(c *Conversation) { c.Profile = "root" },
	}
	for name, mutate := range tests {
		c := validConversation()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func validMessage() Message {
	return Message{ID: "m1", ConversationID: "c1", Role: MessageRoleUser, Content: "hello"}
}

func TestMessage_Validate(t *testing.T) {
	if err := validMessage().Validate(); err != nil {
		t.Fatalf("valid message rejected: %v", err)
	}

	tests := map[string]func(*Message){
		"missing ID":             func(m *Message) { m.ID = "" },
		"missing ConversationID": func(m *Message) { m.ConversationID = " " },
		"bad role":               func(m *Message) { m.Role = "assistant" },
		"empty content":          func(m *Message) { m.Content = "" },
	}
	for name, mutate := range tests {
		m := validMessage()
		mutate(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

// Seq is assigned by the store, so a caller-supplied Seq must not be required — validation
// deliberately ignores it.
func TestMessage_Validate_IgnoresSeq(t *testing.T) {
	m := validMessage()
	m.Seq = 0
	if err := m.Validate(); err != nil {
		t.Fatalf("Seq=0 must be valid (the store assigns it): %v", err)
	}
}
