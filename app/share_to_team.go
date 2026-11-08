package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// documentRetagger is the slice of the raw vector adapter the publish needs.
type documentRetagger interface {
	DocumentTags(ctx context.Context, documentID string) ([]string, bool, error)
	ParentDocumentOf(ctx context.Context, chunkID string) (string, bool, error)
	RetagDocument(ctx context.Context, documentID string, tags []string) error
}

// memorySharer is the ADR-0142 D12 publish: a person's private document is
// re-classified to team/org knowledge.
//
// It holds the RAW adapter (VecDB) — AGENTS.md §2.2: a kernel-authored retag
// after an explicit, authorised action, the same lane TagMemory uses
// (RetagDocument moves the document row and every chunk in one transaction).
// The authorisation is here, not in the store: the caller must OWN the
// document (it carries owner:<caller>), the target tags must survive the
// decision point's ClassifyWrite for the caller unchanged (a person shares only
// within what they may write), and the approval hub's "publish" policy decides
// whether it applies at once or waits for an approver (operator.ShareToTeam).
type memorySharer struct {
	docs  documentRetagger
	authz domain.Authorizer
}

var _ operator.MemorySharer = (*memorySharer)(nil)

func (m *memorySharer) resolve(ctx context.Context, id string) (string, []string, error) {
	tags, found, err := m.docs.DocumentTags(ctx, id)
	if err != nil {
		return "", nil, fmt.Errorf("share: %w", err)
	}
	if found {
		return id, tags, nil
	}
	parent, ok, err := m.docs.ParentDocumentOf(ctx, id)
	if err != nil {
		return "", nil, fmt.Errorf("share: %w", err)
	}
	if !ok {
		return "", nil, operator.ErrShareNotOwner
	}
	tags, found, err = m.docs.DocumentTags(ctx, parent)
	if err != nil {
		return "", nil, fmt.Errorf("share: %w", err)
	}
	if !found {
		return "", nil, operator.ErrShareNotOwner
	}
	return parent, tags, nil
}

func (m *memorySharer) plan(ctx context.Context, person domain.PrincipalRef, docID string, target []string) (string, []string, error) {
	if person.IsZero() {
		return "", nil, operator.ErrShareNotOwner
	}
	doc, current, err := m.resolve(ctx, docID)
	if err != nil {
		return "", nil, err
	}
	mine := domain.OwnerTag(person.ID)
	owned := false
	for _, t := range current {
		if t == mine {
			owned = true
		}
	}
	if !owned {
		return "", nil, operator.ErrShareNotOwner
	}
	// Classified as the person, with no private-write marker: what may THEY write?
	final, dec := m.authz.ClassifyWrite(domain.WithPrivateWrites(ctx, ""), person, target)
	if !dec.Allowed {
		return "", nil, operator.ErrShareNotPermitted
	}
	have := map[string]bool{}
	for _, t := range final {
		have[t] = true
	}
	for _, t := range target {
		if !have[t] || domain.IsOwnerTag(t) {
			return "", nil, operator.ErrShareNotPermitted
		}
	}
	// Keep every other owner's fence: material derived from two people's rows is
	// not one person's to publish alone.
	out := make([]string, 0, len(current)+len(final))
	seen := map[string]bool{}
	for _, t := range current {
		if t == mine || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, t := range final {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return doc, out, nil
}

// PlanShare implements operator.MemorySharer.
func (m *memorySharer) PlanShare(ctx context.Context, person domain.PrincipalRef, docID string, target []string) ([]string, error) {
	_, tags, err := m.plan(ctx, person, docID, target)
	return tags, err
}

// ApplyShare implements operator.MemorySharer.
func (m *memorySharer) ApplyShare(ctx context.Context, person domain.PrincipalRef, docID string, target []string) ([]string, error) {
	doc, tags, err := m.plan(ctx, person, docID, target)
	if err != nil {
		return nil, err
	}
	if err := m.docs.RetagDocument(ctx, doc, tags); err != nil {
		return nil, fmt.Errorf("share: retag: %w", err)
	}
	slog.Info("ADR-0142 D12: private document shared", "event", "memory_shared",
		"document", doc, "by", person.String(), "tags", tags)
	return tags, nil
}

// publishHandler applies an APPROVED publish item (the hub runs it when the
// approval resolves, also after a restart).
func publishHandler(s operator.MemorySharer) domain.ApprovalHandler {
	return func(ctx context.Context, it domain.ApprovalItem) error {
		_, err := s.ApplyShare(ctx, it.Beneficiary, it.Object, operator.ShareTargetsOf(it.ArgsPreview))
		return err
	}
}
