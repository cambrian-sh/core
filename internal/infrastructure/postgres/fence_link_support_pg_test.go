package postgres

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2049 against a real migrated Postgres (PG_TEST_DSN), owner decision
// 2026-11-18 option (a): a derived link is visible only to a reader allowed to
// read EVERY evidence row it rests on — its cited evidence and each id in
// links.supporting_evidence (ENG-2032). The links under test cite PUBLIC
// evidence (seedShared's [mail] row) and carry no row classification, so before
// this change every reader who may read [mail] saw them; only their supporting
// evidence — restricted by a POLICY tag (hr) and a party list, not an owner tag —
// can hide them now.
//
// The property is the typed-row suite's equality: every answer a barred reader
// gets (query plane incl. entity and alias closure, review inbox, catalog,
// listings, identity scans) is byte-identical before and after the supported
// links are written, and the guard in front of the link writes (LinkReadable)
// and the pair inbox (ReviewPairs / ReadablePair) answer a hidden link exactly
// as an absent one. A granted reader — nothing forbids hr, or one of the
// evidence's parties — sees every one of them. The pair inbox and the closure
// agree before and after a pair-level confirmation.
//
// The file name sorts before migration_guard_test.go on purpose (that test drops
// and recreates the public schema).

const (
	lsEvSupport = "ownleak-ev-support-hr" // the restricted supporting evidence
	lsAlias     = "ownleak/alias-supported"
	lsCand      = "ownleak/cand-supported"
	lsParty     = "person:ownleak-carol"
	lsProducer  = "ownleak-support@1"
)

var (
	// A party-scoped hr reader who IS a party to the supporting evidence.
	lsPartyCarol = &domain.TagPredicate{Owners: []string{"bob"}, PartyScopedTags: []string{trHRTag},
		PartyIdentities: []string{lsParty}}
)

// seedSupportEvidence writes the restricted supporting evidence: [mail, hr] with
// carol as its only party.
func (f *slFixture) seedSupportEvidence(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification, parties)
		VALUES ($1,'default','ownleak-src-support',$1,'sha256:test','{mail,hr}',$2)`,
		lsEvSupport, []string{lsParty}); err != nil {
		t.Fatalf("support evidence: %v", err)
	}
}

// seedSupportedLinks writes, citing the public evidence, a confirmed alias and a
// candidate of the shared entity, each derived also from support (the supporting
// set). No other row names the two refs, so any answer naming them came through
// a link.
func (f *slFixture) seedSupportedLinks(t *testing.T, support []domain.EvidenceID) {
	t.Helper()
	link := func(to, state, mechanism string) {
		if _, err := f.links.PutLink(f.ctx, domain.Link{
			NamespaceID: "default", Family: domain.LinkFamilyIdentity,
			FromRef: domain.EntityRef(slShared), ToRef: domain.EntityRef(to),
			Relation: domain.RelationSameAs, State: state, Mechanism: mechanism,
			EvidenceID: trEvPub, AssertedBy: "test", Producer: lsProducer,
			SourceRef: "ownleak-support-" + to, SupportingEvidence: support,
		}); err != nil {
			t.Fatalf("link →%s: %v", to, err)
		}
	}
	link(lsAlias, domain.LinkStateConfirmed, domain.LinkMechanismDeclared)
	link(lsCand, domain.LinkStateCandidate, domain.LinkMechanismScored)
}

// supportSnapshot is typedSnapshot plus the pair inbox.
func (f *slFixture) supportSnapshot(t *testing.T, scope *domain.TagPredicate, pubEvent string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(f.typedSnapshot(t, scope, pubEvent))
	pairs, err := f.plane.ReviewPairs(f.ctx, "default", domain.MaxQueryLimit, scope)
	fmt.Fprintf(&b, "review pairs: %v err=%v\n", pairKeys(pairs), err)
	return b.String()
}

func pairKeys(pairs []domain.LinkPair) []string {
	var out []string
	for _, p := range pairs {
		out = append(out, fmt.Sprintf("%s→%s cand=%d conf=%d", p.Key.FromRef, p.Key.ToRef, len(p.Candidates), len(p.Confirmed)))
	}
	sort.Strings(out)
	return out
}

// supportedLinkID is the id of the seeded link to ref (by its source ref: the
// store canonicalises the pair's ends).
func (f *slFixture) supportedLinkID(t *testing.T, to string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM links WHERE producer = $1 AND source_ref = $2`,
		lsProducer, "ownleak-support-"+to).Scan(&id); err != nil {
		t.Fatalf("link id →%s: %v", to, err)
	}
	return id
}

func TestPg_LinkSupport_RestrictedSupportingEvidenceHidesTheLinkEverywhere(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	f.seedShared(t)
	f.seedSupportEvidence(t)
	pubEvent := f.eventID(t, "ownleak-"+trEvPub+"-"+slPubType)
	barred := []struct {
		name  string
		scope *domain.TagPredicate
	}{
		{"hr closed (policy tag, not granted)", trNoGrant},
		{"hr party-scoped, not a party", trNotParty},
	}
	granted := []struct {
		name  string
		scope *domain.TagPredicate
	}{
		{"nothing forbids hr", trGranted},
		{"hr party-scoped, a party", lsPartyCarol},
	}
	before := map[string]string{}
	for _, r := range barred {
		before[r.name] = f.supportSnapshot(t, r.scope, pubEvent)
	}
	for _, r := range granted {
		before[r.name] = f.supportSnapshot(t, r.scope, pubEvent)
	}

	f.seedSupportedLinks(t, []domain.EvidenceID{lsEvSupport})
	aliasID, candID := f.supportedLinkID(t, lsAlias), f.supportedLinkID(t, lsCand)
	candKey := domain.LinkPairKey{NamespaceID: "default", Family: domain.LinkFamilyIdentity,
		FromRef: domain.EntityRef(slShared), ToRef: domain.EntityRef(lsCand), Relation: domain.RelationSameAs}

	for _, r := range barred {
		after := f.supportSnapshot(t, r.scope, pubEvent)
		if after != before[r.name] {
			t.Errorf("%s: links resting on restricted supporting evidence changed this reader's answers.\n%s",
				r.name, diffLines(before[r.name], after))
		}
		for _, s := range []string{lsAlias, lsCand} {
			if strings.Contains(after, s) {
				t.Errorf("%s: answer names %q", r.name, s)
			}
		}
		for _, id := range []string{aliasID, candID} {
			hidden, herr := f.plane.LinkReadable(f.ctx, "default", id, r.scope)
			absent, aerr := f.plane.LinkReadable(f.ctx, "default", "lnk_ownleak_no_such", r.scope)
			if hidden || herr != nil || fmt.Sprint(hidden, herr) != fmt.Sprint(absent, aerr) {
				t.Errorf("%s: LinkReadable(%s) = %v/%v, want the absent answer %v/%v", r.name, id, hidden, herr, absent, aerr)
			}
		}
		if pr, err := f.plane.ReadablePair(f.ctx, candKey, r.scope); err != nil || len(pr.Candidates)+len(pr.Confirmed) != 0 {
			t.Errorf("%s: ReadablePair(cand) = %+v err=%v, want nothing readable", r.name, pr, err)
		}
	}
	for _, r := range granted {
		after := f.supportSnapshot(t, r.scope, pubEvent)
		if after == before[r.name] {
			t.Fatalf("%s: the supported links are invisible to a reader allowed every evidence row", r.name)
		}
		for _, s := range []string{lsAlias, lsCand} {
			if !strings.Contains(after, s) {
				t.Errorf("%s: answer never names %q", r.name, s)
			}
		}
		for _, id := range []string{aliasID, candID} {
			if ok, err := f.plane.LinkReadable(f.ctx, "default", id, r.scope); !ok || err != nil {
				t.Errorf("%s: LinkReadable(%s) = %v/%v, want readable", r.name, id, ok, err)
			}
		}
		if pr, err := f.plane.ReadablePair(f.ctx, candKey, r.scope); err != nil || len(pr.Candidates) != 1 {
			t.Errorf("%s: ReadablePair(cand) = %+v err=%v, want its candidate", r.name, pr, err)
		}
	}

	// The pair inbox and the closure agree after a pair-level confirmation: the
	// confirmation inherits the candidate's supporting evidence (confirmTx), so
	// the newly joined alias enters the granted reader's closure and stays out of
	// the barred reader's, and the pair leaves both inboxes.
	pr, err := f.plane.ReadablePair(f.ctx, candKey, trGranted)
	if err != nil || len(pr.Candidates) != 1 {
		t.Fatalf("granted ReadablePair: %+v err=%v", pr, err)
	}
	if _, err := f.links.ConfirmLinks(f.ctx, "default", []string{pr.Candidates[0].ID}, "reviewer"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	closure := func(scope *domain.TagPredicate) string {
		res, err := f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryEntity, EntityID: slShared, ExpandAliases: true}, scope)
		if err != nil {
			t.Fatalf("closure: %v", err)
		}
		return fmt.Sprint(res.Rows)
	}
	inbox := func(scope *domain.TagPredicate) string {
		pairs, err := f.plane.ReviewPairs(f.ctx, "default", domain.MaxQueryLimit, scope)
		if err != nil {
			t.Fatalf("pairs: %v", err)
		}
		return fmt.Sprint(pairKeys(pairs))
	}
	for _, r := range barred {
		if c := closure(r.scope); strings.Contains(c, lsCand) || strings.Contains(c, lsAlias) {
			t.Errorf("%s: closure after confirmation names a supported alias: %s", r.name, c)
		}
		if strings.Contains(inbox(r.scope), lsCand) {
			t.Errorf("%s: inbox offers the confirmed pair", r.name)
		}
	}
	for _, r := range granted {
		c := closure(r.scope)
		if !strings.Contains(c, lsCand) || !strings.Contains(c, lsAlias) {
			t.Errorf("%s: closure after confirmation misses a supported alias: %s", r.name, c)
		}
		if strings.Contains(inbox(r.scope), lsCand) {
			t.Errorf("%s: inbox still offers the confirmed pair", r.name)
		}
	}
}

// Narrow-only: a supporting set every reader may read changes nothing — the
// answers with readable support are byte-identical to the same links' answers
// once their supporting set is emptied (same ids, same times).
func TestPg_LinkSupport_ReadableSupportReadsAsNone(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	f.seedShared(t)
	pubEvent := f.eventID(t, "ownleak-"+trEvPub+"-"+slPubType)
	// [mail, owner:bob]: every bob reader below may read it.
	f.seedSupportedLinks(t, []domain.EvidenceID{"ownleak-ev-bob"})
	readers := map[string]*domain.TagPredicate{"no grant": trNoGrant, "not party": trNotParty, "granted": trGranted}
	with := map[string]string{}
	for name, scope := range readers {
		with[name] = f.supportSnapshot(t, scope, pubEvent)
	}
	// ScopeSystem holds no owner tag: owner:bob support hides the links from the
	// operator plane — the supporting fence keeps the owner fence (ADR-0121).
	if sys := f.supportSnapshot(t, domain.ScopeSystem, pubEvent); strings.Contains(sys, lsAlias) || strings.Contains(sys, lsCand) {
		t.Errorf("system: a link resting on owner:bob evidence is visible to the operator plane")
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE links SET supporting_evidence = '{}' WHERE producer = $1`, lsProducer); err != nil {
		t.Fatalf("empty the supporting sets: %v", err)
	}
	for name, scope := range readers {
		without := f.supportSnapshot(t, scope, pubEvent)
		if with[name] != without {
			t.Errorf("%s: readable supporting evidence changed the answers:\n%s", name, diffLines(without, with[name]))
		}
		if !strings.Contains(without, lsAlias) || !strings.Contains(without, lsCand) {
			t.Errorf("%s: unsupported links invisible: the fixture proves nothing", name)
		}
	}
	if sys := f.supportSnapshot(t, domain.ScopeSystem, pubEvent); !strings.Contains(sys, lsAlias) {
		t.Errorf("system: with no supporting set the links must be visible again")
	}
}
