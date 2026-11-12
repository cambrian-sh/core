package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// ADR-0148 (accepted 2026-11-12): the related-rows fields of `why` are part of
// the closed AST, refused rather than ignored wherever they mean nothing.
func TestValidate_CorrelationFields(t *testing.T) {
	why := KnowledgeQuery{Kind: QueryWhy, EntityID: "event:e-1"}
	cases := []struct {
		name string
		q    KnowledgeQuery
		ok   bool
	}{
		{"why, flag off", why, true},
		{"why, flag on, defaults", with(why, func(q *KnowledgeQuery) { q.IncludeCorrelation = true }), true},
		{"why, flag on, 30 days", with(why, func(q *KnowledgeQuery) {
			q.IncludeCorrelation, q.CorrelationWindow = true, CorrelationMaxWindow
		}), true},
		{"why, window above the cap", with(why, func(q *KnowledgeQuery) {
			q.IncludeCorrelation, q.CorrelationWindow = true, CorrelationMaxWindow+time.Second
		}), false},
		{"why, negative window", with(why, func(q *KnowledgeQuery) {
			q.IncludeCorrelation, q.CorrelationWindow = true, -time.Hour
		}), false},
		{"why, window without the flag", with(why, func(q *KnowledgeQuery) { q.CorrelationWindow = time.Hour }), false},
		{"why, roles without the flag", with(why, func(q *KnowledgeQuery) { q.CorrelationRoles = []string{"customer"} }), false},
		{"traverse with the flag", KnowledgeQuery{Kind: QueryTraverse, EntityID: "x", Hops: 1,
			From: time.Unix(0, 0), To: time.Unix(10, 0), IncludeCorrelation: true}, false},
		{"entity with roles", KnowledgeQuery{Kind: QueryEntity, EntityID: "x", CorrelationRoles: []string{"r"}}, false},
	}
	for _, c := range cases {
		err := c.q.Validate()
		if c.ok && err != nil {
			t.Errorf("%s: unexpected refusal %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrCannotExpress) {
			t.Errorf("%s: want ErrCannotExpress, got %v", c.name, err)
		}
	}
}

func with(q KnowledgeQuery, f func(*KnowledgeQuery)) KnowledgeQuery {
	f(&q)
	return q
}

func TestGuarantee_WhySaysWhatTheAnswerHolds(t *testing.T) {
	off := KnowledgeQuery{Kind: QueryWhy}.Guarantee()
	on := KnowledgeQuery{Kind: QueryWhy, IncludeCorrelation: true}.Guarantee()
	if !strings.Contains(off, "stored lineage only") || !strings.Contains(off, "include_correlation") {
		t.Errorf("flag-off label %q", off)
	}
	if !strings.Contains(on, "not causal") || !strings.Contains(on, "related rows") {
		t.Errorf("flag-on label %q", on)
	}
}

func TestCorrelationWindowOrDefault(t *testing.T) {
	if got := (KnowledgeQuery{}).CorrelationWindowOrDefault(); got != 72*time.Hour {
		t.Fatalf("default window %s, want 72h (ADR-0128 §6 precedent)", got)
	}
	if got := (KnowledgeQuery{CorrelationWindow: time.Hour}).CorrelationWindowOrDefault(); got != time.Hour {
		t.Fatalf("explicit window %s", got)
	}
}
