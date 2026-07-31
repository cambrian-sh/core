package domain

// Session isolation (BRAIN-01).
//
// This answers a DIFFERENT question from TagPredicate, and keeping the two apart
// is the whole design:
//
//	TagPredicate     — may this principal see this CLASS of thing?
//	SessionIsolation — does this belong to the conversation I am answering?
//
// Folding them into one predicate makes both unauditable, and there is a sharper
// reason than tidiness: the tag path matches `metadata.tags` against a CONTROLLED
// VOCABULARY, and a session id is an identity, not a classification. Expressing
// isolation as a tag would mean coining `session:<uuid>` per conversation —
// exactly the coinage ADR-0099 rejects, and the mistake that once renamed every
// document and took recall to 0.0.
//
// So isolation gets its own predicate over its own field. Documents already carry
// `metadata["session_id"]` (MetaSessionID), stamped at commit time, so this needs
// no new column and no migration — only a predicate that was never written.
//
// Why it was needed: the kernel had a full caller-scope mechanism
// (Session.CallerScope, SessionManager.CreateScopedSession/SetCallerScope,
// app.Options.SessionScopes, and the premium authorizer's per-session term) and
// NOTHING in production ever called the writers, so every session's caller scope
// was empty and the term contributed nothing. The only session-aware filter on the
// read path did the opposite of isolation: it dropped the current session's own
// step records and kept everyone else's.

// SessionIsolation restricts a read to material the answering conversation is
// entitled to see.
//
// The zero value is NOT usable — it denies everything, which is deliberate. Build
// one with IsolateTo or IsolationBypass so that "what isolation is this read
// running under" is always a decision somebody made rather than a field somebody
// forgot.
type SessionIsolation struct {
	// SessionID is the conversation being answered. Documents stamped with a
	// DIFFERENT session id are excluded.
	SessionID SessionID

	// IncludeUnowned admits documents carrying no session id at all — the ingested
	// corpus, imported knowledge, anything not produced by a conversation.
	//
	// True in every normal read, and it is what makes isolation a predicate rather
	// than a store reset: shared knowledge stays shared, only conversation-owned
	// material is fenced. Setting it false answers the much narrower question
	// "what did THIS conversation produce".
	IncludeUnowned bool

	// Bypass admits everything, including other sessions' material. It is for
	// kernel-internal maintenance reads and for deployments that are deliberately
	// single-conversation — the same role Bypass plays on TagPredicate.
	//
	// Named and greppable on purpose: `IsolationBypass()` at a call site is a
	// decision a reviewer can see, where a nil or a zero value is an omission
	// nobody notices.
	Bypass bool
}

// IsolateTo restricts a read to one conversation plus unowned material.
func IsolateTo(sid SessionID) *SessionIsolation {
	return &SessionIsolation{SessionID: sid, IncludeUnowned: true}
}

// IsolationBypass admits everything. Use where a read genuinely spans
// conversations — kernel maintenance, operator inspection, a single-conversation
// deployment — and never merely to make a call site compile.
func IsolationBypass() *SessionIsolation {
	return &SessionIsolation{Bypass: true}
}

// IsZero reports whether no isolation decision was made at all.
func (s *SessionIsolation) IsZero() bool {
	return s == nil || (!s.Bypass && s.SessionID == "" && !s.IncludeUnowned)
}

// Allows reports whether a document's metadata passes this isolation predicate.
//
// This is the in-memory form and it is AUTHORITATIVE; the SQL builder in the
// postgres adapter mirrors it, and the two must agree. A nil predicate DENIES —
// the fail-closed direction — so a caller that forgot to decide gets nothing
// rather than everything.
func (s *SessionIsolation) Allows(meta map[string]interface{}) bool {
	if s == nil {
		return false
	}
	if s.Bypass {
		return true
	}
	owner := DocSessionID(meta)
	if owner == "" {
		return s.IncludeUnowned
	}
	return SessionID(owner) == s.SessionID
}
