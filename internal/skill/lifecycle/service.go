// Package lifecycle is the ADR-0139 D13 skill lifecycle: the staged approval
// queue for agent-proposed skills, operator locks, the append-only mutation
// ledger with rollback, usage telemetry, and the deterministic curator.
//
// It EXTENDS the existing skills plane rather than standing beside it (AGENTS.md
// §4.3): an approved agent skill is materialised into the same
// domain.InMemorySkillRegistry and DocTypeSkill index that file discovery fills
// (internal/skill/discovery), so ListSkills/use_skill serve it through the one
// path they already have. Approved agent skills are stored in Postgres
// (domain.SkillLifecycleStore), never written into the shipped skills/ directory:
// that directory is the repository's own content, and a kernel writing into it
// would turn a deployment's operator decisions into source-tree churn.
//
// Rules this package enforces (operator.proto / cambrian.proto ADR-0139 D13):
//   - a proposed skill is never active until an operator approves it;
//   - trust is stamped "agent-proposed" whatever the frontmatter says;
//   - an identical re-proposal returns the same deterministic proposal id;
//   - a locked skill refuses agent proposals (LOCKED); operators may still roll
//     it back;
//   - a "dangerous" scan verdict is never queued and can never be approved;
//   - approving against a base_version that is no longer current is a failed
//     precondition;
//   - the ledger is append-only; a rollback is a new entry;
//   - only the deterministic curator (curator.go, no LLM) moves
//     active → stale → archived; pinned and pipeline/schedule-referenced skills
//     are exempt.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/contentscan"
	"github.com/cambrian-sh/core/internal/skill/discovery"
)

// Sentinel errors the transports map onto gRPC codes.
var (
	ErrNotFound           = errors.New("skill lifecycle: not found")
	ErrFailedPrecondition = errors.New("skill lifecycle: failed precondition")
	ErrInvalidArgument    = errors.New("skill lifecycle: invalid argument")
)

// Rejection is why ProposeSkill refused a proposal (cambrian.proto
// SkillProposalRejection). RejectNone = accepted.
type Rejection int

const (
	RejectNone Rejection = iota
	RejectInvalidFormat
	RejectNameTaken
	RejectLocked
	RejectScanDangerous
	RejectQuota
	RejectDenied
)

// Registry is the slice of the kernel skill catalog the lifecycle writes:
// domain.InMemorySkillRegistry satisfies it.
type Registry interface {
	Register(s domain.Skill)
	Get(name string) (domain.Skill, bool)
	Unregister(name string)
}

// Indexer keeps the DocTypeSkill retrieval index in step with the catalog:
// *domain.SkillIndexer satisfies it. Optional (nil = no index maintenance).
type Indexer interface {
	Index(ctx context.Context, s domain.Skill) error
	Remove(ctx context.Context, name string) error
}

// ReferenceChecker reports why a skill is referenced by a pipeline or a schedule
// ("pipeline" | "schedule"), or "" when it is not. The curator never moves a
// referenced skill.
type ReferenceChecker func(name string) string

// ProposalGranter grants an approved skill to the agent that proposed it
// (decisions/skill-access-grants.md, refinement b: a worker's menu is its
// profile's skills plus its grants, so without this an approved proposal reached
// no agent — E2E 20261105T070704Z E40). *access.Service satisfies it.
type ProposalGranter interface {
	GrantProposer(ctx context.Context, agentID, skill, approver, proposalID string) error
}

// Config is the lifecycle's tuning (config execution.skills; see
// internal/config/skills_lifecycle.go).
type Config struct {
	// ProposalsEnabled lets agents call ProposeSkill. Off = every proposal is
	// DENIED. Proposals are inert until approved, so the default is on.
	ProposalsEnabled bool
	// MaxPendingPerAgent caps one agent's PENDING proposals (QUOTA beyond it).
	MaxPendingPerAgent int
	// MaxSkillMDChars caps a proposed SKILL.md (INVALID_FORMAT beyond it).
	MaxSkillMDChars int
	Curator         CuratorConfig
}

// CuratorConfig drives the deterministic lifecycle (curator.go).
type CuratorConfig struct {
	Enabled bool
	// StaleAfter: an active skill idle this long becomes stale.
	StaleAfter time.Duration
	// ArchiveAfter: a stale skill idle this long (measured like StaleAfter, from
	// its last activity) becomes archived. Must exceed StaleAfter.
	ArchiveAfter time.Duration
	// Interval between curator passes.
	Interval time.Duration
	// Sources the curator manages (default: agent-proposed only — the shipped
	// builtin skills are repository content an operator chose to deploy).
	Sources []string
	// Pinned skills are exempt ("pinned").
	Pinned []string
}

// Service is the lifecycle. Construct with New; every dependency is a
// constructor argument (AGENTS.md §2.3).
type Service struct {
	store domain.SkillLifecycleStore
	reg   Registry
	idx   Indexer
	cfg   Config
	refs  ReferenceChecker
	now   func() time.Time
	// grants receives every approved proposal's proposer grant; nil = no grant
	// store (the composition root logs that approvals then reach no agent).
	grants ProposalGranter

	// mu serialises this process's mutations; Commit's ledger-head check guards
	// against any other writer.
	mu sync.Mutex
	// builtins are the names file discovery serves; the lifecycle never
	// materialises a DB skill over one of them.
	builtins map[string]bool
}

// New constructs the lifecycle. store and reg are required. refs may be nil (no
// pipeline/schedule references exist). now nil = time.Now. grants nil = an
// approval grants the skill to nobody (no skill grant store).
func New(store domain.SkillLifecycleStore, reg Registry, idx Indexer, cfg Config, refs ReferenceChecker, now func() time.Time, grants ProposalGranter) (*Service, error) {
	if store == nil || reg == nil {
		return nil, fmt.Errorf("skill lifecycle: store and registry are required")
	}
	if now == nil {
		now = time.Now
	}
	if cfg.MaxSkillMDChars <= 0 {
		cfg.MaxSkillMDChars = 32000
	}
	if cfg.MaxPendingPerAgent <= 0 {
		cfg.MaxPendingPerAgent = 20
	}
	if len(cfg.Curator.Sources) == 0 {
		cfg.Curator.Sources = []string{domain.SkillSourceAgentProposed}
	}
	return &Service{store: store, reg: reg, idx: idx, cfg: cfg, refs: refs, now: now, grants: grants, builtins: map[string]bool{}}, nil
}

var skillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ── boot ────────────────────────────────────────────────────────────────────

// SyncBuiltins gives every file-discovered skill a lifecycle record (source
// "builtin"), so it has a version, usage telemetry and a ledger. A changed file
// appends an "updated" entry. Builtin content always comes from disk; the record
// never overrides it.
func (s *Service) SyncBuiltins(ctx context.Context, skills []domain.Skill) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for _, sk := range skills {
		s.builtins[sk.Name] = true
		md := ""
		if sk.SourcePath != "" {
			if b, err := os.ReadFile(sk.SourcePath); err == nil {
				md = string(b)
			}
		}
		if md == "" {
			md = sk.Instructions
		}
		version := sk.Version
		if version == "" {
			version = "sha-" + domain.SkillContentHash(md)[:12]
		}
		rec, ok, err := s.store.GetSkill(ctx, sk.Name)
		if err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		if ok && rec.Source != domain.SkillSourceBuiltin {
			slog.Warn("ADR-0139 D13: a shipped skill shares its name with a lifecycle skill; the shipped file is served",
				"skill", sk.Name, "lifecycle_source", rec.Source)
			continue
		}
		if ok && rec.Version == version {
			continue
		}
		head, err := s.store.LedgerHead(ctx, sk.Name)
		if err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		now := s.now().UTC()
		next := domain.SkillRecord{
			Name: sk.Name, Source: domain.SkillSourceBuiltin, Version: version,
			State: domain.SkillStateActive, StateChangedAt: now, CurrentSince: now, CreatedAt: now,
		}
		entry := domain.SkillLedgerEntry{
			SkillName: sk.Name, Mutation: domain.SkillMutationCreated, ToVersion: version,
			ToState: domain.SkillStateActive, Actor: "boot", Reason: "discovered " + sk.SourcePath, At: now,
		}
		if ok {
			next = rec
			next.Version, next.CurrentSince = version, now
			entry.Mutation, entry.FromVersion, entry.FromState, entry.ToState = domain.SkillMutationUpdated, rec.Version, rec.State, rec.State
			entry.Reason = "file changed on disk: " + sk.SourcePath
		}
		if _, err := s.store.Commit(ctx, domain.SkillCommit{
			ExpectSeq:  head,
			NewVersion: &domain.SkillVersion{Name: sk.Name, Version: version, SkillMD: md, Source: domain.SkillSourceBuiltin, CreatedAt: now},
			Record:     &next,
			Entry:      entry,
		}); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
	}
	return firstErr
}

// Materialize loads every approved, non-archived agent skill from the store into
// the registry (and the index), and unregisters archived ones. Called at boot
// after file discovery and SyncBuiltins.
func (s *Service) Materialize(ctx context.Context) error {
	recs, err := s.store.ListSkills(ctx)
	if err != nil {
		return fmt.Errorf("skill lifecycle: list: %w", err)
	}
	var firstErr error
	for _, rec := range recs {
		if err := s.apply(ctx, rec); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
	}
	return firstErr
}

// apply makes the catalog match one record: an archived skill is not served; a
// served agent skill carries its stored content. Builtins are served from disk
// and only ever unregistered (when archived).
func (s *Service) apply(ctx context.Context, rec domain.SkillRecord) error {
	if rec.State == domain.SkillStateArchived {
		s.reg.Unregister(rec.Name)
		if s.idx != nil {
			if err := s.idx.Remove(ctx, rec.Name); err != nil {
				slog.Warn("ADR-0139 D13: archived skill index removal failed", "skill", rec.Name, "err", err)
			}
		}
		return nil
	}
	if rec.Source == domain.SkillSourceBuiltin || s.builtins[rec.Name] {
		return nil
	}
	v, ok, err := s.store.GetVersion(ctx, rec.Name, rec.Version)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("skill lifecycle: %s version %s has no stored content", rec.Name, rec.Version)
	}
	sk, err := discovery.ParseSkillMD([]byte(v.SkillMD))
	if err != nil {
		return fmt.Errorf("skill lifecycle: %s v%s: %w", rec.Name, rec.Version, err)
	}
	// ADR-0139 D13: the kernel decides trust and version, never the frontmatter.
	sk.Name, sk.Trust, sk.Version, sk.SourcePath = rec.Name, rec.Source, rec.Version, ""
	s.reg.Register(sk)
	if s.idx != nil {
		if err := s.idx.Index(ctx, sk); err != nil {
			slog.Warn("ADR-0139 D13: skill index update failed (skill still served by name)", "skill", rec.Name, "err", err)
		}
	}
	return nil
}

// ── agent plane ─────────────────────────────────────────────────────────────

// ProposeInput is one ProposeSkill call. AgentID, RunID, SessionID and
// PrincipalID come from the lease the kernel resolved, never from the agent.
type ProposeInput struct {
	Name         string
	SkillMD      string
	Rationale    string
	EvidenceCIDs []string
	BaseVersion  string
	AgentID      string
	PrincipalID  string
	RunID        string
	SessionID    string
}

// ProposeResult is the ProposeSkill answer.
type ProposeResult struct {
	Accepted   bool
	ProposalID string
	Rejection  Rejection
	Detail     string
}

func refuse(r Rejection, format string, args ...any) ProposeResult {
	return ProposeResult{Rejection: r, Detail: fmt.Sprintf(format, args...)}
}

// Propose queues an agent-proposed skill as PENDING. Refusals are results, not
// errors; an error means the store failed.
func (s *Service) Propose(ctx context.Context, in ProposeInput) (ProposeResult, error) {
	if !s.cfg.ProposalsEnabled {
		return refuse(RejectDenied, "skill proposals are disabled on this kernel (execution.skills.proposals_enabled)"), nil
	}
	if in.AgentID == "" {
		return refuse(RejectDenied, "the proposing lease is not bound to an agent"), nil
	}
	if !skillNameRE.MatchString(in.Name) {
		return refuse(RejectInvalidFormat, "name %q must match %s", in.Name, skillNameRE), nil
	}
	if n := len([]rune(in.SkillMD)); n > s.cfg.MaxSkillMDChars {
		return refuse(RejectInvalidFormat, "skill_md is %d chars; the cap is %d", n, s.cfg.MaxSkillMDChars), nil
	}
	parsed, err := discovery.ParseSkillMD([]byte(in.SkillMD))
	if err != nil {
		return refuse(RejectInvalidFormat, "%v", err), nil
	}
	if parsed.Name != in.Name {
		return refuse(RejectInvalidFormat, "frontmatter name %q does not match name %q", parsed.Name, in.Name), nil
	}
	if strings.TrimSpace(parsed.Instructions) == "" {
		return refuse(RejectInvalidFormat, "the skill body is empty"), nil
	}

	hash := domain.SkillContentHash(in.SkillMD)
	id := domain.SkillProposalID(in.Name, in.BaseVersion, hash)
	if p, ok, err := s.store.GetProposal(ctx, id); err != nil {
		return ProposeResult{}, err
	} else if ok {
		return ProposeResult{Accepted: true, ProposalID: id, Detail: "identical proposal already recorded (state " + p.State + ")"}, nil
	}

	rec, exists, err := s.store.GetSkill(ctx, in.Name)
	if err != nil {
		return ProposeResult{}, err
	}
	switch {
	case exists && (rec.Source == domain.SkillSourceBuiltin || s.builtins[in.Name]):
		return refuse(RejectLocked, "%s is a shipped skill; it is edited on disk, not by proposal", in.Name), nil
	case exists && rec.Locked:
		return refuse(RejectLocked, "%s is locked against agent edits", in.Name), nil
	case exists && in.BaseVersion != rec.Version:
		return refuse(RejectNameTaken, "%s exists at version %s; propose an edit with base_version %s", in.Name, rec.Version, rec.Version), nil
	case !exists && in.BaseVersion != "":
		if _, served := s.reg.Get(in.Name); served {
			return refuse(RejectLocked, "%s is not a lifecycle-managed skill", in.Name), nil
		}
		return refuse(RejectInvalidFormat, "base_version %q names a skill that does not exist", in.BaseVersion), nil
	case !exists:
		if _, served := s.reg.Get(in.Name); served {
			return refuse(RejectNameTaken, "a skill named %s is already served", in.Name), nil
		}
	}

	scan := contentscan.Scan(in.SkillMD)
	if scan.Verdict == contentscan.VerdictDangerous {
		slog.WarnContext(ctx, "ADR-0139 D13: dangerous skill proposal refused",
			"event", "skill_proposal_dangerous", "skill", in.Name, "agent_id", in.AgentID,
			"run_id", in.RunID, "findings", scan.FindingStrings())
		return refuse(RejectScanDangerous, "content scan: dangerous (%s)", strings.Join(scan.FindingStrings(), "; ")), nil
	}

	_, pending, err := s.store.ListProposals(ctx, domain.SkillProposalFilter{State: domain.SkillProposalPending, Proposer: in.AgentID, Limit: 1})
	if err != nil {
		return ProposeResult{}, err
	}
	if pending >= s.cfg.MaxPendingPerAgent {
		return refuse(RejectQuota, "%d proposals already pending for %s (cap %d)", pending, in.AgentID, s.cfg.MaxPendingPerAgent), nil
	}

	p := domain.SkillProposal{
		ID: id, Name: in.Name, SkillMD: in.SkillMD, ContentHash: hash, Rationale: in.Rationale,
		EvidenceCIDs: in.EvidenceCIDs, BaseVersion: in.BaseVersion, State: domain.SkillProposalPending,
		ProposerAgentID: in.AgentID, PrincipalID: in.PrincipalID, SourceRunID: in.RunID, SessionID: in.SessionID,
		ScanVerdict: string(scan.Verdict), ScanFindings: scan.FindingStrings(), CreatedAt: s.now().UTC(),
	}
	if _, err := s.store.InsertProposal(ctx, p); err != nil {
		return ProposeResult{}, err
	}
	slog.InfoContext(ctx, "ADR-0139 D13: skill proposal queued", "event", "skill_proposed",
		"skill", in.Name, "proposal_id", id, "agent_id", in.AgentID, "run_id", in.RunID, "scan", scan.Verdict)
	return ProposeResult{Accepted: true, ProposalID: id, Detail: "queued for operator approval; not active until approved"}, nil
}

// RecordUse counts a use_skill load (ADR-0139 D13 telemetry). Unknown names are
// ignored by the store.
func (s *Service) RecordUse(ctx context.Context, name, runID string) error {
	return s.store.RecordUse(ctx, name, runID, s.now().UTC())
}

// RecordRunOutcome credits success_count for every skill a run loaded, once.
func (s *Service) RecordRunOutcome(ctx context.Context, runID string, success bool) error {
	if runID == "" {
		return nil
	}
	_, err := s.store.RecordRunOutcome(ctx, runID, success)
	return err
}

// ── operator plane ──────────────────────────────────────────────────────────

// DecideInput is one DecideSkillProposal command.
type DecideInput struct {
	CommandID  string
	ProposalID string
	Approve    bool
	LockAfter  bool
	Actor      string
	Reason     string
}

// MutationResult is what a ledger-writing command returns.
type MutationResult struct {
	Version       string
	LedgerEntryID string
	Deduped       bool
}

func precondition(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrFailedPrecondition, fmt.Sprintf(format, args...))
}

func (s *Service) deduped(ctx context.Context, commandID string) (MutationResult, bool, error) {
	if commandID == "" {
		return MutationResult{}, false, fmt.Errorf("%w: command_id is required", ErrInvalidArgument)
	}
	e, ok, err := s.store.LedgerByCommand(ctx, commandID)
	if err != nil || !ok {
		return MutationResult{}, false, err
	}
	return MutationResult{Version: e.ToVersion, LedgerEntryID: e.ID, Deduped: true}, true, nil
}

// Decide approves or rejects one PENDING proposal.
func (s *Service) Decide(ctx context.Context, in DecideInput) (MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok, err := s.deduped(ctx, in.CommandID); err != nil || ok {
		if err == nil && in.Approve {
			// A retried approval re-applies the proposer grant (idempotent), so a
			// grant that failed on the first attempt is not lost to the dedupe.
			if p, found, perr := s.store.GetProposal(ctx, in.ProposalID); perr == nil && found && p.State == domain.SkillProposalApproved {
				if gerr := s.grantProposer(ctx, p, in.Actor); gerr != nil {
					return r, gerr
				}
			}
		}
		return r, err
	}
	p, ok, err := s.store.GetProposal(ctx, in.ProposalID)
	if err != nil {
		return MutationResult{}, err
	}
	if !ok {
		return MutationResult{}, fmt.Errorf("%w: proposal %s", ErrNotFound, in.ProposalID)
	}
	if p.State != domain.SkillProposalPending {
		return MutationResult{}, precondition("proposal %s is %s, not pending", p.ID, p.State)
	}
	rec, exists, err := s.store.GetSkill(ctx, p.Name)
	if err != nil {
		return MutationResult{}, err
	}
	head, err := s.store.LedgerHead(ctx, p.Name)
	if err != nil {
		return MutationResult{}, err
	}
	now := s.now().UTC()
	decided := p
	decided.DecidedAt, decided.DecidedBy, decided.DecisionNote = now, in.Actor, in.Reason

	if !in.Approve {
		decided.State = domain.SkillProposalRejected
		e, err := s.store.Commit(ctx, domain.SkillCommit{
			ExpectSeq: head, Proposal: &decided,
			Entry: domain.SkillLedgerEntry{
				SkillName: p.Name, Mutation: domain.SkillMutationRejected,
				FromVersion: rec.Version, ToVersion: rec.Version, FromState: rec.State, ToState: rec.State, Locked: rec.Locked,
				Actor: in.Actor, Reason: in.Reason, At: now, ProposalID: p.ID, CommandID: in.CommandID,
			},
		})
		if err != nil {
			return MutationResult{}, conflict(err)
		}
		return MutationResult{Version: rec.Version, LedgerEntryID: e.ID}, nil
	}

	// A dangerous verdict can never be approved. Re-scan rather than trust the
	// stored verdict, so a rule added since the proposal also binds.
	if p.ScanVerdict == string(contentscan.VerdictDangerous) || contentscan.Scan(p.SkillMD).Verdict == contentscan.VerdictDangerous {
		return MutationResult{}, precondition("proposal %s has a dangerous scan verdict and cannot be approved", p.ID)
	}
	if exists && (rec.Source == domain.SkillSourceBuiltin || s.builtins[p.Name]) {
		return MutationResult{}, precondition("%s is a shipped skill; it is edited on disk", p.Name)
	}
	current := ""
	if exists {
		current = rec.Version
	}
	if p.BaseVersion != current {
		return MutationResult{}, precondition("proposal %s was made against version %q; %s is now at %q", p.ID, p.BaseVersion, p.Name, current)
	}
	if !exists {
		if _, served := s.reg.Get(p.Name); served {
			return MutationResult{}, precondition("a skill named %s is already served", p.Name)
		}
	}
	n, err := s.store.CountVersions(ctx, p.Name)
	if err != nil {
		return MutationResult{}, err
	}
	version := strconv.Itoa(n + 1)
	next := rec
	if !exists {
		next = domain.SkillRecord{Name: p.Name, CreatedAt: now}
	}
	if next.State != domain.SkillStateActive {
		next.StateChangedAt = now
	}
	next.Source, next.Version, next.State, next.CurrentSince = domain.SkillSourceAgentProposed, version, domain.SkillStateActive, now
	mutation := domain.SkillMutationApproved
	decided.State = domain.SkillProposalApproved
	e, err := s.store.Commit(ctx, domain.SkillCommit{
		ExpectSeq:  head,
		NewVersion: &domain.SkillVersion{Name: p.Name, Version: version, SkillMD: p.SkillMD, Source: domain.SkillSourceAgentProposed, CreatedAt: now, ProposalID: p.ID},
		Record:     &next,
		Proposal:   &decided,
		Entry: domain.SkillLedgerEntry{
			SkillName: p.Name, Mutation: mutation, FromVersion: rec.Version, ToVersion: version,
			FromState: rec.State, ToState: domain.SkillStateActive, Locked: next.Locked,
			Actor: in.Actor, Reason: in.Reason, At: now, ProposalID: p.ID, CommandID: in.CommandID,
		},
	})
	if err != nil {
		return MutationResult{}, conflict(err)
	}
	next.LedgerSeq, next.LedgerEntryID = e.Seq, e.ID
	if err := s.apply(ctx, next); err != nil {
		slog.ErrorContext(ctx, "ADR-0139 D13: approved skill could not be materialised", "skill", p.Name, "err", err)
	}
	res := MutationResult{Version: version, LedgerEntryID: e.ID}
	if err := s.grantProposer(ctx, p, in.Actor); err != nil {
		// The approval is committed; the command fails so the operator retries it
		// (same command_id), and the dedupe path above re-applies the grant.
		return res, err
	}
	if in.LockAfter && !next.Locked {
		if _, err := s.setLockLocked(ctx, in.CommandID+"#lock", p.Name, true, in.Actor, in.Reason); err != nil {
			return res, err
		}
	}
	return res, nil
}

// grantProposer grants an approved proposal's skill to its proposer
// (decisions/skill-access-grants.md, refinement b).
func (s *Service) grantProposer(ctx context.Context, p domain.SkillProposal, actor string) error {
	if s.grants == nil || p.ProposerAgentID == "" {
		return nil
	}
	if err := s.grants.GrantProposer(context.WithoutCancel(ctx), p.ProposerAgentID, p.Name, actor, p.ID); err != nil {
		slog.ErrorContext(ctx, "skill access: approved proposal not granted to its proposer; retry the decision",
			"skill", p.Name, "proposal_id", p.ID, "agent_id", p.ProposerAgentID, "err", err)
		return fmt.Errorf("skill lifecycle: grant %s to proposer %s: %w", p.Name, p.ProposerAgentID, err)
	}
	return nil
}

func conflict(err error) error {
	if errors.Is(err, domain.ErrSkillLedgerConflict) {
		return precondition("the skill changed concurrently; re-read and retry")
	}
	return err
}

// SetLock locks or unlocks a skill against agent edits. Locking an already
// locked skill (or unlocking an unlocked one) is a no-op without a ledger entry.
func (s *Service) SetLock(ctx context.Context, commandID, name string, locked bool, actor, reason string) (MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok, err := s.deduped(ctx, commandID); err != nil || ok {
		return r, err
	}
	return s.setLockLocked(ctx, commandID, name, locked, actor, reason)
}

func (s *Service) setLockLocked(ctx context.Context, commandID, name string, locked bool, actor, reason string) (MutationResult, error) {
	rec, ok, err := s.store.GetSkill(ctx, name)
	if err != nil {
		return MutationResult{}, err
	}
	if !ok {
		return MutationResult{}, fmt.Errorf("%w: skill %s", ErrNotFound, name)
	}
	if rec.Source == domain.SkillSourceBuiltin {
		return MutationResult{}, precondition("%s is a shipped skill: always locked against agent edits", name)
	}
	if rec.Locked == locked {
		return MutationResult{Version: rec.Version, LedgerEntryID: rec.LedgerEntryID}, nil
	}
	head, err := s.store.LedgerHead(ctx, name)
	if err != nil {
		return MutationResult{}, err
	}
	mutation := domain.SkillMutationUnlocked
	if locked {
		mutation = domain.SkillMutationLocked
	}
	next := rec
	next.Locked = locked
	e, err := s.store.Commit(ctx, domain.SkillCommit{
		ExpectSeq: head, Record: &next,
		Entry: domain.SkillLedgerEntry{
			SkillName: name, Mutation: mutation, FromVersion: rec.Version, ToVersion: rec.Version,
			FromState: rec.State, ToState: rec.State, Locked: locked,
			Actor: actor, Reason: reason, At: s.now().UTC(), CommandID: commandID,
		},
	})
	if err != nil {
		return MutationResult{}, conflict(err)
	}
	return MutationResult{Version: rec.Version, LedgerEntryID: e.ID}, nil
}

// Rollback restores a skill to the state recorded by one of its ledger entries
// (content, version, lifecycle state), as a NEW "rolled_back" entry.
// Operators may roll back a locked skill.
func (s *Service) Rollback(ctx context.Context, commandID, name, entryID, actor, reason string) (MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok, err := s.deduped(ctx, commandID); err != nil || ok {
		return r, err
	}
	target, ok, err := s.store.GetLedgerEntry(ctx, entryID)
	if err != nil {
		return MutationResult{}, err
	}
	if !ok || target.SkillName != name {
		return MutationResult{}, precondition("ledger entry %s is not an entry of %s", entryID, name)
	}
	if !s.restorable(ctx, target) {
		return MutationResult{}, precondition("ledger entry %s is not restorable", entryID)
	}
	rec, ok, err := s.store.GetSkill(ctx, name)
	if err != nil {
		return MutationResult{}, err
	}
	if !ok {
		return MutationResult{}, fmt.Errorf("%w: skill %s", ErrNotFound, name)
	}
	if rec.Source == domain.SkillSourceBuiltin {
		return MutationResult{}, precondition("%s is a shipped skill; restore it on disk", name)
	}
	head, err := s.store.LedgerHead(ctx, name)
	if err != nil {
		return MutationResult{}, err
	}
	now := s.now().UTC()
	next := rec
	// The lock is NOT part of what a rollback restores (operator.proto: content,
	// version and lifecycle state): rolling a locked skill back must not quietly
	// reopen it to agent edits.
	next.Version, next.CurrentSince = target.ToVersion, now
	if next.State != target.ToState {
		next.State, next.StateChangedAt = target.ToState, now
	}
	e, err := s.store.Commit(ctx, domain.SkillCommit{
		ExpectSeq: head, Record: &next,
		Entry: domain.SkillLedgerEntry{
			SkillName: name, Mutation: domain.SkillMutationRolledBack,
			FromVersion: rec.Version, ToVersion: next.Version, FromState: rec.State, ToState: next.State,
			Locked: next.Locked, Actor: actor, Reason: reason, At: now, RollbackOf: target.ID, CommandID: commandID,
		},
	})
	if err != nil {
		return MutationResult{}, conflict(err)
	}
	next.LedgerSeq, next.LedgerEntryID = e.Seq, e.ID
	if err := s.apply(ctx, next); err != nil {
		slog.ErrorContext(ctx, "ADR-0139 D13: rolled-back skill could not be materialised", "skill", name, "err", err)
	}
	return MutationResult{Version: next.Version, LedgerEntryID: e.ID}, nil
}

// restorable: the entry left a stored, agent-skill version in a known state.
// Rejections changed nothing, and builtin versions live on disk.
func (s *Service) restorable(ctx context.Context, e domain.SkillLedgerEntry) bool {
	if e.Mutation == domain.SkillMutationRejected || e.ToVersion == "" || e.ToState == "" {
		return false
	}
	v, ok, err := s.store.GetVersion(ctx, e.SkillName, e.ToVersion)
	return err == nil && ok && v.Source != domain.SkillSourceBuiltin
}

// ── reads ───────────────────────────────────────────────────────────────────

// Skill is one record for the operator plane, with the curator exemption.
type Skill struct {
	domain.SkillRecord
	ExemptReason string
	// SkillMD is the current version's content (agent-proposed skills that are
	// not in the catalog, i.e. archived ones, are listed from it), and Content its
	// parse (zero when it does not parse).
	SkillMD string
	Content domain.Skill
}

// Skills returns every lifecycle record, by name.
func (s *Service) Skills(ctx context.Context) ([]Skill, error) {
	recs, err := s.store.ListSkills(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })
	out := make([]Skill, 0, len(recs))
	for _, r := range recs {
		sk := Skill{SkillRecord: r, ExemptReason: s.ExemptReason(r.Name)}
		if r.Source == domain.SkillSourceBuiltin {
			sk.Locked = true // shipped skills are never agent-editable
		}
		if v, ok, err := s.store.GetVersion(ctx, r.Name, r.Version); err == nil && ok {
			sk.SkillMD = v.SkillMD
			if parsed, perr := discovery.ParseSkillMD([]byte(v.SkillMD)); perr == nil {
				sk.Content = parsed
			}
		}
		out = append(out, sk)
	}
	return out, nil
}

// ExemptReason is why the curator will not move name: "pinned", "pipeline",
// "schedule", or "".
func (s *Service) ExemptReason(name string) string {
	for _, p := range s.cfg.Curator.Pinned {
		if p == name {
			return "pinned"
		}
	}
	if s.refs != nil {
		return s.refs(name)
	}
	return ""
}

// Proposal is a proposal plus the skill's current version.
type Proposal struct {
	domain.SkillProposal
	CurrentVersion string
}

// Proposals lists proposals (state "" = pending).
func (s *Service) Proposals(ctx context.Context, f domain.SkillProposalFilter) ([]Proposal, int, error) {
	if f.State == "" {
		f.State = domain.SkillProposalPending
	}
	ps, total, err := s.store.ListProposals(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	out := make([]Proposal, 0, len(ps))
	for _, p := range ps {
		cur := ""
		if r, ok, err := s.store.GetSkill(ctx, p.Name); err == nil && ok {
			cur = r.Version
		}
		out = append(out, Proposal{SkillProposal: p, CurrentVersion: cur})
	}
	return out, total, nil
}

// LedgerEntry is an entry plus whether RollbackSkill accepts it.
type LedgerEntry struct {
	domain.SkillLedgerEntry
	Restorable bool
}

// Ledger lists ledger entries, newest first.
func (s *Service) Ledger(ctx context.Context, skillName string, limit, offset int) ([]LedgerEntry, int, error) {
	es, total, err := s.store.ListLedger(ctx, skillName, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	out := make([]LedgerEntry, 0, len(es))
	for _, e := range es {
		out = append(out, LedgerEntry{SkillLedgerEntry: e, Restorable: s.restorable(ctx, e)})
	}
	return out, total, nil
}
