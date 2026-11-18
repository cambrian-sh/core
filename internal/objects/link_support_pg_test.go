package objects_test

import (
	"fmt"
	"sort"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2049 (owner decision 2026-11-18, option a) in the objects reads: a link is
// visible only to a reader allowed to read EVERY evidence row it rests on — its
// cited evidence and each id in links.supporting_evidence (sqlfence.LinkExists).
// Both links cite south evidence and carry no row classification; o1→c1 also
// rests on restricted evidence (cls:restricted, closed for south readers).
// ListObjects link filters and GetObject links hide it from south and show it to
// a reader allowed the restricted evidence; with the supporting set emptied the
// two readers agree again (narrow-only).
func TestPg_LinkSupportHidesTheLinkInObjectReads(t *testing.T) {
	e := newLPEnv(t, "lptest-linksupport")
	e.seedEvidence("ls-")
	const o1, c1, c2 = "lporder/o1", "lpcust/c1", "lpcust/c2"
	e.mustObserveR(o1, "status", txt("open"), 0, "ls-south", "ls:1")
	e.mustObserveR(c1, "segment", txt("hospital"), 0, "ls-south", "ls:2")
	e.mustObserveR(c2, "segment", txt("clinic"), 0, "ls-south", "ls:3")
	for i, l := range []struct {
		to      string
		support []domain.EvidenceID
	}{{c1, []domain.EvidenceID{"ls-restr"}}, {c2, nil}} {
		if _, err := e.links.PutLink(e.ctx, domain.Link{NamespaceID: e.ns, FromRef: domain.EntityRef(o1), ToRef: domain.EntityRef(l.to),
			Relation: lpVerb, State: domain.LinkStateConfirmed, Mechanism: domain.LinkMechanismRecord, EvidenceID: "ls-south",
			AssertedBy: "t", AssertedAt: base, SourceRef: fmt.Sprintf("lsl:%d", i), SupportingEvidence: l.support}); err != nil {
			t.Fatal(err)
		}
	}
	// Allowed the restricted evidence: south's any-of clause, nothing forbidden.
	granted := &domain.TagPredicate{AnyOfClauses: [][]string{{"r:south", "r:global"}}}
	through := func(p *domain.TagPredicate, segment string) string {
		t.Helper()
		return fmt.Sprint(ids(e.list(t, p, domain.ObjectsQuery{Kind: lpOrder, Links: []domain.ObjectLinkFilter{{Path: []string{lpVerb},
			Where: domain.ObjectCondition{Property: "segment", Op: domain.ObjectOpEq, Values: []domain.StatementValue{txt(segment)}}}}}).Rows))
	}
	linksOf := func(p *domain.TagPredicate) string {
		t.Helper()
		v, err := e.reader.Get(e.ctx, p, e.ns, o1)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range v.Links {
			out = append(out, l.OtherRef)
		}
		sort.Strings(out)
		return fmt.Sprint(out)
	}
	check := func(step string, p *domain.TagPredicate, hospital, links string) {
		t.Helper()
		if got := through(p, "hospital"); got != hospital {
			t.Errorf("%s: link filter through o1→c1 = %s, want %s", step, got, hospital)
		}
		if got := through(p, "clinic"); got != "[lporder/o1]" {
			t.Errorf("%s: link filter through the unsupported link = %s, want [lporder/o1]", step, got)
		}
		if got := linksOf(p); got != links {
			t.Errorf("%s: GetObject links = %s, want %s", step, got, links)
		}
	}
	check("south (barred from the supporting evidence)", southOnly, "[]", "[entity:lpcust/c2]")
	check("granted", granted, "[lporder/o1]", "[entity:lpcust/c1 entity:lpcust/c2]")

	if _, err := e.pool.Exec(e.ctx, `UPDATE links SET supporting_evidence = '{}' WHERE namespace_id = $1`, e.ns); err != nil {
		t.Fatal(err)
	}
	check("south, supporting set emptied", southOnly, "[lporder/o1]", "[entity:lpcust/c1 entity:lpcust/c2]")
}
