package domain

import "testing"

func meta(sid string) map[string]interface{} {
	if sid == "" {
		return map[string]interface{}{"text": "x"}
	}
	return map[string]interface{}{MetaSessionID: sid}
}

// The failure BRAIN-01 exists to fix: another conversation's material must not be
// readable. This is the assertion the whole issue reduces to.
func TestIsolation_OtherSessionIsExcluded(t *testing.T) {
	iso := IsolateTo("sess-A")
	if iso.Allows(meta("sess-B")) {
		t.Fatal("a document owned by another session was admitted")
	}
	if !iso.Allows(meta("sess-A")) {
		t.Fatal("the conversation's own material was excluded")
	}
}

// Shared knowledge stays shared. This is what makes isolation a PREDICATE rather
// than a store reset: an ingested corpus carries no session id and must remain
// visible, or every deployment loses its knowledge base the day isolation ships.
func TestIsolation_UnownedCorpusStaysVisible(t *testing.T) {
	if !IsolateTo("sess-A").Allows(meta("")) {
		t.Fatal("unowned corpus material was excluded")
	}
	narrow := &SessionIsolation{SessionID: "sess-A"}
	if narrow.Allows(meta("")) {
		t.Fatal("IncludeUnowned=false still admitted unowned material")
	}
}

// A nil predicate DENIES. Mutation-checked: this fails the moment the nil guard in
// Allows is removed, which is the point — a caller that forgot to decide must get
// nothing, not everything.
func TestIsolation_NilDenies(t *testing.T) {
	var iso *SessionIsolation
	for _, m := range []map[string]interface{}{meta(""), meta("sess-A"), nil} {
		if iso.Allows(m) {
			t.Fatalf("nil isolation admitted %v — the fail-open direction", m)
		}
	}
}

// Bypass is explicit and total.
func TestIsolation_Bypass(t *testing.T) {
	b := IsolationBypass()
	for _, m := range []map[string]interface{}{meta(""), meta("sess-A"), meta("sess-B")} {
		if !b.Allows(m) {
			t.Fatalf("bypass excluded %v", m)
		}
	}
	if b.IsZero() {
		t.Fatal("an explicit bypass must not read as 'no decision made'")
	}
}

// The zero value is a decision nobody made, and must be distinguishable from one
// somebody did make.
func TestIsolation_ZeroValueIsNotADecision(t *testing.T) {
	var nilIso *SessionIsolation
	if !nilIso.IsZero() {
		t.Fatal("nil should read as no decision")
	}
	if !(&SessionIsolation{}).IsZero() {
		t.Fatal("the zero value should read as no decision")
	}
	if IsolateTo("sess-A").IsZero() {
		t.Fatal("an explicit isolation read as no decision")
	}
}

// Isolation must not be confused with classification. A document tagged for a
// session-shaped tag is NOT isolated by that tag — identity is not a
// classification (ADR-0099), and this test pins the separation.
func TestIsolation_IgnoresTags(t *testing.T) {
	m := map[string]interface{}{
		"tags":        []interface{}{"session:sess-B", "sales"},
		MetaSessionID: "sess-A",
	}
	if !IsolateTo("sess-A").Allows(m) {
		t.Fatal("isolation consulted tags instead of the session field")
	}
}
