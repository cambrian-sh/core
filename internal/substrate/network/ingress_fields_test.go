package network

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The payload shape is the SDK's: Ingress.receive(external_id, text, **extra)
// builds {"external_id", "text"} and then does payload.update(extra), so an
// ingress's extra kwargs arrive as sibling keys. The Telegram bridge has been
// sending speaker_id and speaker_name that way all along; the kernel was
// dropping them, which is why the unbound worklist could only show bare ids.
func handoff(payload string) *domain.Handoff {
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(payload)}}
}

func TestIngressFields_CarriesTheSpeakerTheBridgeAlreadySends(t *testing.T) {
	m := ingressFields(handoff(`{
		"external_id": "tg:6484759603",
		"text": "hello",
		"chat_type": "private",
		"speaker_id": 6484759603,
		"speaker_name": "afsin",
		"message_id": 42
	}`))

	if m.ExternalID != "tg:6484759603" || m.Text != "hello" {
		t.Fatalf("base fields lost: %+v", m)
	}
	// A NUMBER on the wire. A string-tagged field would have silently decoded to
	// empty and the worklist would still be nameless.
	if m.SpeakerID != "6484759603" {
		t.Fatalf("speaker id not carried, got %q", m.SpeakerID)
	}
	if m.Username != "afsin" {
		t.Fatalf("speaker name not carried, got %q", m.Username)
	}
}

func TestIngressFields_SpeakerIsOptional(t *testing.T) {
	// A bridge that reports no speaker is a thinner row, not a failure — the
	// message must still be accepted and delivered.
	m := ingressFields(handoff(`{"external_id": "ex:1", "text": "hi"}`))

	if m.ExternalID != "ex:1" || m.Text != "hi" {
		t.Fatalf("a bridge without speaker fields must still deliver: %+v", m)
	}
	if m.SpeakerID != "" || m.Username != "" {
		t.Fatalf("invented a speaker from nothing: %+v", m)
	}
}

func TestIngressFields_StringSpeakerIDAlsoWorks(t *testing.T) {
	// Not every platform numbers its users. Accepting both shapes means a bridge
	// author does not have to know which one this decoder happened to pick.
	m := ingressFields(handoff(`{"external_id": "x:1", "text": "hi", "speaker_id": "U0431"}`))

	if m.SpeakerID != "U0431" {
		t.Fatalf("string speaker id not carried, got %q", m.SpeakerID)
	}
}

func TestIngressFields_MalformedPayloadYieldsNothing(t *testing.T) {
	// Not a panic and not a half-filled message: an undecodable payload is not an
	// inbound message, and Accept rejects the empty one on its own terms.
	if m := ingressFields(handoff(`{not json`)); m.ExternalID != "" || m.Text != "" {
		t.Fatalf("garbage decoded into a message: %+v", m)
	}
}

// Telegram ids already exceed what a float64 represents exactly. Decoding
// through one would round a user id into a DIFFERENT person's, which is the
// worst possible failure here: the binding would still work, it would just be
// somebody else's.
func TestIngressFields_LargeIDSurvivesExactly(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1, unrepresentable as a float64
	m := ingressFields(handoff(`{"external_id": "tg:1", "text": "hi", "speaker_id": ` + big + `}`))

	if m.SpeakerID != big {
		t.Fatalf("id lost precision: want %s, got %s", big, m.SpeakerID)
	}
}
