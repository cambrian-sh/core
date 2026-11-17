package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The run ledger (ADR-0145 D1). One entry per thing a turn produces: every tool
// result, every step output, every calculation, every promoted artifact. It
// replaces the content store's role as plan-scoped step and tool scratch, so that
// nothing a tool returns is lost between steps, at plan end, or before the reply.
//
// Writes happen at two kernel choke points only (the ToolExecutor after authorize,
// approval and post_tool hooks; the DAGExecutor for every step output), so nothing
// depends on an agent remembering to save. The kernel classifies every entry at
// write time (authz.ClassifyArtifactWrite, the vault's own write classification),
// and every read — by id or by query — goes through authz.EnforcingLedger, which
// applies the reader's scope predicate and reports a forbidden entry as absent
// (AGENTS.md §2.4/§2.5). The raw LedgerStore is assembled only by
// kernel.NewLedgerStack and never handed to a reader.

// LedgerKind is what an entry records.
type LedgerKind string

const (
	// LedgerToolResult is one tool call's result (read or write). A memory recall
	// (the kernel's seed recall and every memory_query, QueryMemory) is recorded as
	// a tool result of the pseudo-tool RecallToolName.
	LedgerToolResult LedgerKind = "tool_result"
	// LedgerStepOutput is one plan step's output.
	LedgerStepOutput LedgerKind = "step_output"
	// LedgerComputed is a deterministic calculation's result with its inputs (D4).
	LedgerComputed LedgerKind = "computed"
	// LedgerArtifact is a file a tool wrote, promoted to the artifact vault.
	LedgerArtifact LedgerKind = "artifact"
	// LedgerScratch is turn-lived working state; deleted when the turn ends.
	LedgerScratch LedgerKind = "scratch"
)

// ValidLedgerKind reports whether k is one of the ledger kinds (the five of D1,
// plus LedgerComposition, the D12 worker composition record, worker_composition.go,
// and LedgerSubtask, the ADR-0150 D2 subtask record, subtasks.go).
func ValidLedgerKind(k LedgerKind) bool {
	switch k {
	case LedgerToolResult, LedgerStepOutput, LedgerComputed, LedgerArtifact, LedgerScratch, LedgerComposition, LedgerSubtask:
		return true
	}
	return false
}

// RecallToolName is the ToolName of a memory recall's ledger entry (ADR-0145 D1:
// what a reply rests on is in the ledger, recalled memory included).
const RecallToolName = "memory_recall"

// LedgerIDPrefix marks a ledger reference wherever one travels as a "cid" (a
// step dependency, a tool result offload), so a reader can route it to the ledger
// instead of the content store.
const LedgerIDPrefix = "led_"

// IsLedgerID reports whether ref is a ledger entry id.
func IsLedgerID(ref string) bool { return strings.HasPrefix(ref, LedgerIDPrefix) }

// LedgerProvenance is where an entry came from. The kernel resolves it from the
// caller's lease (ExecuteTool) or from the run it is executing (the DAG
// executor), never from a payload an agent wrote (INV-5).
type LedgerProvenance struct {
	ConversationID string
	// TurnID is the conversation message that opened the turn (the lease
	// binding's OriginMessageID); "" outside a conversation.
	TurnID string
	RunID  string
	// StepIndex is the plan step; -1 when the entry is not step-scoped (a chat
	// turn's own tool call, an agent-plane call outside a plan).
	StepIndex int
	// Owner is the conversation owner the work is for (ADR-0142 act-for); "" when
	// the work is for no person.
	Owner string
	// SessionID is the task session the lease was minted under (ADR-0157 D5:
	// ListStepArtifacts addresses artifacts by session and step); "" outside a
	// task session. Not part of the entry id (the run already is).
	SessionID string
}

type ledgerProvenanceKey struct{}

// WithLedgerProvenance carries p on ctx for the ledger writers below it.
func WithLedgerProvenance(ctx context.Context, p LedgerProvenance) context.Context {
	return context.WithValue(ctx, ledgerProvenanceKey{}, p)
}

// LedgerProvenanceFromContext returns the provenance on ctx. Absent, it is built
// from the facts ctx already carries (conversation, conversation owner) with no
// run and StepIndex -1, so a write is still attributed as far as the kernel knows.
func LedgerProvenanceFromContext(ctx context.Context) LedgerProvenance {
	if p, ok := ctx.Value(ledgerProvenanceKey{}).(LedgerProvenance); ok {
		if p.Owner == "" {
			p.Owner = ConversationOwnerFromContext(ctx)
		}
		if p.ConversationID == "" {
			p.ConversationID = ConversationIDFromContext(ctx)
		}
		return p
	}
	return LedgerProvenance{
		ConversationID: ConversationIDFromContext(ctx),
		StepIndex:      -1,
		Owner:          ConversationOwnerFromContext(ctx),
	}
}

// LedgerEntry is one stored entry. Body is populated only by a read by id.
type LedgerEntry struct {
	ID             string     `json:"id"`
	Kind           LedgerKind `json:"kind"`
	ConversationID string     `json:"conversation_id,omitempty"`
	TurnID         string     `json:"turn_id,omitempty"`
	RunID          string     `json:"run_id,omitempty"`
	StepIndex      int        `json:"step"`
	ToolName       string     `json:"tool,omitempty"`
	// Args are the tool call's canonical arguments (CanonicalJSON).
	Args json.RawMessage `json:"args,omitempty"`
	// Writer is the principal whose call produced the entry (an agent id).
	Writer string `json:"writer,omitempty"`
	// Owner is the conversation owner the work was for (ADR-0142).
	Owner string `json:"owner,omitempty"`
	// SessionID is the task session of the lease the entry was written under.
	SessionID string `json:"session_id,omitempty"`
	// InstanceID is the agent instance that wrote the entry (ADR-0157 D5.2: the
	// verified per-instance token, or the instance whose out/ the kernel
	// collected); "" when the writer is not a kernel-spawned instance.
	InstanceID string `json:"instance_id,omitempty"`
	// DerivedFrom are the ledger entries this one was built from (the record's
	// DerivedFrom plus, for a derived kind, the step's or turn's tool results the
	// writer inherited) — the provenance an artifact carries (ADR-0157 D5.2).
	DerivedFrom []string `json:"derived_from,omitempty"`
	// SourceRefs are the memory records (chunk, document, experience ids) the
	// content was built from, transitively through DerivedFrom. The ADR-0137
	// erase deletes every entry whose SourceRefs meet what it erases.
	SourceRefs []string `json:"-"`
	// Tags are the kernel-derived classification (ADR-0034/0035); the read gate.
	Tags []string `json:"-"`
	// SourceTags are the classifications of every entry this one was derived from
	// (a step output's own tool results and its upstream inputs, transitively). The
	// read gate is the CONJUNCTION: a reader must be allowed Tags AND every set here
	// (LedgerEntryReadable), so a derived entry is never easier to read than the
	// most restrictive thing it was built from. A union of tags would not do that:
	// a predicate's AnyOf and Required terms admit MORE rows as tags are added.
	SourceTags  [][]string `json:"-"`
	ContentType string     `json:"content_type"`
	// BodyHash is the artifact-vault hash of the stored body.
	BodyHash string `json:"-"`
	// SizeBytes is the size of the ORIGINAL content; the stored body is at most
	// execution.ledger.max_entry_bytes (Truncated).
	SizeBytes int64  `json:"size_bytes"`
	Truncated bool   `json:"truncated,omitempty"`
	Preview   string `json:"preview"`
	// ContentHash is the sha256 of the original content (not of the stored body).
	ContentHash string    `json:"content_hash,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Body        []byte    `json:"-"`
}

// LedgerRecord is what a choke point hands the ledger writer. The writer derives
// the id, the classification, the preview and the expiry; the caller supplies only
// what happened.
type LedgerRecord struct {
	Kind LedgerKind
	// Provenance is resolved by the kernel (LedgerProvenanceFromContext + the
	// executor's step); the writer takes it as given.
	Provenance LedgerProvenance
	ToolName   string
	ArgsJSON   []byte
	// Writer is the principal that produced the content (the calling agent).
	Writer PrincipalRef
	// ClassificationHint narrows the kernel's write classification (a tool's
	// domain tags, so a result inherits the classification of what it returned);
	// it can never broaden it (ADR-0035).
	ClassificationHint []string
	// DerivedFrom names the ledger entries the content was built from (a step's
	// upstream inputs). The writer looks them up on the raw store and carries their
	// classifications forward (LedgerEntry.SourceTags); a step output also inherits
	// its own step's tool results without being told. Unknown ids are ignored.
	DerivedFrom []string
	// SourceClassifications are the classifications of what the content was built
	// from OUTSIDE the ledger: a memory recall's entry carries each recalled
	// chunk's own tags (T-20261109-chat-agent-answers-a-published-rule). They join
	// LedgerEntry.SourceTags, so the entry is never easier to read than the most
	// restrictive chunk in it.
	SourceClassifications [][]string
	// SourceRefs are the memory records (chunk / document / experience ids) the
	// content quotes (a recall's hits). They join LedgerEntry.SourceRefs, with
	// those of every entry it was derived from, so an ADR-0137 erase of the
	// records reaches the entry and everything built from it.
	SourceRefs []string
	// InstanceID is the writing agent instance, from the verified context or the
	// instance the kernel collected out/ from; never from a payload.
	InstanceID  string
	ContentType string
	Body        []byte
}

// LedgerEntryReadable is the ledger's one row test: the reader's predicate must
// admit the entry's own classification and every classification it was derived
// from (SourceTags). A nil predicate admits nothing (fail-closed).
func LedgerEntryReadable(eff *TagPredicate, e LedgerEntry) bool {
	if eff == nil || !eff.Allows(e.Tags) {
		return false
	}
	for _, src := range e.SourceTags {
		if !eff.Allows(src) {
			return false
		}
	}
	return true
}

// LedgerQuery selects entries for ledger_find. Empty fields do not filter; at
// least one of ConversationID, TurnID or RunID must be set (a query never spans
// the whole ledger).
type LedgerQuery struct {
	// SessionID filters by the task session (ListStepArtifacts).
	SessionID      string
	ConversationID string
	TurnID         string
	RunID          string
	// StepIndex filters by step when HasStep is true.
	StepIndex int
	HasStep   bool
	ToolName  string
	Kind      LedgerKind
	// Text is a case-insensitive substring over the entry's searchable text
	// (preview, tool, arguments and the head of the body).
	Text  string
	Limit int
}

// Scoped reports whether q names a conversation, turn or run.
func (q LedgerQuery) Scoped() bool {
	return q.ConversationID != "" || q.TurnID != "" || q.RunID != "" || q.SessionID != ""
}

// LedgerWriter records entries. The kernel's choke points hold this port only.
type LedgerWriter interface {
	Record(ctx context.Context, rec LedgerRecord) (LedgerEntry, error)
}

// ErrArtifactTooLarge refuses an artifact entry over the configured per-file cap
// (execution.ledger.artifacts.max_file_bytes). An artifact is never truncated:
// half a model file or a cut-off CSV is worse than a named refusal.
var ErrArtifactTooLarge = errors.New("ledger: artifact exceeds execution.ledger.artifacts.max_file_bytes")

// LedgerReader reads entries. Every implementation handed to a reader is
// authz.EnforcingLedger: a nil entry means absent OR forbidden, by design.
type LedgerReader interface {
	Get(ctx context.Context, id string) (*LedgerEntry, error)
	Find(ctx context.Context, q LedgerQuery) ([]LedgerEntry, error)
}

// LedgerStore is the raw, UNENFORCED store (Postgres metadata + vault bytes). It
// is constructed only by kernel.NewLedgerStack, wrapped there by the writer and by
// authz.EnforcingLedger, and never handed to a principal-serving reader.
type LedgerStore interface {
	LedgerReader
	// Put stores an entry; an existing id is left as it is (idempotent).
	Put(ctx context.Context, e LedgerEntry) error
	// DeleteScratch removes a turn's scratch entries.
	DeleteScratch(ctx context.Context, turnID string) (int, error)
	// PurgeExpired removes entries whose retention ended before now.
	PurgeExpired(ctx context.Context, now time.Time) (int, error)
}

// LedgerEntryID is the deterministic id of an entry (AGENTS.md §2.6): the hash of
// where it came from (conversation, turn, run, step), which call within the step
// produced it (callKey: the tool and its canonical arguments, or the entry kind for
// a step output) and its content. A retried write of the same thing finds the same
// id; the same call returning different content is a different entry.
func LedgerEntryID(p LedgerProvenance, callKey, contentHash string) string {
	h := sha256.New()
	for _, part := range []string{p.ConversationID, p.TurnID, p.RunID, strconv.Itoa(p.StepIndex), callKey, contentHash} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return LedgerIDPrefix + hex.EncodeToString(h.Sum(nil))[:32]
}

// LedgerCallKey is the call identity part of an entry id: the kind, plus for a
// tool result the tool and its canonical arguments.
func LedgerCallKey(kind LedgerKind, tool string, canonicalArgs []byte) string {
	if tool == "" {
		return string(kind)
	}
	return string(kind) + "\x00" + tool + "\x00" + string(canonicalArgs)
}

// CanonicalJSON re-encodes a JSON value with sorted object keys and no
// insignificant whitespace, so two encodings of the same arguments hash alike.
// Input that is not JSON is returned unchanged.
func CanonicalJSON(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

// LedgerPreview is the first n runes of body when it is text; "" for binary.
func LedgerPreview(body []byte, n int) string {
	if !utf8.Valid(body) {
		return ""
	}
	s := strings.TrimSpace(string(body))
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for c := 0; c < n && i < len(s); c++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i] + "…"
}
