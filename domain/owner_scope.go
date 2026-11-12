package domain

import (
	"context"
	"log/slog"
	"strings"
)

// The owner fence (ADR-0141 position 3; T-20261106-owner-scope-tag-owner-principal-from-personal).
//
// A personal ingress stamps owner:<principal> on every delivery: the mailbox,
// calendar or drive of ONE person. A row carrying owner:X is readable ONLY by a
// reader acting as X. That rule is not a policy term — it is part of what a
// TagPredicate MEANS, applied by TagPredicate.Check and by every SQL mirror of
// it, so it holds on every read lane without any decision point remembering to
// add it, and no policy, grant, or plain Bypass can widen it:
//
//   - A non-bypass predicate always fences. Owners lists the principals the
//     reader acts as (the decision point fills it from the READER — the user
//     themself, or the conversation owner an agent is working for); empty
//     Owners admits no owner row at all. Fail closed: "acting for nobody" and
//     "could not tell who" agree.
//   - A Bypass predicate (ScopeSystem, the OSS allow-all) ALSO fences. Bypass
//     waives the policy tag terms, never the owner fence: the operator plane and
//     the kernel's own enrichment lanes read at ScopeSystem, and neither is a
//     person's own assistant.
//   - Only ScopeMaintenance (Bypass + AllOwners) reads every owner's rows. It
//     is for kernel maintenance that must see a row to keep it correct — fact
//     extraction/backfill of the owner's own data, journal reloads — never for
//     a read whose result reaches a principal. AllOwners on a non-bypass
//     predicate is ignored: the decision point can never produce Bypass for a
//     principal, so no composition can reach it.
//
// The tag is matched by PREFIX, which is why it is not a ForbiddenTags entry:
// the set of owners is open-ended, and enumerating "everyone but me" fails open
// for every owner the enumeration did not know about.

// OwnerTagPrefix is the reserved scope-tag namespace of owner-scoped rows.
// No classification floor, classification_from rule, or writer hint may coin a
// tag in it except as the owner fence allows (see the premium write path).
const OwnerTagPrefix = "owner:"

// OwnerTag is the scope tag of principal id's personal material ("" for "").
func OwnerTag(principalID string) string {
	if principalID == "" {
		return ""
	}
	return OwnerTagPrefix + principalID
}

// IsOwnerTag reports whether tag is in the reserved owner namespace.
func IsOwnerTag(tag string) bool { return strings.HasPrefix(tag, OwnerTagPrefix) }

// DropCallerOwnerTags returns tags without any owner-namespace tag, logging what
// it dropped (ADR-0146 D5: only the kernel puts owner:<id> on a row).
//
// It is applied at every boundary where a CALLER-controlled tag list becomes a
// write's classification hint — the agent plane's IngestMemory, UploadArtifact
// and ProposeKnowledge, the operator plane's ingest, a tool's declared tags on
// the rows its results become, the substrate lane's IngestEvent and a reactive
// signal's declared classification. Under the own-rows rule an owner tag no
// longer only narrows a row: it also waives that owner's positive read terms for
// it, so a forged one would put the forger's content past someone else's floor.
// The write still succeeds; the owner the kernel resolved for the work (the
// conversation owner, from the lease) is stamped after classification by
// StampPrivateOwner, which this does not touch. source names the boundary in the
// log line. Never mutates tags; returns tags itself when nothing is dropped.
func DropCallerOwnerTags(ctx context.Context, source string, tags []string) []string {
	n := 0
	for _, t := range tags {
		if IsOwnerTag(t) {
			n++
		}
	}
	if n == 0 {
		return tags
	}
	out := make([]string, 0, len(tags)-n)
	dropped := make([]string, 0, n)
	for _, t := range tags {
		if IsOwnerTag(t) {
			dropped = append(dropped, t)
			continue
		}
		out = append(out, t)
	}
	slog.WarnContext(ctx, "authz: caller-supplied owner tag dropped (owner tags are kernel-stamped, ADR-0146 D5)",
		slog.String("event", "authz_owner_tag_dropped"),
		slog.String("source", source),
		slog.Any("dropped", dropped))
	return out
}

// OwnersOf returns the principal ids a tag set is owner-scoped to, in order.
// A derived row (a fact, an assessment) carries its sources' owner tags, so this
// is also "whose rows was this derived from".
func OwnersOf(tags []string) []string {
	var out []string
	for _, t := range tags {
		if id, ok := strings.CutPrefix(t, OwnerTagPrefix); ok && id != "" {
			out = append(out, id)
		}
	}
	return out
}

// ScopeMaintenance is the ONLY predicate that reads every owner's rows: the
// kernel-maintenance bypass. Greppable on purpose — a security review enumerates
// every reader of other people's private material by grepping this identifier.
// Never seed it on a path whose result reaches a principal.
//
// Treat as read-only.
var ScopeMaintenance = &TagPredicate{Bypass: true, AllOwners: true}

// ScopeSystemFor is the kernel bypass that may read the owner rows of exactly
// the given principals — for maintenance over one owner's own material (the fact
// extractor reading candidates for a fact derived from X's rows sees X's facts
// and the unowned corpus, never Y's).
func ScopeSystemFor(owners ...string) *TagPredicate {
	return &TagPredicate{Bypass: true, Owners: append([]string(nil), owners...)}
}

// Unfiltered reports whether a read under p may skip row filtering entirely —
// true only for ScopeMaintenance. A plain Bypass is NOT unfiltered: it still
// applies the owner fence. Every "if scope.Bypass { return everything }"
// shortcut must ask this instead, or the shortcut is the leak.
func (p *TagPredicate) Unfiltered() bool { return p != nil && p.Bypass && p.AllOwners }

// HeldOwnerTags returns the owner tags this reader may read: OwnerTag of each
// Owners entry. Never nil when non-empty, and an EMPTY non-nil slice otherwise,
// so a SQL mirror binding it as text[] gets '{}' (admits no owner row) rather
// than NULL (whose three-valued logic would admit them all).
func (p *TagPredicate) HeldOwnerTags() []string {
	out := []string{}
	if p == nil {
		return out
	}
	for _, o := range p.Owners {
		if o != "" {
			out = append(out, OwnerTag(o))
		}
	}
	return out
}

// foreignOwnerTag returns the first owner tag in tags the reader does not hold,
// "" when the row passes the owner fence.
func (p *TagPredicate) foreignOwnerTag(tags []string) string {
	if p.Unfiltered() {
		return ""
	}
	for _, t := range tags {
		if !IsOwnerTag(t) {
			continue
		}
		held := false
		for _, o := range p.Owners {
			if o != "" && t == OwnerTagPrefix+o {
				held = true
				break
			}
		}
		if !held {
			return t
		}
	}
	return ""
}
