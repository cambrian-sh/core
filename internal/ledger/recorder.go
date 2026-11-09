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
	"sort"
	"strings"
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
	previewChars     int
	privateByDefault bool
	now              func() time.Time
}

var _ domain.LedgerWriter = (*Recorder)(nil)

// RecorderConfig carries the execution.ledger values the recorder reads.
type RecorderConfig struct {
	Retention     time.Duration
	MaxEntryBytes int
	PreviewChars  int
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
	return &Recorder{
		store: store, authorizer: authorizer, retention: cfg.Retention,
		maxEntryBytes: cfg.MaxEntryBytes, previewChars: cfg.PreviewChars,
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
	if r.privateByDefault && p.Owner != "" && domain.PrivateWriteOwnerFromContext(ctx) == "" {
		// The DAG executor's context does not carry the D12 mark the tool path's
		// lease resolution sets; the provenance's owner is the same kernel-held fact.
		ctx = domain.WithPrivateWrites(ctx, p.Owner)
	}
	tags, err := r.classify(ctx, rec)
	if err != nil {
		return domain.LedgerEntry{}, err
	}
	sources := r.sourceTags(ctx, rec)
	sum := sha256.Sum256(rec.Body)
	contentHash := hex.EncodeToString(sum[:])
	args := domain.CanonicalJSON(rec.ArgsJSON)
	body := rec.Body
	truncated := false
	if len(body) > r.maxEntryBytes {
		body = body[:r.maxEntryBytes]
		truncated = true
	}
	now := r.now()
	e := domain.LedgerEntry{
		ID:             domain.LedgerEntryID(p, domain.LedgerCallKey(rec.Kind, rec.ToolName, args), contentHash),
		Kind:           rec.Kind,
		ConversationID: p.ConversationID,
		TurnID:         p.TurnID,
		RunID:          p.RunID,
		StepIndex:      p.StepIndex,
		ToolName:       rec.ToolName,
		Args:           args,
		Writer:         rec.Writer.String(),
		Owner:          p.Owner,
		Tags:           tags,
		SourceTags:     sources,
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

// classify is the kernel write classification (ADR-0035), the vault's own
// (authz.ClassifyArtifactWrite): the writer's principal decides, the hint (a
// tool's domain tags — what the result came from) may only narrow, and the
// ADR-0142 D12 owner stamp is added after. A hint the decision point rejects is
// dropped and the principal's own classification used; a principal it refuses
// outright gets no entry (ErrUnclassifiable).
func (r *Recorder) classify(ctx context.Context, rec domain.LedgerRecord) ([]string, error) {
	tags, err := authz.ClassifyArtifactWrite(ctx, r.authorizer, rec.Writer, rec.ClassificationHint)
	if err == nil {
		return tags, nil
	}
	if len(rec.ClassificationHint) > 0 && errors.Is(err, authz.ErrWriteDenied) {
		slog.Warn("ledger: classification hint refused; using the writer's own classification",
			"writer", rec.Writer.String(), "tool", rec.ToolName, "hint", rec.ClassificationHint)
		if tags, err = authz.ClassifyArtifactWrite(ctx, r.authorizer, rec.Writer, nil); err == nil {
			return tags, nil
		}
	}
	slog.Warn("ledger: entry not recorded (unclassifiable)", "writer", rec.Writer.String(),
		"tool", rec.ToolName, "kind", string(rec.Kind), "err", err)
	return nil, ErrUnclassifiable
}

// sourceTags is the classification a derived entry inherits
// (T-20261108-ledger-step-outputs-do-not-inherit-the): for a step output or a
// calculation, the tag sets of the tool results recorded under the same run and
// step, and of every entry named in rec.DerivedFrom (the step's upstream inputs),
// each with the sets IT was derived from — so inheritance is transitive along the
// plan. Read on the RAW store: the writer is the kernel classifying, not a reader.
// Deduplicated; nil for an entry derived from nothing (a tool result).
//
// A lookup failure is logged and the entry is still written with what was found:
// failing the write would lose the step's output, and the entries that could not
// be read are absent from the run anyway.
func (r *Recorder) sourceTags(ctx context.Context, rec domain.LedgerRecord) [][]string {
	var srcs []domain.LedgerEntry
	p := rec.Provenance
	if (rec.Kind == domain.LedgerStepOutput || rec.Kind == domain.LedgerComputed) && p.RunID != "" && p.StepIndex >= 0 {
		rows, err := r.store.Find(ctx, domain.LedgerQuery{RunID: p.RunID, StepIndex: p.StepIndex, HasStep: true,
			Kind: domain.LedgerToolResult})
		if err != nil {
			slog.Warn("ledger: the step's tool results could not be read for classification inheritance",
				"run", p.RunID, "step", p.StepIndex, "err", err)
		}
		srcs = append(srcs, rows...)
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
	if len(srcs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out [][]string
	add := func(tags []string) {
		set := append([]string(nil), tags...)
		sort.Strings(set)
		key := strings.Join(set, "|")
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, set)
	}
	for _, e := range srcs {
		add(e.Tags)
		for _, st := range e.SourceTags {
			add(st)
		}
	}
	return out
}
