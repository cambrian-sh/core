package postgres

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0148 D1: with include_correlation off, `why` answers from STORED lineage
// only — and that answer must be exactly the stored-lineage answer `why` gave
// before the related lane changed (T-20261112-implement-adr-0148-why-stored-
// lineage-default).
//
// testdata/why_lineage_only.golden was generated (WHYLIN_UPDATE=1) by the code
// BEFORE the ADR-0148 plane change, on a fixture whose events share no
// participant, so the old computed lane contributed nothing and the old answer
// was the pure stored-lineage walk. The flag-off answer is held to it byte for
// byte under four scopes and two limits. Do not regenerate it to make a failure
// pass.
//
// Reuses the walk-equivalence fixture's plumbing (weFixture) with its own rows.

const whyLineageGolden = "testdata/why_lineage_only.golden"

func newWhyLineageFixture(t *testing.T) *weFixture {
	t.Helper()
	f := newWalkEqFixture(t) // migrated pool, cleanup of walkeq-* rows, names map
	pool, ctx := f.pool, f.ctx
	evs := NewPgEvidenceStore(pool)
	evidence := func(name string, tags []string) domain.EvidenceID {
		id, _, err := evs.Insert(ctx, domain.Evidence{
			NamespaceID: "default", SourceID: "walkeq-src", SourceKey: "lin-" + name,
			SourceTime: f.base, IngestedAt: f.base,
			ContentHash: domain.CID("sha256:walkeq-lin-" + name), Classification: tags,
		})
		if err != nil {
			t.Fatalf("evidence %s: %v", name, err)
		}
		f.names[string(id)] = "EV:lin-" + name
		return id
	}
	pub := evidence("pub", []string{"mail"})
	ali := evidence("alice", []string{"mail", "owner:alice"})
	sec := evidence("secret", []string{"mail", "k-secret"})

	events := NewPgEventStore(pool, nil)
	event := func(name string, ev domain.EvidenceID, minutesAgo int) string {
		id := "walkeq-lin-evt-" + name
		// NO roles: no two events share a participant, so the pre-change walk's
		// computed lane was empty and its answer was stored lineage alone.
		if _, _, err := events.RecordEvent(ctx, domain.Event{ID: domain.EventID(id),
			NamespaceID: "default", Type: "lin_" + name,
			OccurredAt: f.base.Add(-time.Duration(minutesAgo) * time.Minute),
			EvidenceID: ev, SourceRef: "walkeq-lin-" + name}); err != nil {
			t.Fatalf("event %s: %v", name, err)
		}
		f.names[id] = "E:lin-" + name
		f.ev["lin-"+name] = domain.RefPrefixEvent + id
		return domain.RefPrefixEvent + id
	}
	a := event("a", pub, 50)
	b := event("b", pub, 40)
	c := event("c", pub, 30)
	d := event("d", ali, 20)
	e := event("e", sec, 10)
	g := event("g", pub, 5)

	reg, _ := domain.NewRelationRegistry([]domain.RelationSpec{{Name: "walkeq_caused_by", Family: domain.LinkFamilyLineage}})
	links := NewPgLinkStore(pool, reg)
	n := 0
	link := func(id, from, to, state string, ev domain.EvidenceID) {
		n++
		if _, err := links.PutLink(ctx, domain.Link{
			ID: id, NamespaceID: "default", FromRef: from, ToRef: to, Relation: "walkeq_caused_by",
			State: state, Mechanism: domain.LinkMechanismReference, EvidenceID: ev, AssertedBy: "test",
			Producer: "walkeq@1", Confidence: 1, SourceRef: "walkeq-" + id,
			AssertedAt: f.base.Add(time.Duration(n) * time.Minute),
		}); err != nil {
			t.Fatalf("link %s: %v", id, err)
		}
	}
	conf, cand := domain.LinkStateConfirmed, domain.LinkStateCandidate
	link("walkeq-lin-1", g, c, conf, pub)
	link("walkeq-lin-2", c, b, conf, pub)
	link("walkeq-lin-3", b, a, conf, pub)
	link("walkeq-lin-4", g, d, conf, ali) // alice's basis
	link("walkeq-lin-5", d, a, conf, pub) // reachable only through alice's hop
	link("walkeq-lin-6", g, e, conf, sec) // secret basis
	link("walkeq-lin-7", c, e, cand, pub) // candidate: Bypass only
	link("walkeq-lin-8", domain.EntityRef("walkeq/lin-subject"), g, conf, pub)
	link("walkeq-lin-9", "decision:walkeq-lin-dec", c, conf, pub)
	return f
}

func (f *weFixture) whyLineageSnapshot() string {
	var b strings.Builder
	queries := []struct {
		label string
		q     domain.KnowledgeQuery
	}{
		{"why lin-g hops=3", domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: f.ev["lin-g"], Hops: 3}},
		{"why lin-g hops=1", domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: f.ev["lin-g"], Hops: 1}},
		{"why lin-g hops=3 limit=3", domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: f.ev["lin-g"], Hops: 3, Limit: 3}},
		{"why lin-c hops=2", domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: f.ev["lin-c"], Hops: 2}},
		{"why lin-a hops=2", domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: f.ev["lin-a"], Hops: 2}},
		{"why entity lin-subject hops=3", domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: "walkeq/lin-subject", Hops: 3}},
		{"why decision hops=2", domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: "decision:walkeq-lin-dec", Hops: 2}},
	}
	for _, s := range weScopes() {
		for _, q := range queries {
			res, err := f.plane.Query(f.ctx, q.q, s.scope)
			// The guarantee label changed on purpose with ADR-0148 D3; the
			// ROWS are what this golden holds still.
			res.Guarantee = ""
			b.WriteString(f.render(s.name+" / "+q.label, res, err))
		}
	}
	return b.String()
}

func TestPg_WhyStoredLineage_FlagOffAnswerUnchanged(t *testing.T) {
	f := newWhyLineageFixture(t)
	got := f.whyLineageSnapshot()
	path := filepath.FromSlash(whyLineageGolden)
	if os.Getenv("WHYLIN_UPDATE") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(want) != got {
		t.Fatalf("flag-off why changed:\n%s", diffLines(string(want), got))
	}
	if !strings.Contains(got, "row=hop") {
		t.Fatal(fmt.Sprintf("golden holds no hop rows, so it proves nothing:\n%s", got))
	}
}
