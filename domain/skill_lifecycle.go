package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

// ADR-0139 D13: the skill lifecycle. Agent-proposed skills wait in a staged
// approval queue and are never active until an operator approves them; every
// mutation of a skill appends to an append-only ledger; a deterministic curator
// (no LLM) moves skills active → stale → archived from usage telemetry. Nothing
// is ever deleted.
//
// These are the ports and entities. The service that enforces the rules is
// internal/skill/lifecycle; the durable adapter is
// internal/infrastructure/postgres (PgSkillLifecycleStore).

// Skill trust/source classes (ADR-0139 C3). The operator plane shows unknown
// values as-is, so this list is not closed for readers.
const (
	SkillSourceBuiltin       = "builtin"
	SkillSourceOfficial      = "official"
	SkillSourceHub           = "hub"
	SkillSourceAgentProposed = "agent-proposed"
)

// Skill lifecycle states.
const (
	SkillStateActive   = "active"
	SkillStateStale    = "stale"
	SkillStateArchived = "archived"
)

// Skill proposal states.
const (
	SkillProposalPending  = "pending"
	SkillProposalApproved = "approved"
	SkillProposalRejected = "rejected"
)

// Ledger mutation kinds (operator.proto SkillLedgerEntryOp.mutation).
const (
	SkillMutationCreated      = "created"
	SkillMutationUpdated      = "updated"
	SkillMutationApproved     = "approved"
	SkillMutationRejected     = "rejected"
	SkillMutationLocked       = "locked"
	SkillMutationUnlocked     = "unlocked"
	SkillMutationStateChanged = "state_changed"
	SkillMutationRolledBack   = "rolled_back"
)

// SkillActorCurator is the ledger actor for a deterministic-curator transition.
const SkillActorCurator = "curator"

// ErrSkillLedgerConflict is returned by SkillLifecycleStore.Commit when the skill's
// ledger moved since the caller read it (optimistic concurrency): re-read and retry.
var ErrSkillLedgerConflict = errors.New("skill lifecycle: ledger moved since read")

// SkillProposal is one agent-proposed skill (Orchestrator/ProposeSkill).
type SkillProposal struct {
	// ID is deterministic over (name, base_version, content hash) — AGENTS.md §2.6:
	// an identical re-proposal is the same row, so a retry cannot queue a duplicate.
	ID   string
	Name string
	// SkillMD is the proposed C3 SKILL.md, verbatim as the agent sent it (the kernel
	// stamps trust "agent-proposed" when it materialises the skill, whatever the
	// frontmatter says).
	SkillMD      string
	ContentHash  string
	Rationale    string
	EvidenceCIDs []string
	// BaseVersion is the version an edit was proposed against; "" for a new skill.
	BaseVersion string
	State       string
	// Provenance, derived server-side from the proposing lease — never claimed by
	// the agent.
	ProposerAgentID string
	PrincipalID     string
	SourceRunID     string
	SessionID       string
	ScanVerdict     string
	ScanFindings    []string
	CreatedAt       time.Time
	DecidedAt       time.Time
	DecidedBy       string
	DecisionNote    string
}

// SkillVersion is one immutable version of a skill's content.
type SkillVersion struct {
	Name      string
	Version   string
	SkillMD   string
	Source    string
	CreatedAt time.Time
	// ProposalID is the proposal this version came from, when it did.
	ProposalID string
}

// SkillRecord is a skill's lifecycle row: the current version pointer, state,
// lock and usage telemetry. One per skill name.
type SkillRecord struct {
	Name           string
	Source         string
	Version        string
	State          string
	StateChangedAt time.Time
	// CurrentSince is when the current version (or the current active spell)
	// began: the curator measures idleness from max(LastUsedAt, CurrentSince).
	CurrentSince time.Time
	Locked       bool
	UseCount     int64
	SuccessCount int64
	LastUsedAt   time.Time
	// LedgerEntryID / LedgerSeq name the ledger entry that produced this state;
	// LedgerSeq is the optimistic-concurrency token for Commit.
	LedgerEntryID string
	LedgerSeq     int64
	CreatedAt     time.Time
}

// SkillLedgerEntry is one append-only mutation of one skill.
type SkillLedgerEntry struct {
	// ID is derived from the natural key (skill name, per-skill sequence).
	ID          string
	SkillName   string
	Seq         int64
	Mutation    string
	FromVersion string
	ToVersion   string
	FromState   string
	ToState     string
	// Locked is the lock flag AFTER this entry (so a rollback can restore it).
	Locked     bool
	Actor      string
	Reason     string
	At         time.Time
	ProposalID string
	RollbackOf string
	// CommandID is the operator command that produced the entry ("" for curator
	// and boot entries); unique when set, which makes operator commands idempotent
	// at the ledger itself, not only in the audit log.
	CommandID string
}

// SkillLedgerEntryID derives a ledger entry's id from its natural key (AGENTS.md
// §2.6): the skill name and the entry's per-skill sequence number. Two writers
// racing for the same sequence produce the same id, so the loser fails on the
// primary key instead of appending a duplicate.
func SkillLedgerEntryID(skillName string, seq int64) string {
	return "sle-" + shortHash(skillName+"\x00"+strconv.FormatInt(seq, 10))
}

// SkillProposalID derives a proposal's id from (name, base_version, content
// hash): an identical re-proposal maps to the same id (operator.proto
// ProposeSkillResponse.proposal_id).
func SkillProposalID(name, baseVersion, contentHash string) string {
	return "sp-" + shortHash(name+"\x00"+baseVersion+"\x00"+contentHash)
}

// SkillContentHash is the hex SHA-256 of a SKILL.md.
func SkillContentHash(skillMD string) string {
	sum := sha256.Sum256([]byte(skillMD))
	return hex.EncodeToString(sum[:])
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:12])
}

// SkillCommit is one atomic lifecycle mutation: optionally a new immutable
// version, the skill's new record, optionally a proposal decision, and exactly
// one ledger entry. ExpectSeq is the skill's ledger head as read (0 = no entry
// yet); a mismatch is ErrSkillLedgerConflict. The store assigns Entry.Seq
// (ExpectSeq+1) and Entry.ID (SkillLedgerEntryID), and stamps them onto Record.
type SkillCommit struct {
	ExpectSeq  int64
	NewVersion *SkillVersion
	Record     *SkillRecord // nil = no record change (e.g. a rejection of a new skill)
	Proposal   *SkillProposal
	Entry      SkillLedgerEntry
}

// SkillProposalFilter selects proposals.
type SkillProposalFilter struct {
	State     string // "" = all
	SkillName string
	Proposer  string
	Limit     int
	Offset    int
}

// SkillLifecycleStore is the durable port behind the lifecycle. The Postgres
// adapter and the in-memory fake implement it identically; all rules
// (scan verdicts, locks, stale bases) live in the service, not here.
type SkillLifecycleStore interface {
	// InsertProposal stores p unless a proposal with p.ID exists; created reports
	// whether this call inserted it.
	InsertProposal(ctx context.Context, p SkillProposal) (created bool, err error)
	GetProposal(ctx context.Context, id string) (SkillProposal, bool, error)
	ListProposals(ctx context.Context, f SkillProposalFilter) ([]SkillProposal, int, error)

	GetSkill(ctx context.Context, name string) (SkillRecord, bool, error)
	ListSkills(ctx context.Context) ([]SkillRecord, error)
	GetVersion(ctx context.Context, name, version string) (SkillVersion, bool, error)
	// CountVersions is the number of stored versions of name (the next ledger
	// version number is CountVersions+1).
	CountVersions(ctx context.Context, name string) (int, error)

	// Commit applies c atomically (see SkillCommit) and returns the appended entry.
	Commit(ctx context.Context, c SkillCommit) (SkillLedgerEntry, error)
	// LedgerHead is the highest ledger sequence for skillName (0 = none).
	LedgerHead(ctx context.Context, skillName string) (int64, error)

	ListLedger(ctx context.Context, skillName string, limit, offset int) ([]SkillLedgerEntry, int, error)
	GetLedgerEntry(ctx context.Context, id string) (SkillLedgerEntry, bool, error)
	LedgerByCommand(ctx context.Context, commandID string) (SkillLedgerEntry, bool, error)

	// RecordUse counts one use_skill load of name (use_count, last_used_at) and,
	// when runID is set, remembers (run, skill) so the run's outcome can credit it.
	RecordUse(ctx context.Context, name, runID string, at time.Time) error
	// RecordRunOutcome settles every (run, skill) use of runID exactly once: on
	// success it increments each skill's success_count. Returns the skills credited.
	RecordRunOutcome(ctx context.Context, runID string, success bool) ([]string, error)
}
