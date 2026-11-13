package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Agent-drafted ontology changes (ADR-0149 D2: "Agents may draft changes; a
// person approves (the existing approval hub)";
// T-20261113-agent-drafted-ontology-changes-have-no-approval, owner option (a)).
//
// An agent never applies a declarations change. It DRAFTS one: the kernel
// classifies it with the same classifier Apply uses, refuses a breaking, invalid
// or empty change at once, persists the draft (append-only, migration 0041) and
// raises an async approval of kind ApprovalKindOntologyChange naming the draft in
// its Object — approval arguments are not persisted, so the draft row is what
// survives a restart. When an eligible person approves, the kernel handler calls
// Ontology.Apply AS THAT PERSON (author = the approver the hub authenticated,
// never the agent). If the namespace moved since the draft and the change no
// longer classifies the same, nothing is applied and the draft is superseded.
// Deny and expiry write no declaration.

// ApprovalKindOntologyChange is the approval kind (and action class) of an
// agent-drafted ontology change. The shipped policy has no row for it, so it
// routes by the "*" fallback (operator) until a deployment names its approvers.
const ApprovalKindOntologyChange = "ontology_change"

// OntologyDraftState is where a draft stands. Pending is the only non-terminal
// state; a draft reaches at most one terminal state.
type OntologyDraftState string

const (
	OntologyDraftPending    OntologyDraftState = "pending"
	OntologyDraftApplied    OntologyDraftState = "applied"
	OntologyDraftRejected   OntologyDraftState = "rejected"
	OntologyDraftExpired    OntologyDraftState = "expired"
	OntologyDraftSuperseded OntologyDraftState = "superseded"
)

// OntologyDraft is one agent-drafted change and its current state.
type OntologyDraft struct {
	ID        string
	Namespace string
	Change    OntologyDoc
	// Proposer is the agent principal the transport resolved ("agent:<id>").
	Proposer string
	// BaseVersion is the namespace's declarations version the draft was
	// classified against; Class and Changes are that classification.
	BaseVersion int64
	Class       OntologyChangeClass
	Changes     []OntologyChange
	Reason      string
	CreatedAt   time.Time

	// State is the latest state; ApprovalID the approval raised for it.
	State      OntologyDraftState
	ApprovalID string
	// StateBy is who moved it to State (the approving person for applied).
	StateBy string
	// Detail explains a terminal state (the denial reason, why it was superseded).
	Detail string
	// AppliedVersion is the declarations version an applied draft became.
	AppliedVersion int64
	StateAt        time.Time
}

// ErrOntologyDraftRefused marks a draft the kernel would not record (breaking,
// invalid, empty, or already decided). The detail is what the agent should see.
var ErrOntologyDraftRefused = errors.New("ontology draft refused")

// OntologyDraftID is the deterministic id of a draft (AGENTS.md §2.6): the same
// namespace, change (canonical: spec order does not matter), proposer and client
// reference always name the same draft, so a retry is a replay.
func OntologyDraftID(namespace string, change OntologyDoc, proposer, clientRef string) (string, error) {
	canon, err := change.Canonical()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(strings.Join([]string{namespace, string(canon), proposer, clientRef}, "\x1f")))
	return "od_" + hex.EncodeToString(h[:16]), nil
}

// ParseOntologyChange decodes a change's JSON strictly: an unknown field (a
// misspelt "requried") is refused rather than silently dropped, because a
// dropped constraint is a change that did not happen. A change that declares
// nothing is refused too. The one parser for every surface that takes a change
// (the operator's ApplyOntologyChange, an agent's draft).
func ParseOntologyChange(raw string) (OntologyDoc, error) {
	var doc OntologyDoc
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return doc, errors.New("change_json: " + err.Error())
	}
	if len(doc.Kinds) == 0 && len(doc.Relations) == 0 {
		return doc, errors.New("change_json declares no kinds and no relations")
	}
	return doc, nil
}
