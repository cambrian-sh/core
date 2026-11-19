// Package ledger is the kernel side of the run ledger (ADR-0145 D1): the writer
// the two choke points record through, the retention sweeper, and the kernel-native
// ledger_get / ledger_find tools. The raw store and the enforced reader are
// assembled together by kernel.NewLedgerStack; nothing here constructs either.
package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// Recorder is the domain.LedgerWriter the ToolExecutor and the DAG executor hold.
// It derives everything an entry needs that the caller must not choose: the
// deterministic id, the kernel classification, the preview, the expiry and the
// size cap. All its dependencies are constructor arguments (AGENTS.md §2.3).
type Recorder struct {
	store            domain.LedgerStore
	authorizer       domain.Authorizer
	retention        time.Duration
	maxEntryBytes    int
	maxArtifactBytes int64
	previewChars     int
	privateByDefault bool
	now              func() time.Time
	// unrecorded counts the entries the decision point refused to classify
	// (ENG-2173: a refusal is counted and logged at WARN, never a silent drop).
	unrecorded atomic.Uint64
}

var _ domain.LedgerWriter = (*Recorder)(nil)

// RecorderConfig carries the execution.ledger values the recorder reads.
type RecorderConfig struct {
	Retention     time.Duration
	MaxEntryBytes int
	// MaxArtifactBytes caps one artifact entry (execution.ledger.artifacts.
	// max_file_bytes, ADR-0157 D5.2). An artifact is never truncated: a larger
	// one is refused with domain.ErrArtifactTooLarge. 0 ⇒ the 100 MiB vault ceiling.
	MaxArtifactBytes int64
	PreviewChars     int
	// PrivateByDefault mirrors the kernel's ADR-0142 D12 switch: an entry written
	// for a conversation owner is stamped owner:<owner>, like every other write
	// made during that person's turn.
	PrivateByDefault bool
}

// NewRecorder builds the writer over the raw store. A nil authorizer is the OSS
// allow-all (the kernel always asks; only the answer differs).
func NewRecorder(store domain.LedgerStore, authorizer domain.Authorizer, cfg RecorderConfig) *Recorder {
	if authorizer == nil {
		authorizer = domain.AllowAllAuthorizer{}
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 720 * time.Hour
	}
	if cfg.MaxEntryBytes <= 0 {
		cfg.MaxEntryBytes = 1 << 20
	}
	if cfg.PreviewChars <= 0 {
		cfg.PreviewChars = 300
	}
	if cfg.MaxArtifactBytes <= 0 {
		cfg.MaxArtifactBytes = 100 << 20
	}
	return &Recorder{
		store: store, authorizer: authorizer, retention: cfg.Retention,
		maxEntryBytes: cfg.MaxEntryBytes, maxArtifactBytes: cfg.MaxArtifactBytes, previewChars: cfg.PreviewChars,
		privateByDefault: cfg.PrivateByDefault, now: time.Now,
	}
}

// ErrUnclassifiable is returned when the decision point refuses to classify an
// entry. The entry is NOT written: a row the kernel cannot classify cannot be
// read-gated, so storing it would be the fail-open choice.
var ErrUnclassifiable = errors.New("ledger: the decision point refused to classify the entry; not recorded")

// Record classifies, caps and stores one entry, returning it without its body.
func (r *Recorder) Record(ctx context.Context, rec domain.LedgerRecord) (domain.LedgerEntry, error) {
	if !domain.ValidLedgerKind(rec.Kind) {
		return domain.LedgerEntry{}, fmt.Errorf("ledger record: invalid kind %q", rec.Kind)
	}
	p := rec.Provenance
	if rec.Kind == domain.LedgerArtifact && int64(len(rec.Body)) > r.maxArtifactBytes {
		return domain.LedgerEntry{}, fmt.Errorf("%w (%d > %d bytes)", domain.ErrArtifactTooLarge, len(rec.Body), r.maxArtifactBytes)
	}
	if r.privateByDefault && p.Owner != "" && domain.PrivateWriteOwnerFromContext(ctx) == "" {
		// The DAG executor's context does not carry the D12 mark the tool path's
		// lease resolution sets; the provenance's owner is the same kernel-held fact.
		ctx = domain.WithPrivateWrites(ctx, p.Owner)
	}
	src := r.sources(ctx, rec)
	tags, err := r.classify(ctx, rec, src)
	if err != nil {
		return domain.LedgerEntry{}, err
	}
	sum := sha256.Sum256(rec.Body)
	contentHash := hex.EncodeToString(sum[:])
	args := domain.CanonicalJSON(rec.ArgsJSON)
	body := rec.Body
	truncated := false
	// An artifact is kept whole (capped above); every other kind keeps its head.
	if rec.Kind != domain.LedgerArtifact && len(body) > r.maxEntryBytes {
		body = body[:r.maxEntryBytes]
		truncated = true
	}
	now := r.now()
	callKey := domain.LedgerCallKey(rec.Kind, rec.ToolName, args)
	if rec.Kind == domain.LedgerArtifact {
		// ADR-0157 D5.2 (T-20261115-artifact-metadata-is-keyed-by-content-hash-so):
		// the same bytes written by two principals (or two instances) are two
		// artifacts with their own classification, never one entry the second
		// writer silently shares — so the writer is part of an artifact's identity.
		callKey += "\x00" + rec.Writer.String() + "\x00" + rec.InstanceID
	}
	e := domain.LedgerEntry{
		ID:             domain.LedgerEntryID(p, callKey, contentHash),
		Kind:           rec.Kind,
		ConversationID: p.ConversationID,
		TurnID:         p.TurnID,
		RunID:          p.RunID,
		StepIndex:      p.StepIndex,
		ToolName:       rec.ToolName,
		Args:           args,
		Writer:         rec.Writer.String(),
		Owner:          p.Owner,
		SessionID:      p.SessionID,
		InstanceID:     rec.InstanceID,
		DerivedFrom:    src.derived,
		SourceRefs:     src.refs,
		Tags:           tags,
		SourceTags:     src.tags,
		ContentType:    rec.ContentType,
		SizeBytes:      int64(len(rec.Body)),
		Truncated:      truncated,
		Preview:        domain.LedgerPreview(rec.Body, r.previewChars),
		ContentHash:    contentHash,
		CreatedAt:      now,
		ExpiresAt:      now.Add(r.retention),
		Body:           body,
	}
	if err := r.store.Put(ctx, e); err != nil {
		return domain.LedgerEntry{}, err
	}
	e.Body = nil
	return e, nil
}

// Unrecorded is how many entries the decision point refused to classify since
// the recorder was built (ENG-2173). Each refusal is also logged at WARN with the
// running total.
func (r *Recorder) Unrecorded() uint64 { return r.unrecorded.Load() }

// classify is the kernel write classification (ADR-0035), the vault's own
// (authz.ClassifyArtifactWrite): the writer's principal decides, a hint may only
// narrow, and the ADR-0142 D12 owner stamp is added after. Every candidate goes
// through that one enforcing path; nothing here judges a tag itself.
//
// The candidates, in order (ENG-2173, owner decision 2026-11-19 "inherit from
// sources"):
//
//  1. a TOOL RESULT built from classified records — a memory recall's chunks
//     (rec.SourceClassifications), a staged call's input entries (DerivedFrom) —
//     is classified with their classification: the union of the source tag sets'
//     requested classification (authz.RequestedClassification: provenance stamps
//     and owner tags are kernel-stamped, never requested). Union, because the
//     entry holds every source at once. The sources still gate reads through
//     SourceTags (domain.LedgerEntryReadable), so the entry is readable by exactly
//     the people who could read every source.
//  2. the tool's domain tags (rec.ClassificationHint) — what the result came from
//     when the ledger knows no sources. A writer's OWN requested tags
//     (UploadArtifact) also arrive here; the RPC refuses an unhonourable request
//     before the recorder sees it.
//  3. no hint: the decision point's default for the writer (its write ceiling).
//     Since ENG-2161 the decision point refuses this when the ceiling holds a
//     closed tag (ClassificationRequiredError); an open-only ceiling keeps it.
//
// A candidate the decision point refuses (or cannot carry in full, ENG-2115) is
// passed over for the next. When none is honoured the entry is NOT written — a row
// the kernel cannot classify cannot be read-gated — and the refusal is loud:
// counted (Unrecorded) and logged at WARN with each refusal's reason
// (ErrUnclassifiable).
func (r *Recorder) classify(ctx context.Context, rec domain.LedgerRecord, src inheritance) ([]string, error) {
	type candidate struct {
		from string
		hint []string
	}
	var cands []candidate
	if rec.Kind == domain.LedgerToolResult && len(src.tags) > 0 {
		var union []string
		for _, set := range src.tags {
			for _, t := range authz.RequestedClassification(domain.ClassificationHint(set)) {
				if !slices.Contains(union, t) {
					union = append(union, t)
				}
			}
		}
		sort.Strings(union)
		if len(union) > 0 {
			cands = append(cands, candidate{"sources", union})
		}
	}
	if len(rec.ClassificationHint) > 0 {
		cands = append(cands, candidate{"tool", rec.ClassificationHint})
	}
	cands = append(cands, candidate{"writer", nil})

	refusals := make([]string, 0, len(cands))
	for _, c := range cands {
		tags, err := authz.ClassifyArtifactWrite(ctx, r.authorizer, rec.Writer, c.hint)
		if err == nil {
			if len(refusals) > 0 {
				slog.Debug("ledger: entry classified after a refused candidate", "writer", rec.Writer.String(),
					"tool", rec.ToolName, "from", c.from, "refused", refusals)
			}
			return tags, nil
		}
		refusals = append(refusals, c.from+": "+err.Error())
	}
	n := r.unrecorded.Add(1)
	slog.Warn("ledger: entry not recorded (unclassifiable)", "event", "ledger_entry_unclassifiable",
		"writer", rec.Writer.String(), "tool", rec.ToolName, "kind", string(rec.Kind),
		"refusals", refusals, "unrecorded_total", n)
	return nil, ErrUnclassifiable
}

// inheritance is what a derived entry carries forward from what it was built
// from: their classifications (tags), their ids (derived) and the memory records
// they quote (refs).
type inheritance struct {
	tags    [][]string
	derived []string
	refs    []string
}

// sources is the classification a derived entry inherits
// (T-20261108-ledger-step-outputs-do-not-inherit-the): for a step output, a
// calculation or an artifact, the tag sets of the tool results recorded under the
// same run and step (an artifact outside a run: under the same turn, by the same
// writer — ADR-0157 D5.2), and of every entry named in rec.DerivedFrom (the
// step's upstream inputs), each with the sets IT was derived from — so
// inheritance is transitive along the plan. The same walk yields the entry's
// DerivedFrom ids and its SourceRefs (the memory records an ADR-0137 erase must
// reach). Read on the RAW store: the writer is the kernel classifying, not a
// reader. Deduplicated; empty for an entry derived from nothing.
//
// A lookup failure is logged and the entry is still written with what was found:
// failing the write would lose the step's output, and the entries that could not
// be read are absent from the run anyway.
func (r *Recorder) sources(ctx context.Context, rec domain.LedgerRecord) inheritance {
	var srcs []domain.LedgerEntry
	p := rec.Provenance
	derivedKind := rec.Kind == domain.LedgerStepOutput || rec.Kind == domain.LedgerComputed || rec.Kind == domain.LedgerArtifact
	switch {
	case derivedKind && p.RunID != "" && p.StepIndex >= 0:
		rows, err := r.store.Find(ctx, domain.LedgerQuery{RunID: p.RunID, StepIndex: p.StepIndex, HasStep: true,
			Kind: domain.LedgerToolResult})
		if err != nil {
			slog.Warn("ledger: the step's tool results could not be read for classification inheritance",
				"run", p.RunID, "step", p.StepIndex, "err", err)
		}
		srcs = append(srcs, rows...)
	case rec.Kind == domain.LedgerArtifact && p.RunID == "" && p.TurnID != "":
		rows, err := r.store.Find(ctx, domain.LedgerQuery{TurnID: p.TurnID, Kind: domain.LedgerToolResult})
		if err != nil {
			slog.Warn("ledger: the turn's tool results could not be read for classification inheritance",
				"turn", p.TurnID, "err", err)
		}
		writer := rec.Writer.String()
		for _, e := range rows {
			if e.Writer == writer {
				srcs = append(srcs, e)
			}
		}
	}
	for _, id := range rec.DerivedFrom {
		if !domain.IsLedgerID(id) {
			continue
		}
		e, err := r.store.Get(ctx, id)
		if err != nil {
			slog.Warn("ledger: an input entry could not be read for classification inheritance", "id", id, "err", err)
			continue
		}
		if e != nil {
			srcs = append(srcs, *e)
		}
	}
	var out inheritance
	seen := map[string]bool{}
	add := func(tags []string) {
		set := append([]string(nil), tags...)
		sort.Strings(set)
		key := strings.Join(set, "|")
		if seen[key] {
			return
		}
		seen[key] = true
		out.tags = append(out.tags, set)
	}
	seenID := map[string]bool{}
	seenRef := map[string]bool{}
	addRef := func(ref string) {
		if ref != "" && !seenRef[ref] {
			seenRef[ref] = true
			out.refs = append(out.refs, ref)
		}
	}
	for _, e := range srcs {
		add(e.Tags)
		for _, st := range e.SourceTags {
			add(st)
		}
		if !seenID[e.ID] {
			seenID[e.ID] = true
			out.derived = append(out.derived, e.ID)
		}
		for _, ref := range e.SourceRefs {
			addRef(ref)
		}
	}
	// Classifications of content from outside the ledger (recalled chunks).
	for _, st := range rec.SourceClassifications {
		add(st)
	}
	for _, ref := range rec.SourceRefs {
		addRef(ref)
	}
	sort.Strings(out.derived)
	sort.Strings(out.refs)
	return out
}
