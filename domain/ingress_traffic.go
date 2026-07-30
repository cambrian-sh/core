package domain

import (
	"context"
	"time"
)

// InboundContact is one person's traffic through an entry point.
//
// It answers the question the operator console's "recent inbound" list is
// actually asking — *who came through my bot, what did they say, and did they get
// an answer* — from the durable conversation record.
//
// That list was previously built from the access-decision journal, and could not
// answer it. Two reasons, both structural: the journal is held in memory and
// starts empty on every restart, and it only records when something ASKS the
// decision point. A conversational turn that answers a greeting touches no
// governed resource, so it makes no access decision and left no trace — the
// panel was empty for exactly the traffic it existed to show.
//
// Access decisions remain the right source for *what policy did*; they are
// merged in as an overlay rather than being the spine of the list.
type InboundContact struct {
	ConversationID string
	IngressAgentID string
	// ExternalID is the sender's address on the far side, as the ingress knows it.
	ExternalID string
	FirstSeen  time.Time
	LastSeen   time.Time
	// MessageCount counts what the PERSON sent, not the total exchange. An
	// operator judging whether someone is worth binding cares how much they
	// asked, not how much the system replied.
	MessageCount int
	// LastText is the most recent thing they said. Present because the
	// conversation store records message bodies — unlike the decision journal,
	// which is why this list can finally show what was asked.
	LastText string
	// Answered reports whether the last inbound message has a reply after it. A
	// turn that failed silently leaves this false, which is the one outcome the
	// old list could never surface.
	Answered bool
}

// IngressTrafficLister reports who has come through an entry point.
//
// A narrow read-only port rather than a method on ConversationStore: the console
// needs one aggregate query, and widening the store interface would oblige every
// implementation and every test fake to grow a method none of them use.
type IngressTrafficLister interface {
	// ListIngressTraffic returns recent contacts through one ingress, newest
	// first. An empty ingressAgentID means every ingress.
	ListIngressTraffic(ctx context.Context, ingressAgentID string, limit int) ([]InboundContact, error)
}
