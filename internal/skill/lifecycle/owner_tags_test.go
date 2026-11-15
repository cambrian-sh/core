package lifecycle

import (
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1046 (owner decision 2026-11-15): an agent may not propose a skill whose
// scope carries an owner tag, and an approved skill already STORED with one
// (written before this rule) is refused at load — loudly, naming the tag —
// rather than served fenced to that owner.
func TestPropose_RefusesOwnerScope(t *testing.T) {
	r := newRig(t, nil)
	md := "---\nname: mine\ndescription: d\nscope: [\"owner:alice\"]\n---\nbody"
	res, err := r.svc.Propose(ctxb(), ProposeInput{Name: "mine", SkillMD: md, AgentID: "agent-a"})
	mustNoErr(t, err)
	if res.Rejection != RejectInvalidFormat || !strings.Contains(res.Detail, "owner:alice") {
		t.Fatalf("want invalid_format naming the tag, got %+v", res)
	}
}

func TestMaterialize_RefusesStoredOwnerScopedSkill(t *testing.T) {
	r := newRig(t, nil)
	p := r.propose(t, "tidy-notes", "1. Sort the notes.", "")
	r.approve(t, "cmd-1", p.ProposalID)
	// Simulate a row stored before the rule: rewrite the stored content.
	k := "tidy-notes\x00" + "1"
	v, ok := r.store.versions[k]
	if !ok {
		t.Fatalf("no stored version %q", k)
	}
	v.SkillMD = "---\nname: tidy-notes\ndescription: d\nscope: [\"owner:alice\"]\n---\n1. Sort the notes."
	r.store.versions[k] = v

	reg2 := domain.NewInMemorySkillRegistry()
	svc2, _ := New(r.store, reg2, nil, r.svc.cfg, nil, r.clk.now, nil)
	err := svc2.Materialize(ctxb())
	if !errors.Is(err, domain.ErrOwnerTagInDescriptor) || !strings.Contains(err.Error(), "owner:alice") {
		t.Fatalf("want a load error naming the tag, got %v", err)
	}
	if _, ok := reg2.Get("tidy-notes"); ok {
		t.Fatal("a stored owner-scoped skill must not be served")
	}
}
