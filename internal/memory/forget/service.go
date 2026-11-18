// Package forget is the two-phase principal-initiated forget of ADR-0137 D2 (ENG-411
// stage C): Preview resolves a selector to the concrete records the caller can see and
// hands back a deterministic token; Forget executes exactly those records (or explicit
// ids) through the ONE erasure chokepoint, MemoryStack.Eraser() (authz.EnforcingEraser).
//
// It is the single service behind every forget door — the agent-plane PreviewForget /
// Forget / ListMyErasures RPCs, the operator-plane PreviewForgetMemory / ForgetMemory /
// ListErasures / GetErasure RPCs and the forget_memory kernel tool — so the two-phase rule
// and the disabled answer are the same everywhere (AGENTS.md §4.3).
//
// It decides nothing about who may erase what: the caller's principal and read
// predicate ride ctx (AGENTS.md §2.4) and the enforcing eraser applies them, at preview
// (hidden = absent) and again at execution (a preview is never a capability). The kernel
// never asks a model which rows to delete. A natural-language forget is either the
// agent's own search followed by a preview of the ids it found, or — the query form,
// PreviewQuery (ENG-1948, owner decision 2026-11-17) — the same search run here with
// the caller's own read scope, whose hits are previewed as concrete ids. Either way the
// preview names concrete records and nothing is erased until Forget runs with its token.
package forget

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memorynotes"
)

// Config is the forget service's configuration (execution.erasure).
type Config struct {
	// PreviewTTL is how long a preview token stays executable (preview_ttl_seconds).
	PreviewTTL time.Duration
	// PolicyFingerprint identifies the erasure policy in force; it is part of every
	// token, so a policy change invalidates outstanding previews.
	PolicyFingerprint string
	// MaxPreviews bounds the outstanding previews held in memory; 0 ⇒ 4096. When full,
	// expired previews go first, then the soonest to expire.
	MaxPreviews int
	// Now is the clock for preview expiry only (never an identifier); nil ⇒ time.Now.
	Now func() time.Time
	// Searcher resolves PreviewQuery's free text: the caller's own memory search, the
	// pass QueryMemory runs (memory.QueryService.SearchMatches). nil ⇒ the query form
	// answers ErrQueryUnavailable; the selector form is unaffected.
	Searcher Searcher
	// SnippetChars bounds each query-preview snippet in characters
	// (execution.erasure.preview_snippet_chars); 0 ⇒ 200.
	SnippetChars int
	// MinCosine is the query preview's relevance floor (ENG-1953,
	// execution.erasure.preview_min_cosine): a hit names a record only when its
	// cosine similarity to the query (SearchResult.RawScore — the scale of the
	// retrieval floors recall_similarity_floor and workspace_min_fact_cosine, NOT the
	// fused/blended Score) is at least this. A hit with no cosine (RawScore <= 0:
	// lexical-only, associatively injected) never qualifies. 0 ⇒ DefaultMinCosine.
	MinCosine float64
	// MaxCandidates caps a query preview's candidate list after deduplication, in
	// rank order (execution.erasure.preview_max_candidates); a cut sets
	// Preview.Truncated. 0 ⇒ DefaultMaxCandidates.
	MaxCandidates int
	// Notes is the principal memory-notes service (ADR-0139 D14, ENG-2106): the query
	// form also matches the run principal's own personal notes (the store a chat agent
	// puts "remember this for me" facts in), and an executed erase of note entries
	// drops the cached run snapshots of their owner. nil ⇒ notes are not searched by
	// the query form (ids naming note entries still resolve through the chokepoint).
	Notes Notes
}

// Notes is what the forget service needs of the memory-notes service
// (memorynotes.Service): the entries of a person's personal notes that a free-text
// forget names, and the snapshot cache to invalidate after an erase.
type Notes interface {
	MatchForErase(ctx context.Context, owner, query string) ([]memorynotes.EntryMatch, error)
	DropSnapshots(owner string)
}

// The query preview's defaults (ENG-1953; execution.erasure documents the same).
const (
	// DefaultMinCosine is workspace_min_fact_cosine's value: the kernel's existing
	// "this memory is about the query" cosine threshold for the default embedder.
	DefaultMinCosine = 0.60
	// DefaultMaxCandidates bounds what an agent has to check before confirming.
	DefaultMaxCandidates = 20
)

// Searcher is the slice of domain.MemorySearcher the query form uses: the caller-scoped
// recall an agent's own QueryMemory runs. It honours the predicate on ctx exactly as
// QueryMemory does, so the query form can never see more than a search by the same
// caller would (AGENTS.md §2.4; §4.3 — no second retrieval pathway).
type Searcher interface {
	SearchMatches(ctx context.Context, query, callerID string) ([]domain.SearchResult, error)
}

// ErrQueryUnavailable: the query form was asked for but no searcher is composed.
var ErrQueryUnavailable = errors.New("forget: the query form is unavailable (no memory searcher)")

// ErrNoPrincipal: the query form needs the caller's principal on ctx. The chokepoint
// would refuse later; refusing before the search spends nothing.
var ErrNoPrincipal = errors.New("forget: no principal on the call")

// Preview is one resolved forget preview: content-free (ids and kinds only).
type Preview struct {
	// Token executes exactly Selector for the same caller until ExpiresAt.
	Token string
	// Candidates are the records the caller can see that the selector names.
	Candidates []domain.ErasureCandidate
	// Selector is the concrete selector Forget will execute for Token.
	Selector  domain.ErasureSelector
	ExpiresAt time.Time
	// Snippets (query previews only) holds, per candidate (SnippetKey), a clipped
	// excerpt of the caller's own text that matched. nil for selector previews, which
	// stay content-free.
	Snippets map[string]string
	// Truncated (query previews only) reports that more records qualified than
	// Config.MaxCandidates and the list was cut, in rank order; the caller should
	// narrow the query (or name ids) to reach the rest.
	Truncated bool
}

// SnippetKey is the Preview.Snippets key of a candidate.
func SnippetKey(c domain.ErasureCandidate) string { return c.Kind + ":" + c.ID }

// Request is one Forget: a preview token, or explicit ids — exactly one.
type Request struct {
	PreviewToken string
	Selector     domain.ErasureSelector
	Reason       domain.ErasureReason
}

type preview struct {
	principal string
	selector  domain.ErasureSelector
	expires   time.Time
}

// Service is the forget service. The zero value is not usable; build with New.
type Service struct {
	eraser domain.MemoryEraser
	cfg    Config

	mu       sync.Mutex
	previews map[string]preview
}

// New builds the service over the enforcing eraser (MemoryStack.Eraser()). A nil
// eraser is legal — erasure is off — and every method then answers
// domain.ErrErasureDisabled.
func New(eraser domain.MemoryEraser, cfg Config) *Service {
	if cfg.PreviewTTL <= 0 {
		cfg.PreviewTTL = 10 * time.Minute
	}
	if cfg.MaxPreviews <= 0 {
		cfg.MaxPreviews = 4096
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SnippetChars <= 0 {
		cfg.SnippetChars = 200
	}
	if cfg.MinCosine <= 0 {
		cfg.MinCosine = DefaultMinCosine
	}
	if cfg.MaxCandidates <= 0 {
		cfg.MaxCandidates = DefaultMaxCandidates
	}
	return &Service{eraser: eraser, cfg: cfg, previews: map[string]preview{}}
}

// Enabled reports whether an eraser is composed.
func (s *Service) Enabled() bool { return s != nil && s.eraser != nil }

func (s *Service) eraserOrDisabled() (domain.MemoryEraser, error) {
	if !s.Enabled() {
		return nil, domain.ErrErasureDisabled
	}
	return s.eraser, nil
}

// Preview resolves sel to the records the caller (principal + predicate on ctx) can
// see, and returns them with a token that executes exactly those records. Nothing
// visible → domain.ErrNothingToErase; no principal or predicate → the chokepoint's
// ErrScopeMissing.
func (s *Service) Preview(ctx context.Context, sel domain.ErasureSelector) (Preview, error) {
	e, err := s.eraserOrDisabled()
	if err != nil {
		return Preview{}, err
	}
	sel = sel.Normalized()
	if sel.IsEmpty() {
		return Preview{}, fmt.Errorf("%w: the selector names nothing", domain.ErrInvalidErasureRequest)
	}
	cands, err := e.Resolve(ctx, sel)
	if err != nil {
		return Preview{}, err
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Kind != cands[j].Kind {
			return cands[i].Kind < cands[j].Kind
		}
		return cands[i].ID < cands[j].ID
	})
	return s.issue(ctx, cands)
}

// PreviewQuery is the query form of Preview for an AGENT caller (ADR-0137 D2,
// ENG-1948, owner decision 2026-11-17). The free text is resolved by the caller's own
// memory search — Config.Searcher, the recall pass QueryMemory runs, under the
// principal and predicate already on ctx — never by a model deciding what to delete.
// Then:
//
//   - the relevance floor (ENG-1953) drops every hit whose cosine to the query
//     (RawScore) is below Config.MinCosine, or that has no cosine at all, BEFORE hits
//     are mapped to records: a document qualifies when any one of its hits passes;
//   - every remaining hit is named by its chunk id and resolved through the same chokepoint
//     Resolve as the selector form (hidden = absent), so a hit in a stored document
//     becomes that WHOLE DOCUMENT (ENG-1947); a chunk with no document stays a chunk;
//   - candidates the caller did not write are OMITTED (agent erase is writer-only
//     within read scope, ADR-0137 Q3): the preview lists only what the caller can
//     actually erase, so its snippets only ever quote the caller's own text;
//   - candidates are deduplicated (several hits in one document are one candidate)
//     and kept in rank order, each with a snippet of its best-ranked hit, clipped to
//     Config.SnippetChars;
//   - the list is capped at Config.MaxCandidates in that rank order (Truncated set
//     when the cap cut it).
//
// ENG-2106: with Config.Notes composed and a run principal on ctx (the person the
// kernel resolved from the presented lease), the lines of THAT person's personal memory
// notes that carry every query word (memorynotes.MatchEntries) are listed too, first,
// as memory_note candidates resolved through the same chokepoint, each with its line as
// the snippet; the cap counts them. Notes are where a chat agent saves "remember this
// for me", so a forget that skipped them left the fact behind.
//
// The token covers exactly the concrete ids listed; Forget re-authorizes them in full.
// No candidate → domain.ErrNothingToErase. callerID is the agent id the search runs as.
func (s *Service) PreviewQuery(ctx context.Context, query, callerID string) (Preview, error) {
	e, err := s.eraserOrDisabled()
	if err != nil {
		return Preview{}, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return Preview{}, fmt.Errorf("%w: the query is empty", domain.ErrInvalidErasureRequest)
	}
	if s.cfg.Searcher == nil && s.cfg.Notes == nil {
		return Preview{}, ErrQueryUnavailable
	}
	writer := domain.PrincipalFromContext(ctx).WriterID()
	if writer == "" {
		return Preview{}, ErrNoPrincipal
	}
	// ENG-2106: the run principal's own memory notes, matched by their words (notes are
	// never embedded; memorynotes.MatchEntries). Only with a run principal — the person
	// the kernel resolved from the presented lease — whose PERSONAL notes alone are
	// searched; the chokepoint still resolves and authorizes every id below.
	text := map[string]string{}
	var noteIDs []string
	if s.cfg.Notes != nil {
		if owner := domain.RunPrincipalFromContext(ctx); owner != "" {
			ms, err := s.cfg.Notes.MatchForErase(ctx, owner, query)
			if err != nil {
				return Preview{}, fmt.Errorf("forget preview: memory notes: %w", err)
			}
			for _, m := range ms {
				noteIDs = append(noteIDs, m.ID)
				text[m.ID] = m.Text
			}
		}
	}
	var hits []domain.SearchResult
	if s.cfg.Searcher != nil {
		if hits, err = s.cfg.Searcher.SearchMatches(ctx, query, callerID); err != nil {
			return Preview{}, fmt.Errorf("forget preview: search: %w", err)
		}
	}
	rank := map[string]int{}
	var ids []string
	for _, h := range hits {
		id := h.Document.ID
		if id == "" {
			continue
		}
		// The floor, on the hit, before any widening to a document. Over-matching is
		// the riskier failure for a deletion (owner decision 2026-11-17): a hit with no
		// cosine — lexical-only, or injected associatively (KG/entity seeds) — earned
		// its recall slot without resembling the query and does not qualify. A record
		// missed this way can still be previewed by id.
		if h.RawScore <= 0 || h.RawScore < s.cfg.MinCosine {
			continue
		}
		if _, seen := rank[id]; seen {
			continue
		}
		rank[id] = len(ids)
		text[id] = h.Document.Text
		ids = append(ids, id)
	}
	if len(ids) == 0 && len(noteIDs) == 0 {
		return Preview{}, &domain.NothingToEraseError{}
	}
	// Note entries first, in document order: the person's own explicit notes, matched on
	// every query word. Hidden = absent (the chokepoint's Resolve).
	var kept []domain.ErasureCandidate
	if len(noteIDs) > 0 {
		cands, err := e.Resolve(ctx, domain.ErasureSelector{MemoryNoteIDs: noteIDs})
		if err != nil {
			return Preview{}, err
		}
		order := map[string]int{}
		for i, id := range noteIDs {
			order[id] = i
		}
		sort.SliceStable(cands, func(i, j int) bool { return order[cands[i].ID] < order[cands[j].ID] })
		for _, c := range cands {
			if c.Kind == domain.ErasureTargetMemoryNote {
				kept = append(kept, c)
			}
		}
	}
	if len(ids) > 0 {
		cands, err := e.Resolve(ctx, domain.ErasureSelector{ChunkIDs: ids})
		if err != nil {
			return Preview{}, err
		}
		best := map[string]domain.ErasureCandidate{}
		for _, c := range cands {
			if c.WrittenBy == "" || c.WrittenBy != writer {
				continue // not the caller's to erase: omitted, never listed
			}
			r, ok := rank[c.Via]
			if !ok {
				continue // defensive: Via is always the hit chunk id that named it
			}
			k := SnippetKey(c)
			if prev, dup := best[k]; dup && rank[prev.Via] <= r {
				continue
			}
			best[k] = c
		}
		memory := make([]domain.ErasureCandidate, 0, len(best))
		for _, c := range best {
			memory = append(memory, c)
		}
		sort.SliceStable(memory, func(i, j int) bool { return rank[memory[i].Via] < rank[memory[j].Via] })
		kept = append(kept, memory...)
	}
	truncated := len(kept) > s.cfg.MaxCandidates
	if truncated {
		kept = kept[:s.cfg.MaxCandidates]
	}
	p, err := s.issue(ctx, kept)
	if err != nil {
		return Preview{}, err
	}
	p.Truncated = truncated
	p.Snippets = make(map[string]string, len(kept))
	for _, c := range kept {
		p.Snippets[SnippetKey(c)] = clip(text[c.Via], s.cfg.SnippetChars)
	}
	return p, nil
}

// clip collapses whitespace and cuts s to at most n characters, marking a cut with "…".
func clip(s string, n int) string {
	s = strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return strings.TrimRightFunc(string(r[:n-1]), unicode.IsSpace) + "…"
}

// issue mints the token over the candidates' concrete selector and remembers it.
func (s *Service) issue(ctx context.Context, cands []domain.ErasureCandidate) (Preview, error) {
	concrete := domain.SelectorOfCandidates(cands)
	if concrete.IsEmpty() {
		return Preview{}, &domain.NothingToEraseError{}
	}
	principal := domain.PrincipalFromContext(ctx).String()
	token := domain.ErasurePreviewToken(principal, concrete, s.cfg.PolicyFingerprint)
	expires := s.cfg.Now().Add(s.cfg.PreviewTTL)
	s.remember(token, preview{principal: principal, selector: concrete, expires: expires})
	return Preview{Token: token, Candidates: cands, Selector: concrete, ExpiresAt: expires}, nil
}

func (s *Service) remember(token string, p preview) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	if _, ok := s.previews[token]; !ok && len(s.previews) >= s.cfg.MaxPreviews {
		for t, old := range s.previews {
			if !now.Before(old.expires) {
				delete(s.previews, t)
			}
		}
		for len(s.previews) >= s.cfg.MaxPreviews {
			var soonest string
			for t, old := range s.previews {
				if soonest == "" || old.expires.Before(s.previews[soonest].expires) {
					soonest = t
				}
			}
			delete(s.previews, soonest)
		}
	}
	s.previews[token] = p
}

// lookup returns the preview of token for principal, or ErrPreviewUnknown (unknown,
// expired, or another caller's — indistinguishable on purpose).
func (s *Service) lookup(token, principal string) (domain.ErasureSelector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.previews[token]
	if ok && !s.cfg.Now().Before(p.expires) {
		delete(s.previews, token)
		ok = false
	}
	if !ok || p.principal != principal {
		return domain.ErasureSelector{}, domain.ErrPreviewUnknown
	}
	return p.selector, nil
}

// Forget executes one erasure: the records a preview token resolved (for the caller
// that previewed them, before it expired) or explicit ids. Re-authorized in full by the
// chokepoint. A retry of the same act returns the same receipt (Replayed), because the
// request id is deterministic (ADR-0137 D6) — the token stays valid until it expires so
// a retry after a lost answer works.
func (s *Service) Forget(ctx context.Context, req Request) (domain.ErasureReceipt, error) {
	e, err := s.eraserOrDisabled()
	if err != nil {
		return domain.ErasureReceipt{}, err
	}
	sel := req.Selector.Normalized()
	switch {
	case req.PreviewToken != "" && !sel.IsEmpty():
		return domain.ErasureReceipt{}, fmt.Errorf("%w: name a preview token or explicit ids, not both", domain.ErrInvalidErasureRequest)
	case req.PreviewToken != "":
		sel, err = s.lookup(req.PreviewToken, domain.PrincipalFromContext(ctx).String())
		if err != nil {
			return domain.ErasureReceipt{}, err
		}
	case sel.IsEmpty():
		return domain.ErasureReceipt{}, fmt.Errorf("%w: a preview token or explicit ids are required", domain.ErrInvalidErasureRequest)
	}
	reason := req.Reason
	if reason == "" {
		reason = domain.ErasureReasonUserRequest
	}
	// Principal and kind are overwritten from ctx by the chokepoint (never trusted from
	// a request); Validate needs them set to pass, so they are filled from ctx here.
	rec, err := e.Erase(ctx, domain.ErasureRequest{
		Principal:     domain.PrincipalFromContext(ctx).WriterID(),
		PrincipalKind: domain.ErasureByAgent,
		Reason:        reason,
		Selector:      sel,
	})
	if err != nil {
		return domain.ErasureReceipt{}, err
	}
	s.dropNoteSnapshots(sel.MemoryNoteIDs, rec)
	return rec, nil
}

// dropNoteSnapshots invalidates the cached run snapshots of every person whose notes an
// executed (not replayed) erase reached, so no run keeps serving the erased lines from
// its frozen copy (ENG-2106).
func (s *Service) dropNoteSnapshots(noteIDs []string, rec domain.ErasureReceipt) {
	if s.cfg.Notes == nil || rec.Replayed || len(noteIDs) == 0 {
		return
	}
	seen := map[string]bool{}
	for _, id := range noteIDs {
		if _, owner, _, ok := domain.ParseMemoryNoteEntryID(id); ok && !seen[owner] {
			seen[owner] = true
			s.cfg.Notes.DropSnapshots(owner)
		}
	}
}

// Receipt returns one receipt the caller may see (found=false otherwise).
func (s *Service) Receipt(ctx context.Context, requestID string) (domain.ErasureReceipt, bool, error) {
	e, err := s.eraserOrDisabled()
	if err != nil {
		return domain.ErasureReceipt{}, false, err
	}
	if requestID == "" {
		return domain.ErasureReceipt{}, false, fmt.Errorf("%w: request id is required", domain.ErrInvalidErasureRequest)
	}
	return e.Receipt(ctx, requestID)
}

// List lists the receipts the caller may see, newest first.
func (s *Service) List(ctx context.Context, f domain.ErasureReceiptFilter) ([]domain.ErasureReceipt, error) {
	e, err := s.eraserOrDisabled()
	if err != nil {
		return nil, err
	}
	return e.ListReceipts(ctx, f)
}

// IsCallerError reports whether err is the caller's fault (a malformed request or an
// unknown preview) rather than a decision or a store failure — the RPC boundary's
// InvalidArgument / FailedPrecondition split.
func IsCallerError(err error) bool {
	return errors.Is(err, domain.ErrInvalidErasureRequest) || errors.Is(err, domain.ErrPreviewUnknown)
}
