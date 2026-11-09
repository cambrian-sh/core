package memorynotes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
)

// Config is the service's tunables (execution.memory_notes, internal/config).
type Config struct {
	// PersonalBudgetChars / OrgBudgetChars are the scope default budgets in Unicode
	// code points, used when neither the document nor the scope has an operator
	// budget row. Must be > 0.
	PersonalBudgetChars int
	OrgBudgetChars      int
	// OrgID is the deployment's org scope owner (ADR-0139 D9 org profile). Empty =
	// no org scope: a personal-profile deployment has none, so org notes are never
	// served to a run and agent org writes are SCOPE_DENIED.
	OrgID string
	// AgentOrgWrite lets an AGENT write the org document. Off by default: org notes
	// are shared by every member, and one member's agent rewriting them is an
	// operator decision, not an agent's.
	AgentOrgWrite bool
	// SnapshotTTL bounds how long a run's frozen snapshot is kept; a run that lasts
	// longer re-freezes on its next read. MaxSnapshots caps the cache (oldest out).
	SnapshotTTL  time.Duration
	MaxSnapshots int
}

func (c Config) withDefaults() Config {
	if c.SnapshotTTL <= 0 {
		c.SnapshotTTL = 24 * time.Hour
	}
	if c.MaxSnapshots <= 0 {
		c.MaxSnapshots = 4096
	}
	return c
}

// Scanner is the injection scan every write passes through (ADR-0139 D14: "scanned
// for injection on write"). The composition root binds the shared deterministic
// content scanner (internal/contentscan, also used by D13 skills); this narrow
// port keeps the scanner's rule set out of this package.
type Scanner interface {
	// ScanNote reports whether text is flagged and, if so, a human-readable detail
	// naming what matched.
	ScanNote(text string) (flagged bool, detail string)
}

// ScannerFunc adapts a function to Scanner.
type ScannerFunc func(text string) (bool, string)

// ScanNote implements Scanner.
func (f ScannerFunc) ScanNote(text string) (bool, string) { return f(text) }

// Principal is WHO a run is, as the kernel resolved it from the lease — never
// from a request field (INV-5).
type Principal struct {
	// ID is the run principal (the "sub"): whose personal notes the run reads and
	// writes. Empty = none could be established; the run then gets no personal
	// document and a personal write is SCOPE_DENIED (fail-closed).
	ID string
	// AgentID is the acting agent ("act"), recorded as the writer of record.
	AgentID string
	// Session correlates authorization decisions with the run.
	Session domain.SessionID
	// ActedBy is the operator who opened the run's conversation turn on behalf of
	// ID (operator act-for), "" otherwise. AUDIT ONLY: it is named in the writer of
	// record and never selects whose notes are read or written, nor the scope the
	// write is authorized under — that is ID's.
	ActedBy string
}

func (p Principal) ref() domain.PrincipalRef {
	return domain.PrincipalRef{ID: p.ID, Kind: domain.PrincipalUser}
}

// writerOfRecord renders "agent:<id> for <principal>" for UpdatedBy.
func (p Principal) writerOfRecord() string {
	switch {
	case p.AgentID != "" && p.ID != "" && p.ActedBy != "" && p.ActedBy != p.ID:
		// Act-for: the subject is the owner, the human behind the turn is named too.
		return "agent:" + p.AgentID + " for " + p.ID + " (on behalf, by operator:" + p.ActedBy + ")"
	case p.AgentID != "" && p.ID != "":
		return "agent:" + p.AgentID + " for " + p.ID
	case p.AgentID != "":
		return "agent:" + p.AgentID
	}
	return p.ID
}

// Service is the one place notes are read and written. Every surface — the agent
// plane, the operator plane, any future tool — goes through it, so the budget, the
// scan and the authorization cannot be skipped by a second pathway (AGENTS.md §4.3).
type Service struct {
	store Store
	scan  Scanner
	authz domain.Authorizer
	cfg   Config
	now   func() time.Time

	mu    sync.Mutex
	snaps map[string]*snapEntry
}

// New builds the service. store, scan and authz are REQUIRED constructor
// arguments (AGENTS.md §2.3): a service that could be built without its scan or
// its decision point would one day be.
func New(store Store, scan Scanner, authz domain.Authorizer, cfg Config) (*Service, error) {
	if store == nil || scan == nil || authz == nil {
		return nil, errors.New("memorynotes.New: store, scanner and authorizer are required")
	}
	if cfg.PersonalBudgetChars <= 0 || cfg.OrgBudgetChars <= 0 {
		return nil, fmt.Errorf("memorynotes.New: default budgets must be > 0 (personal %d, org %d)",
			cfg.PersonalBudgetChars, cfg.OrgBudgetChars)
	}
	return &Service{store: store, scan: scan, authz: authz, cfg: cfg.withDefaults(),
		now: time.Now, snaps: make(map[string]*snapEntry)}, nil
}

// OrgID is the configured org scope owner ("" = none).
func (s *Service) OrgID() string { return s.cfg.OrgID }

// ── budgets ──────────────────────────────────────────────────────────────────

func (s *Service) scopeDefault(scope Scope) int {
	if scope == ScopeOrg {
		return s.cfg.OrgBudgetChars
	}
	return s.cfg.PersonalBudgetChars
}

// budgetFor resolves a document's effective budget: its own row, else the scope's
// operator default row, else the configured default. overridden reports the first.
func (s *Service) budgetFor(ctx context.Context, key Key) (budget int, overridden bool, err error) {
	if b, ok, err := s.store.GetBudget(ctx, key.Scope, key.OwnerID); err != nil {
		return 0, false, err
	} else if ok {
		return b, true, nil
	}
	if b, ok, err := s.store.GetBudget(ctx, key.Scope, ""); err != nil {
		return 0, false, err
	} else if ok {
		return b, false, nil
	}
	return s.scopeDefault(key.Scope), false, nil
}

// ── authorization (C8) ───────────────────────────────────────────────────────

// resourceFor is the decision-point view of a document. A PERSONAL document is
// owner-scoped: it carries owner:<owner> (ADR-0142 D12 — "anything an agent
// writes while acting in a person's conversation carries owner:<person>. This
// covers memory notes"), so the owner fence makes it readable by that person
// alone, whoever else holds a broad policy — the operator included
// (T-20261107-a-person-s-assistant-cannot-save-that-person-s). The premium
// decision point checks a person's OWN owner-scoped resource without their
// floor's positive classification terms (authz.ownScopedCheck), which is what
// lets a person with a narrow read floor keep their own notes.
func resourceFor(key Key) (domain.ResourceRef, []string) {
	tags := classificationTags(key)
	if key.Scope == ScopePersonal && key.OwnerID != "" {
		tags = append(tags, domain.OwnerTag(key.OwnerID))
	}
	return domain.ResourceRef{Kind: domain.KindMemory, ID: "memory-notes:" + key.String()}, tags
}

// classificationTags are a document's tags without the owner fence.
func classificationTags(key Key) []string {
	return []string{"memory-notes", "memory-notes:" + string(key.Scope)}
}

func (s *Service) allowed(ctx context.Context, who domain.PrincipalRef, surface domain.SurfaceRef,
	session domain.SessionID, key Key, write bool) bool {
	res, tags := resourceFor(key)
	return s.decide(ctx, who, surface, session, res, tags, write)
}

// adminAllowed authorizes an operator-plane ADMINISTRATIVE mutation (Set, Delete,
// SetBudget) on a document by its classification alone, without the owner fence.
// These are audited operator acts on the deployment's notes (moderation, erasure,
// budgets), not reads of a person's material: reading another person's notes
// (List, Get) stays owner-fenced.
func (s *Service) adminAllowed(ctx context.Context, who domain.PrincipalRef, key Key) bool {
	res, _ := resourceFor(key)
	return s.decide(ctx, who, operatorSurface, "", res, classificationTags(key), true)
}

func (s *Service) decide(ctx context.Context, who domain.PrincipalRef, surface domain.SurfaceRef,
	session domain.SessionID, res domain.ResourceRef, tags []string, write bool) bool {
	req := domain.AccessRequest{Principal: who, Surface: surface, Resource: res, Tags: tags, Session: session}
	if write {
		req.Effects = []domain.ToolEffect{domain.EffectWrite}
	}
	return s.authz.Authorize(ctx, req).Allowed
}

// runReadable lists the keys a run principal may read: its own personal document
// and the configured org's, each also passing the decision point.
func (s *Service) runReadable(ctx context.Context, p Principal) []Key {
	var keys []Key
	surface := domain.SurfaceRef{Kind: domain.SurfaceAgent}
	if p.ID != "" {
		k := Key{Scope: ScopePersonal, OwnerID: p.ID}
		if s.allowed(ctx, p.ref(), surface, p.Session, k, false) {
			keys = append(keys, k)
		}
	}
	if s.cfg.OrgID != "" {
		k := Key{Scope: ScopeOrg, OwnerID: s.cfg.OrgID}
		if s.allowed(ctx, p.ref(), surface, p.Session, k, false) {
			keys = append(keys, k)
		}
	}
	return keys
}

// runWritable resolves the key an agent write targets, or "" detail when denied.
func (s *Service) runWritable(ctx context.Context, p Principal, scope Scope) (Key, string) {
	var k Key
	switch scope {
	case ScopePersonal:
		if p.ID == "" {
			return Key{}, "no run principal could be established for this lease"
		}
		k = Key{Scope: ScopePersonal, OwnerID: p.ID}
	case ScopeOrg:
		if s.cfg.OrgID == "" {
			return Key{}, "this deployment has no org scope"
		}
		if !s.cfg.AgentOrgWrite {
			return Key{}, "agents may not write org notes (execution.memory_notes.agent_org_write is off)"
		}
		k = Key{Scope: ScopeOrg, OwnerID: s.cfg.OrgID}
	default:
		return Key{}, "unknown scope"
	}
	if !s.allowed(ctx, p.ref(), domain.SurfaceRef{Kind: domain.SurfaceAgent}, p.Session, k, true) {
		return Key{}, "the access policy denies this write"
	}
	return k, ""
}

// ── the frozen run snapshot (D3) ─────────────────────────────────────────────

// SnapshotDoc is one document as a run sees it.
type SnapshotDoc struct {
	Key
	Content   string
	Size      int
	Budget    int
	ETag      string
	Truncated bool
	UpdatedAt time.Time
}

// Snapshot is the run-start view of every document the run may read.
type Snapshot struct {
	Documents []SnapshotDoc
	Version   string
	At        time.Time
}

type snapEntry struct {
	once    sync.Once
	snap    Snapshot
	err     error
	expires time.Time
	created time.Time
}

// Snapshot returns the run's FROZEN snapshot. The first call for runKey builds it;
// every later call within SnapshotTTL returns the same value (same Version) — even
// after a write in that run, which lands durably and shows from the next run.
// runKey identifies the run (the kernel's run id, or the lease for a lease-scoped
// turn); it is joined with the principal so two principals can never share one.
func (s *Service) Snapshot(ctx context.Context, runKey string, p Principal) (Snapshot, error) {
	if runKey == "" {
		return Snapshot{}, fmt.Errorf("%w: run key is required", ErrInvalid)
	}
	cacheKey := runKey + "\x00" + p.ID
	now := s.now()
	s.mu.Lock()
	e, ok := s.snaps[cacheKey]
	if ok && now.After(e.expires) {
		delete(s.snaps, cacheKey)
		ok = false
	}
	if !ok {
		s.evictLocked(now)
		e = &snapEntry{expires: now.Add(s.cfg.SnapshotTTL), created: now}
		s.snaps[cacheKey] = e
	}
	s.mu.Unlock()

	e.once.Do(func() { e.snap, e.err = s.buildSnapshot(ctx, p, now) })
	if e.err != nil {
		// A failed build is not frozen: the next call retries.
		s.mu.Lock()
		if s.snaps[cacheKey] == e {
			delete(s.snaps, cacheKey)
		}
		s.mu.Unlock()
		return Snapshot{}, e.err
	}
	return e.snap, nil
}

// evictLocked drops expired entries, then the oldest while over the cap.
func (s *Service) evictLocked(now time.Time) {
	for k, e := range s.snaps {
		if now.After(e.expires) {
			delete(s.snaps, k)
		}
	}
	for len(s.snaps) >= s.cfg.MaxSnapshots {
		var oldestKey string
		var oldest time.Time
		for k, e := range s.snaps {
			if oldestKey == "" || e.created.Before(oldest) {
				oldestKey, oldest = k, e.created
			}
		}
		delete(s.snaps, oldestKey)
	}
}

func (s *Service) buildSnapshot(ctx context.Context, p Principal, at time.Time) (Snapshot, error) {
	snap := Snapshot{At: at.UTC()}
	h := sha256.New()
	for _, k := range s.runReadable(ctx, p) {
		d, err := s.store.Get(ctx, k)
		if err != nil {
			return Snapshot{}, fmt.Errorf("memory notes snapshot: get %s: %w", k, err)
		}
		if !d.Exists() || d.Content == "" {
			continue // a scope with no notes is omitted
		}
		budget, _, err := s.budgetFor(ctx, k)
		if err != nil {
			return Snapshot{}, fmt.Errorf("memory notes snapshot: budget %s: %w", k, err)
		}
		sd := SnapshotDoc{Key: k, Content: d.Content, Size: d.Size(), Budget: budget,
			ETag: d.ETag(), UpdatedAt: d.UpdatedAt}
		if sd.Size > budget {
			// The operator lowered the budget below the stored size: serve it cut,
			// and SAY so. Nothing stored changes.
			sd.Content = truncateRunes(d.Content, budget)
			sd.Truncated = true
		}
		snap.Documents = append(snap.Documents, sd)
		fmt.Fprintf(h, "%s|%s|%d|%t\n", k, sd.ETag, budget, sd.Truncated)
	}
	sum := h.Sum(nil)
	snap.Version = hex.EncodeToString(sum[:8])
	return snap, nil
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// ── writes ───────────────────────────────────────────────────────────────────

// WriteResult is a write's outcome. A refusal is Accepted=false + Rejection; the
// document state fields then describe the document as it still is.
type WriteResult struct {
	Accepted  bool
	Rejection Rejection
	Detail    string
	ETag      string
	Size      int
	Budget    int
	// OverriddenFlag is the scan detail an operator override stored past (Set with
	// override, content flagged). "" when the scan did not flag or nothing was stored.
	OverriddenFlag string
}

// Proposal is an agent-plane write.
type Proposal struct {
	Scope   Scope
	Op      Op
	Section string
	Content string
	IfMatch string
	Reason  string
}

const casRetries = 3

// Propose applies an agent's edit to its run principal's notes. Durable at once;
// visible from the next run's snapshot.
func (s *Service) Propose(ctx context.Context, p Principal, req Proposal) (WriteResult, error) {
	if req.Scope == "" {
		req.Scope = ScopePersonal
	}
	switch req.Op {
	case OpAppend:
		if strings.TrimSpace(req.Content) == "" {
			return WriteResult{}, fmt.Errorf("%w: append needs content", ErrInvalid)
		}
	case OpReplaceSection:
		if req.Section == "" {
			return WriteResult{}, fmt.Errorf("%w: replace_section needs a section", ErrInvalid)
		}
	case OpRemoveSection:
		if req.Section == "" {
			return WriteResult{}, fmt.Errorf("%w: remove_section needs a section", ErrInvalid)
		}
	default:
		return WriteResult{}, fmt.Errorf("%w: op is required", ErrInvalid)
	}
	if strings.ContainsAny(req.Section, "\r\n") {
		return WriteResult{}, fmt.Errorf("%w: a section name is one line", ErrInvalid)
	}
	key, denied := s.runWritable(ctx, p, req.Scope)
	if denied != "" {
		return WriteResult{Rejection: RejectScopeDenied, Detail: denied}, nil
	}
	if req.Op != OpRemoveSection {
		if flagged, detail := s.scan.ScanNote(req.Section + "\n" + req.Content); flagged {
			return s.refused(ctx, key, RejectInjection, detail)
		}
	}
	meta := WriteMeta{Actor: p.writerOfRecord(), Reason: req.Reason, Op: string(req.Op)}
	return s.casWrite(ctx, key, req.IfMatch, meta, func(cur string) (string, Rejection, string) {
		next, ok := applyOp(cur, req.Op, req.Section, req.Content)
		if !ok {
			return "", RejectSectionNotFound, fmt.Sprintf("no section %q", req.Section)
		}
		return next, RejectNone, ""
	})
}

// refused reports a refusal with the document's current state.
func (s *Service) refused(ctx context.Context, key Key, r Rejection, detail string) (WriteResult, error) {
	res := WriteResult{Rejection: r, Detail: detail}
	d, err := s.store.Get(ctx, key)
	if err != nil {
		return WriteResult{}, fmt.Errorf("memory notes: get %s: %w", key, err)
	}
	budget, _, err := s.budgetFor(ctx, key)
	if err != nil {
		return WriteResult{}, fmt.Errorf("memory notes: budget %s: %w", key, err)
	}
	res.ETag, res.Size, res.Budget = d.ETag(), d.Size(), budget
	if !d.Exists() {
		res.Size = 0
	}
	return res, nil
}

// casWrite is the shared read-modify-write: if_match check, edit, budget check and
// a compare-and-swap Put. A lost race with no if_match re-reads and re-applies; a
// lost race WITH if_match is a conflict (the caller wrote against a version that
// is no longer current).
func (s *Service) casWrite(ctx context.Context, key Key, ifMatch string, meta WriteMeta,
	edit func(cur string) (string, Rejection, string)) (WriteResult, error) {
	budget, _, err := s.budgetFor(ctx, key)
	if err != nil {
		return WriteResult{}, fmt.Errorf("memory notes: budget %s: %w", key, err)
	}
	for attempt := 0; ; attempt++ {
		cur, err := s.store.Get(ctx, key)
		if err != nil {
			return WriteResult{}, fmt.Errorf("memory notes: get %s: %w", key, err)
		}
		curContent := ""
		if cur.Exists() {
			curContent = cur.Content
		}
		state := WriteResult{ETag: cur.ETag(), Size: utf8.RuneCountInString(curContent), Budget: budget}
		if ifMatch != "" && ifMatch != cur.ETag() {
			state.Rejection, state.Detail = RejectConflict, "if_match does not match the current etag"
			return state, nil
		}
		next, rej, detail := edit(curContent)
		if rej != RejectNone {
			state.Rejection, state.Detail = rej, detail
			return state, nil
		}
		if n := utf8.RuneCountInString(next); n > budget {
			// Refused, never trimmed; nothing is evicted to make room.
			state.Rejection = RejectOverBudget
			state.Detail = fmt.Sprintf("the write would make the document %d chars; the budget is %d", n, budget)
			return state, nil
		}
		meta.At = s.now().UTC()
		d, err := s.store.Put(ctx, key, next, cur.Revision, meta)
		if errors.Is(err, ErrRevisionConflict) {
			if ifMatch != "" || attempt+1 >= casRetries {
				state.Rejection, state.Detail = RejectConflict, "the document changed during the write"
				return state, nil
			}
			continue
		}
		if err != nil {
			return WriteResult{}, fmt.Errorf("memory notes: put %s: %w", key, err)
		}
		return WriteResult{Accepted: true, ETag: d.ETag(), Size: d.Size(), Budget: budget}, nil
	}
}

// ── operator surface ─────────────────────────────────────────────────────────

// Caller is an operator-plane principal.
type Caller struct {
	ID string
}

func (c Caller) ref() domain.PrincipalRef { return domain.UserPrincipal(c.ID) }

var operatorSurface = domain.SurfaceRef{Kind: domain.SurfaceOperator}

// Summary is one document without its body.
type Summary struct {
	Key
	Size       int
	Budget     int
	ETag       string
	UpdatedAt  time.Time
	UpdatedBy  string
	OverBudget bool
}

// Full is one whole document for the operator.
type Full struct {
	Summary
	Content          string
	BudgetOverridden bool
}

// List returns the documents the caller may read, ordered by (scope, owner).
func (s *Service) List(ctx context.Context, c Caller, f ListFilter) ([]Summary, error) {
	docs, err := s.store.List(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("memory notes: list: %w", err)
	}
	out := make([]Summary, 0, len(docs))
	for _, d := range docs {
		if !s.allowed(ctx, c.ref(), operatorSurface, "", d.Key, false) {
			continue
		}
		budget, _, err := s.budgetFor(ctx, d.Key)
		if err != nil {
			return nil, fmt.Errorf("memory notes: budget %s: %w", d.Key, err)
		}
		out = append(out, summaryOf(d, budget))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].OwnerID < out[j].OwnerID
	})
	return out, nil
}

func summaryOf(d Document, budget int) Summary {
	return Summary{Key: d.Key, Size: d.Size(), Budget: budget, ETag: d.ETag(),
		UpdatedAt: d.UpdatedAt, UpdatedBy: d.UpdatedBy, OverBudget: d.Size() > budget}
}

// Get returns one document. Unknown and unreadable are both ErrNotFound.
func (s *Service) Get(ctx context.Context, c Caller, key Key) (Full, error) {
	if err := key.validate(); err != nil {
		return Full{}, err
	}
	if !s.allowed(ctx, c.ref(), operatorSurface, "", key, false) {
		return Full{}, ErrNotFound
	}
	d, err := s.store.Get(ctx, key)
	if err != nil {
		return Full{}, fmt.Errorf("memory notes: get %s: %w", key, err)
	}
	if !d.Exists() {
		return Full{}, ErrNotFound
	}
	budget, overridden, err := s.budgetFor(ctx, key)
	if err != nil {
		return Full{}, fmt.Errorf("memory notes: budget %s: %w", key, err)
	}
	return Full{Summary: summaryOf(d, budget), Content: d.Content, BudgetOverridden: overridden}, nil
}

// Set replaces a whole document for an operator. Same budget and scan as an agent
// write; override lifts ONLY the scan (operator judgement, audited by the caller
// with the reason), never the budget.
func (s *Service) Set(ctx context.Context, c Caller, key Key, content, ifMatch string, override bool, reason string) (WriteResult, error) {
	if err := key.validate(); err != nil {
		return WriteResult{}, err
	}
	if !s.adminAllowed(ctx, c.ref(), key) {
		return WriteResult{Rejection: RejectScopeDenied, Detail: "the access policy denies this write"}, nil
	}
	flagged, detail := s.scan.ScanNote(content)
	if flagged && !override {
		return s.refused(ctx, key, RejectInjection, detail)
	}
	op := "set"
	if flagged {
		op = "set_override_injection"
	}
	res, err := s.casWrite(ctx, key, ifMatch, WriteMeta{Actor: "operator:" + c.ID, Reason: reason, Op: op},
		func(string) (string, Rejection, string) { return content, RejectNone, "" })
	if err == nil && res.Accepted && flagged {
		res.OverriddenFlag = detail
	}
	return res, err
}

// Delete clears a document and returns the cleared one (for the audit record).
// ErrNotFound when it does not exist or the caller may not reach it.
func (s *Service) Delete(ctx context.Context, c Caller, key Key, reason string) (Document, error) {
	if err := key.validate(); err != nil {
		return Document{}, err
	}
	if !s.adminAllowed(ctx, c.ref(), key) {
		return Document{}, ErrNotFound
	}
	for attempt := 0; ; attempt++ {
		cur, err := s.store.Get(ctx, key)
		if err != nil {
			return Document{}, fmt.Errorf("memory notes: get %s: %w", key, err)
		}
		if !cur.Exists() {
			return Document{}, ErrNotFound
		}
		_, err = s.store.Delete(ctx, key, cur.Revision,
			WriteMeta{Actor: "operator:" + c.ID, Reason: reason, Op: "delete", At: s.now().UTC()})
		if errors.Is(err, ErrRevisionConflict) && attempt+1 < casRetries {
			continue
		}
		if err != nil {
			return Document{}, fmt.Errorf("memory notes: delete %s: %w", key, err)
		}
		return cur, nil
	}
}

// SetBudget sets (or clears) a document's budget, or the scope default when owner
// is "". It never touches stored content.
func (s *Service) SetBudget(ctx context.Context, c Caller, scope Scope, owner string, budget int, clear bool) error {
	if scope != ScopePersonal && scope != ScopeOrg {
		return fmt.Errorf("%w: scope is required", ErrInvalid)
	}
	key := Key{Scope: scope, OwnerID: owner}
	if owner != "" && !s.adminAllowed(ctx, c.ref(), key) {
		return ErrNotFound
	}
	if clear {
		return s.store.ClearBudget(ctx, scope, owner)
	}
	if budget <= 0 {
		return fmt.Errorf("%w: budget_chars must be > 0", ErrInvalid)
	}
	return s.store.SetBudget(ctx, scope, owner, budget)
}
